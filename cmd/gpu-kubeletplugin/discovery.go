/*
 * Copyright 2023 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

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
	"sort"

	"github.com/ROCm/k8s-gpu-dra-driver/pkg/amdgpu"
	"github.com/ROCm/k8s-gpu-dra-driver/pkg/consts"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	klog "k8s.io/klog/v2"
)

func parseDeviceName(name string) (int, int, error) {
	var card, renderD int
	_, err := fmt.Sscanf(name, "gpu-%d-%d", &card, &renderD)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse device name %s: %v", name, err)
	}
	return card, renderD, nil
}

// Helper function to extract topology information from GPU info map
func extractTopologyInfo(gpuInfoMap map[string]interface{}) (simdUnits, computeUnits int) {
	if simdCount, ok := gpuInfoMap["simdCount"].(int); ok {
		simdUnits = simdCount
	}
	if cuCount, ok := gpuInfoMap["cuCount"].(int); ok {
		computeUnits = cuCount
	}
	return
}

// Helper function to get memory bytes with fallback
func getMemoryBytes(gpuInfoMap map[string]interface{}, defaultBytes uint64, deviceType, pciAddr string) uint64 {
	if vramBytes, ok := gpuInfoMap["vramBytes"].(uint64); ok && vramBytes > 0 {
		return vramBytes
	}
	// Fallback to default if VRAM parsing failed
	klog.Warningf("VRAM info not available for %s %s, using default %dGB", deviceType, pciAddr, defaultBytes/(1024*1024*1024))
	return defaultBytes
}

func getPcieInfo(gpuInfoMap map[string]interface{}) (deviceattribute.DeviceAttribute, string, error) {
	pciAddr := gpuInfoMap["pciAddr"].(string)

	// Use the PCI address from the device info (which is the parent's PCI address for partitions)
	pcieRootAttr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(pciAddr)
	if err != nil {
		return pcieRootAttr, "", fmt.Errorf("Failed to get PCIe root attribute for device %s (using PCI addr %s): %v", pciAddr, pciAddr, err)
	}

	return pcieRootAttr, pciAddr, nil
}

// enumerateAllPossibleDevices discovers AMD GPUs and returns allocatable devices.
//
// When enableSyntheticPartition is false, it discovers physical GPUs and
// partitions as-is (normal mode). The extra return values (gpuPCIAddresses,
// partitionableGPUs) are nil/empty.
//
// When enableSyntheticPartition is true, it generates virtual synthetic-partition
// devices for each valid compute+memory partition combination on partitionable
// GPUs, while non-partitionable GPUs are advertised as normal full GPUs. It
// also returns a map from GPU index to PCI address (for amd-smi targeting) and
// the list of GPU indices that support partitioning (for building shared
// counter sets).
func enumerateAllPossibleDevices(enableSyntheticPartition bool) (AllocatableDevices, map[int]string, []int, error) {
	alldevices := make(AllocatableDevices)
	allAMDGPUs := amdgpu.GetAMDGPUs()

	// Sort PCI addresses for deterministic GPU index assignment
	// (needed for synthetic-partition mode, but harmless in normal mode)
	pciAddrs := make([]string, 0, len(allAMDGPUs))
	for pciAddr := range allAMDGPUs {
		pciAddrs = append(pciAddrs, pciAddr)
	}
	sort.Strings(pciAddrs)

	var gpuPCIAddresses map[int]string
	var partitionableGPUs []int
	gpuIndex := 0

	if enableSyntheticPartition {
		gpuPCIAddresses = make(map[int]string)
	}

	for _, pciAddr := range pciAddrs {
		gpuInfoMap := allAMDGPUs[pciAddr]

		// Get PCIe root attribute for this device using the PCI address from the device info
		pcieRootAttr, pciAddrFromMap, err := getPcieInfo(gpuInfoMap)
		if err != nil {
			// Continue without PCIe root attribute rather than failing completely
			klog.Warning(err.Error())
		}

		// Check compute partition type to determine device type
		computePartitionType := gpuInfoMap["computePartitionType"].(string)
		memoryPartitionType := gpuInfoMap["memoryPartitionType"].(string)

		// Extract common topology information
		simdUnits, computeUnits := extractTopologyInfo(gpuInfoMap)
		totalMemory := getMemoryBytes(gpuInfoMap, 80*1024*1024*1024, "device", pciAddr)

		if enableSyntheticPartition {
			// Record PCI address for this GPU index (needed for amd-smi targeting)
			gpuPCIAddresses[gpuIndex] = pciAddr

			supportsPartitioning := computePartitionType != ""

			if !supportsPartitioning {
				// GPU doesn't support partitioning - advertise as a normal full GPU
				amdGpuInfo := &AmdGpuInfo{
					PCIAddress:       pciAddr,
					CardIndex:        gpuInfoMap["card"].(int),
					RenderIndex:      gpuInfoMap["renderD"].(int),
					DeviceID:         gpuInfoMap["devID"].(string),
					DriverVersion:    gpuInfoMap["driverVersion"].(string),
					DriverSrcVersion: gpuInfoMap["driverSrcVersion"].(string),
					PartitionProfile: consts.DefaultPartitionProfile,
					Family:           gpuInfoMap["family"].(string),
					ProductName:      gpuInfoMap["productName"].(string),
					pcieRootAttr:     pcieRootAttr,
					SimdUnits:        simdUnits,
					ComputeUnits:     computeUnits,
					NumaNode:         gpuInfoMap["numaNode"].(int),
					MemoryBytes:      totalMemory,
				}
				device := &AllocatableDevice{AmdGpu: amdGpuInfo}
				alldevices[device.CanonicalName()] = device
				klog.Infof("GPU %d (%s) does not support partitioning, advertising as normal GPU: %s",
					gpuIndex, pciAddr, device.CanonicalName())
				gpuIndex++
				continue
			}

			partitionableGPUs = append(partitionableGPUs, gpuIndex)

			// Generate virtual partition devices for each valid compute+memory combination
			for _, cfg := range consts.ValidPartitionConfigs {
				apDevice := &SyntheticPartitionDevice{
					GPUIndex:         gpuIndex,
					ComputePartition: cfg.Compute,
					MemoryPartition:  cfg.Memory,
					PartitionCount:   cfg.PartitionCount,
					PCIAddress:       pciAddr,
					ProductName:      gpuInfoMap["productName"].(string),
					Family:           gpuInfoMap["family"].(string),
					DriverVersion:    gpuInfoMap["driverVersion"].(string),
					DriverSrcVersion: gpuInfoMap["driverSrcVersion"].(string),
					MemoryBytes:      totalMemory / uint64(cfg.PartitionCount),
					ComputeUnits:     computeUnits / cfg.PartitionCount,
					SimdUnits:        simdUnits / cfg.PartitionCount,
					NumaNode:         gpuInfoMap["numaNode"].(int),
					pcieRootAttr:     pcieRootAttr,
				}

				device := &AllocatableDevice{SyntheticPartition: apDevice}
				alldevices[device.CanonicalName()] = device
				klog.Infof("Auto-partition device: %s (GPU %d, %s, %d partitions, %dMB each)",
					device.CanonicalName(), gpuIndex, fmt.Sprintf("%s-%s", cfg.Compute, cfg.Memory),
					cfg.PartitionCount, apDevice.MemoryBytes/(1024*1024))
			}

			gpuIndex++
		} else {
			// Normal mode: discover devices as-is
			if computePartitionType == consts.ComputePartitionSPX || computePartitionType == "" {
				// This is a full AMD GPU (either explicitly "spx" or no partition support)
				partitionProfile := consts.DefaultPartitionProfile
				if computePartitionType != "" && memoryPartitionType != "" {
					partitionProfile = fmt.Sprintf("%s_%s", computePartitionType, memoryPartitionType)
				}

				amdGpuInfo := &AmdGpuInfo{
					PCIAddress:       pciAddr,
					CardIndex:        gpuInfoMap["card"].(int),
					RenderIndex:      gpuInfoMap["renderD"].(int),
					DeviceID:         gpuInfoMap["devID"].(string),
					DriverVersion:    gpuInfoMap["driverVersion"].(string),
					DriverSrcVersion: gpuInfoMap["driverSrcVersion"].(string),
					PartitionProfile: partitionProfile,
					Family:           gpuInfoMap["family"].(string),
					ProductName:      gpuInfoMap["productName"].(string),
					pcieRootAttr:     pcieRootAttr,
					SimdUnits:        simdUnits,
					ComputeUnits:     computeUnits,
					NumaNode:         gpuInfoMap["numaNode"].(int),
					MemoryBytes:      totalMemory,
				}

				// Create allocatable device for the full GPU
				device := &AllocatableDevice{
					AmdGpu: amdGpuInfo,
				}
				alldevices[device.CanonicalName()] = device

				klog.Infof("Found full AMD GPU: %s, compute type: %s, memory type: %s",
					device.CanonicalName(), computePartitionType, memoryPartitionType)
			} else if computePartitionType != "" {
				// This is a partition - create both parent GPU info and partition info

				// Create parent GPU info
				parentGpuInfo := &AmdGpuInfo{
					PCIAddress:       pciAddrFromMap,
					DeviceID:         gpuInfoMap["devID"].(string),
					DriverVersion:    gpuInfoMap["driverVersion"].(string),
					DriverSrcVersion: gpuInfoMap["driverSrcVersion"].(string),
					Family:           gpuInfoMap["family"].(string),
					ProductName:      gpuInfoMap["productName"].(string),
					pcieRootAttr:     pcieRootAttr,
				}

				// Create partition info
				partitionInfo := &AmdPartitionInfo{
					CardIndex:        gpuInfoMap["card"].(int),
					RenderIndex:      gpuInfoMap["renderD"].(int),
					Parent:           parentGpuInfo,
					PartitionProfile: fmt.Sprintf("%s_%s", computePartitionType, memoryPartitionType),
					SimdUnits:        simdUnits,
					ComputeUnits:     computeUnits,
					NumaNode:         gpuInfoMap["numaNode"].(int),
					MemoryBytes:      getMemoryBytes(gpuInfoMap, 20*1024*1024*1024, "partition", pciAddr),
				}

				// Create allocatable device for the partition
				device := &AllocatableDevice{
					AmdPartition: partitionInfo,
				}
				alldevices[device.CanonicalName()] = device

				klog.Infof("Found AMD GPU partition: %s, compute type: %s, memory type: %s",
					device.CanonicalName(), computePartitionType, memoryPartitionType)
			} else {
				klog.Warningf("Unknown compute partition type '%s' for device %s, skipping", computePartitionType, pciAddr)
			}
		}
	}

	if enableSyntheticPartition {
		klog.Infof("Auto-partition mode: discovered %d physical GPUs (%d partitionable), %d virtual partition devices",
			len(gpuPCIAddresses), len(partitionableGPUs), len(alldevices))
	} else {
		klog.Infof("Discovered %d AMD GPU devices", len(alldevices))
	}
	return alldevices, gpuPCIAddresses, partitionableGPUs, nil
}
