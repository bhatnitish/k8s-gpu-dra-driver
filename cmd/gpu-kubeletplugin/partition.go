/*
Copyright (c) Advanced Micro Devices, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the \"License\");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an \"AS IS\" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ROCm/k8s-gpu-dra-driver/pkg/amdsmi"
	"github.com/ROCm/k8s-gpu-dra-driver/pkg/consts"
	resourceapi "k8s.io/api/resource/v1"
	klog "k8s.io/klog/v2"
)

// PartitionState tracks the active partition state for all GPUs on the node.
// It manages compute mode per GPU and memory mode per node, and coordinates
// libamd_smi calls for GPU reconfiguration.
type PartitionState struct {
	mu sync.Mutex

	// activeMemoryMode is the currently locked memory partition mode for the node.
	// Empty string means unlocked (any mode can be requested).
	activeMemoryMode string

	// gpuComputeModes tracks the active compute partition mode per physical GPU.
	// Key is gpuIndex, value is the compute mode string (e.g., "cpx").
	gpuComputeModes map[int]string

	// gpuAllocCounts tracks the number of active allocations per physical GPU.
	gpuAllocCounts map[int]int

	// totalAllocCount is the total number of active partition allocations across all GPUs.
	totalAllocCount int

	// gpuPCIAddresses maps GPU index to PCI address (used by discoverPartitionDeviceNodes).
	gpuPCIAddresses map[int]string

	// partitionableGPUs is the list of GPU indices that support compute partitioning.
	// Used for building shared counter sets in the ResourceSlice.
	partitionableGPUs []int

	// allocatable is a reference to the allocatable devices map for taint updates.
	allocatable AllocatableDevices
}

// NewPartitionState creates a new PartitionState with the given GPU PCI addresses
// and the list of GPU indices that support partitioning.
func NewPartitionState(gpuPCIAddresses map[int]string, partitionableGPUs []int, allocatable AllocatableDevices) *PartitionState {
	return &PartitionState{
		gpuComputeModes:   make(map[int]string),
		gpuAllocCounts:    make(map[int]int),
		gpuPCIAddresses:   gpuPCIAddresses,
		partitionableGPUs: partitionableGPUs,
		allocatable:       allocatable,
	}
}

// PreparePartition handles the partition setup for a device allocation.
// It calls libamd_smi to set compute/memory modes if this is the first allocation
// on the GPU/node, and tracks allocation counts.
// Returns an error if the requested mode conflicts with an existing allocation.
func (ps *PartitionState) PreparePartition(deviceName string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	gpuIndex, computeMode, memoryMode, err := parseSyntheticPartitionDeviceName(deviceName)
	if err != nil {
		return fmt.Errorf("error parsing device name for partition prepare: %v", err)
	}

	// Check and set compute partition mode on this GPU
	if existingMode, ok := ps.gpuComputeModes[gpuIndex]; ok {
		if existingMode != computeMode {
			return fmt.Errorf("GPU %d is already in compute mode %q, cannot switch to %q while allocations exist",
				gpuIndex, existingMode, computeMode)
		}
		klog.Infof("GPU %d already in compute mode %q, skipping partition set", gpuIndex, computeMode)
	} else {
		// First allocation on this GPU - set compute partition mode
		currentMode := ps.readCurrentComputePartition(gpuIndex)
		if strings.EqualFold(currentMode, computeMode) {
			klog.Infof("GPU %d already in compute mode %q (via sysfs), skipping partition set", gpuIndex, computeMode)
		} else {
			klog.Infof("Setting compute partition mode %q on GPU %d (current: %q)", computeMode, gpuIndex, currentMode)
			if err := amdsmi.SetComputePartition(gpuIndex, computeMode); err != nil {
				klog.Warningf("AMD SMI failed to set compute partition on GPU %d: %v, trying sysfs fallback", gpuIndex, err)
				if sysfsErr := ps.writeComputePartitionViaSysfs(gpuIndex, computeMode); sysfsErr != nil {
					return fmt.Errorf("failed to set compute partition on GPU %d (amdsmi: %v, sysfs: %v)", gpuIndex, err, sysfsErr)
				}
			}
		}
		ps.gpuComputeModes[gpuIndex] = computeMode
	}

	// Check and set memory partition mode on the node
	if ps.activeMemoryMode != "" {
		if ps.activeMemoryMode != memoryMode {
			return fmt.Errorf("node is already in memory mode %q, cannot switch to %q while allocations exist",
				ps.activeMemoryMode, memoryMode)
		}
		klog.Infof("Node already in memory mode %q, skipping partition set", memoryMode)
	} else {
		// First allocation on the node - set memory partition mode on all partitionable GPUs
		klog.Infof("Setting memory partition mode %q on all partitionable GPUs via libamd_smi", memoryMode)
		for _, gpuIdx := range ps.partitionableGPUs {
			currentMemMode := ps.readCurrentMemoryPartition(gpuIdx)
			if strings.EqualFold(currentMemMode, memoryMode) {
				klog.Infof("GPU %d already in memory mode %q (via sysfs), skipping partition set", gpuIdx, memoryMode)
				continue
			}
			if err := amdsmi.SetMemoryPartition(gpuIdx, memoryMode); err != nil {
				klog.Warningf("AMD SMI failed to set memory partition on GPU %d: %v, trying sysfs fallback", gpuIdx, err)
				if sysfsErr := ps.writeMemoryPartitionViaSysfs(gpuIdx, memoryMode); sysfsErr != nil {
					return fmt.Errorf("failed to set memory partition on GPU %d (amdsmi: %v, sysfs: %v)", gpuIdx, err, sysfsErr)
				}
			}
		}
		ps.activeMemoryMode = memoryMode

		// Apply taints to devices with incompatible memory modes
		ps.applyMemoryTaints(memoryMode)
	}

	// Increment allocation counts
	ps.gpuAllocCounts[gpuIndex]++
	ps.totalAllocCount++

	klog.Infof("Partition prepare complete: GPU %d, compute=%s, memory=%s, gpuAllocs=%d, totalAllocs=%d",
		gpuIndex, computeMode, memoryMode, ps.gpuAllocCounts[gpuIndex], ps.totalAllocCount)

	return nil
}

// UnpreparePartition handles cleanup when a partition allocation is released.
// It decrements allocation counts and returns whether taints changed
// (indicating that ResourceSlice needs to be re-published).
func (ps *PartitionState) UnpreparePartition(deviceName string) (taintsChanged bool, err error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	gpuIndex, _, _, err := parseSyntheticPartitionDeviceName(deviceName)
	if err != nil {
		return false, fmt.Errorf("error parsing device name for partition unprepare: %v", err)
	}

	// Decrement allocation counts
	if ps.gpuAllocCounts[gpuIndex] > 0 {
		ps.gpuAllocCounts[gpuIndex]--
	}
	if ps.totalAllocCount > 0 {
		ps.totalAllocCount--
	}

	// If no more allocations on this GPU, clear its compute mode
	if ps.gpuAllocCounts[gpuIndex] == 0 {
		delete(ps.gpuComputeModes, gpuIndex)
		delete(ps.gpuAllocCounts, gpuIndex)
		klog.Infof("GPU %d has no more allocations, compute mode cleared", gpuIndex)
	}

	// If no more allocations on the entire node, clear memory mode and remove taints
	if ps.totalAllocCount == 0 {
		klog.Infof("No more partition allocations on node, clearing memory mode %q and removing taints",
			ps.activeMemoryMode)
		ps.activeMemoryMode = ""
		ps.removeMemoryTaints()
		taintsChanged = true
	}

	return taintsChanged, nil
}

// GetActiveMemoryMode returns the currently active memory partition mode.
func (ps *PartitionState) GetActiveMemoryMode() string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.activeMemoryMode
}

// GetGPUComputeModes returns a copy of the GPU compute modes map.
func (ps *PartitionState) GetGPUComputeModes() map[int]string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	result := make(map[int]string, len(ps.gpuComputeModes))
	for k, v := range ps.gpuComputeModes {
		result[k] = v
	}
	return result
}

// RecoverFromCheckpoint reconstructs partition state from checkpoint data.
// This is called on driver restart to recover the active memory mode and
// per-GPU compute modes from the persisted checkpoint.
func (ps *PartitionState) RecoverFromCheckpoint(activeMemoryMode string, gpuComputeModes map[int]string, preparedClaims PreparedClaims) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.activeMemoryMode = activeMemoryMode

	if gpuComputeModes != nil {
		ps.gpuComputeModes = make(map[int]string, len(gpuComputeModes))
		for k, v := range gpuComputeModes {
			ps.gpuComputeModes[k] = v
		}
	}

	// Reconstruct allocation counts from prepared claims
	ps.gpuAllocCounts = make(map[int]int)
	ps.totalAllocCount = 0

	for _, devices := range preparedClaims {
		for _, device := range devices {
			gpuIndex, _, _, err := parseSyntheticPartitionDeviceName(device.DeviceName)
			if err != nil {
				// Not a synthetic-partition device, skip
				continue
			}
			ps.gpuAllocCounts[gpuIndex]++
			ps.totalAllocCount++
		}
	}

	// Re-apply memory taints if a memory mode is active
	if ps.activeMemoryMode != "" {
		ps.applyMemoryTaints(ps.activeMemoryMode)
	}

	klog.Infof("Partition state recovered: memoryMode=%q, computeModes=%v, totalAllocs=%d",
		ps.activeMemoryMode, ps.gpuComputeModes, ps.totalAllocCount)
}

// applyMemoryTaints adds NoExecute taints to all synthetic-partition devices
// whose memory mode is incompatible with the given active mode.
func (ps *PartitionState) applyMemoryTaints(activeMemoryMode string) {
	for _, device := range ps.allocatable {
		if device.SyntheticPartition == nil {
			continue
		}
		ap := device.SyntheticPartition
		if ap.MemoryPartition != activeMemoryMode {
			ap.Taints = []resourceapi.DeviceTaint{
				{
					Key:    consts.MemoryPartitionTaintKey,
					Value:  activeMemoryMode,
					Effect: resourceapi.DeviceTaintEffectNoExecute,
				},
			}
		} else {
			// Clear any taints from compatible devices
			ap.Taints = nil
		}
	}
}

// removeMemoryTaints removes all memory partition taints from synthetic-partition devices.
func (ps *PartitionState) removeMemoryTaints() {
	for _, device := range ps.allocatable {
		if device.SyntheticPartition == nil {
			continue
		}
		device.SyntheticPartition.Taints = nil
	}
}

// HasTaints returns true if any synthetic-partition devices currently have taints.
func (ps *PartitionState) HasTaints() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.activeMemoryMode != ""
}

// writeComputePartitionViaSysfs sets the compute partition mode via sysfs as a fallback.
func (ps *PartitionState) writeComputePartitionViaSysfs(gpuIndex int, mode string) error {
	pciAddr, ok := ps.gpuPCIAddresses[gpuIndex]
	if !ok {
		return fmt.Errorf("no PCI address for GPU %d", gpuIndex)
	}
	path := filepath.Join("/sys/bus/pci/devices", pciAddr, "current_compute_partition")
	if err := os.WriteFile(path, []byte(strings.ToUpper(mode)), 0644); err != nil {
		return fmt.Errorf("sysfs write to %s failed: %v", path, err)
	}
	klog.Infof("Successfully set compute partition mode %q on GPU %d via sysfs", strings.ToUpper(mode), gpuIndex)
	return nil
}

// writeMemoryPartitionViaSysfs sets the memory partition mode via sysfs as a fallback.
func (ps *PartitionState) writeMemoryPartitionViaSysfs(gpuIndex int, mode string) error {
	pciAddr, ok := ps.gpuPCIAddresses[gpuIndex]
	if !ok {
		return fmt.Errorf("no PCI address for GPU %d", gpuIndex)
	}
	path := filepath.Join("/sys/bus/pci/devices", pciAddr, "current_memory_partition")
	if err := os.WriteFile(path, []byte(strings.ToUpper(mode)), 0644); err != nil {
		return fmt.Errorf("sysfs write to %s failed: %v", path, err)
	}
	klog.Infof("Successfully set memory partition mode %q on GPU %d via sysfs", strings.ToUpper(mode), gpuIndex)
	return nil
}

// readCurrentComputePartition reads the current compute partition mode from sysfs.
func (ps *PartitionState) readCurrentComputePartition(gpuIndex int) string {
	pciAddr, ok := ps.gpuPCIAddresses[gpuIndex]
	if !ok {
		return ""
	}
	path := filepath.Join("/sys/bus/pci/devices", pciAddr, "current_compute_partition")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readCurrentMemoryPartition reads the current memory partition mode from sysfs.
func (ps *PartitionState) readCurrentMemoryPartition(gpuIndex int) string {
	pciAddr, ok := ps.gpuPCIAddresses[gpuIndex]
	if !ok {
		return ""
	}
	path := filepath.Join("/sys/bus/pci/devices", pciAddr, "current_memory_partition")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
