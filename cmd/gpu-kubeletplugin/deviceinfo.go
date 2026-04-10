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

	"github.com/ROCm/k8s-gpu-dra-driver/pkg/consts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

// AmdGpuInfo represents a full AMD GPU device
type AmdGpuInfo struct {
	UUID             string
	RenderIndex      int
	CardIndex        int
	ProductName      string
	Family           string
	DeviceID         string
	DriverVersion    string
	DriverSrcVersion string
	PCIAddress       string
	PartitionProfile string
	MemoryBytes      uint64
	ComputeUnits     int
	SimdUnits        int
	NumaNode         int
	// PCIe root attribute for topology awareness
	pcieRootAttr deviceattribute.DeviceAttribute
}

// AmdPartitionInfo represents a partition of an AMD GPU
type AmdPartitionInfo struct {
	Parent           *AmdGpuInfo // Reference to parent GPU
	UUID             string
	RenderIndex      int
	CardIndex        int
	PartitionProfile string
	MemoryBytes      uint64
	ComputeUnits     int
	SimdUnits        int
	NumaNode         int
}

// CanonicalName returns the canonical name for this GPU
func (d *AmdGpuInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%v-%v", d.CardIndex, d.RenderIndex)
}

// GetDevice returns the DRA Device representation for a full AMD GPU
func (d *AmdGpuInfo) GetDevice() resourceapi.Device {
	attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(AmdGpuDeviceType),
		},
		"pciAddr": {
			StringValue: ptr.To(d.PCIAddress),
		},
		"cardIndex": {
			IntValue: ptr.To(int64(d.CardIndex)),
		},
		"renderIndex": {
			IntValue: ptr.To(int64(d.RenderIndex)),
		},
		"deviceID": {
			StringValue: ptr.To(d.DeviceID),
		},
		"family": {
			StringValue: ptr.To(d.Family),
		},
		"productName": {
			StringValue: ptr.To(d.ProductName),
		},
		"driverVersion": {
			VersionValue: ptr.To(d.DriverVersion),
		},
		"driverSrcVersion": {
			StringValue: ptr.To(d.DriverSrcVersion),
		},
		"partitionProfile": {
			StringValue: ptr.To(d.PartitionProfile),
		},
		"numaNode": {
			IntValue: ptr.To(int64(d.NumaNode)),
		},
	}

	// Add PCIe root attribute if available
	if d.pcieRootAttr.Name != "" {
		attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	return resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attributes,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(d.MemoryBytes), resource.BinarySI),
			},
			"computeUnits": {
				Value: *resource.NewQuantity(int64(d.ComputeUnits), resource.BinarySI),
			},
			"simdUnits": {
				Value: *resource.NewQuantity(int64(d.SimdUnits), resource.BinarySI),
			},
		},
	}
}

// CanonicalName returns the canonical name for this partition
func (d *AmdPartitionInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%v-%v", d.CardIndex, d.RenderIndex)
}

// GetDevice returns the DRA Device representation for an AMD GPU partition
func (d *AmdPartitionInfo) GetDevice() resourceapi.Device {
	attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(AmdPartitionDeviceType),
		},
		"parentPciAddr": {
			StringValue: ptr.To(d.Parent.PCIAddress),
		},
		"cardIndex": {
			IntValue: ptr.To(int64(d.CardIndex)),
		},
		"renderIndex": {
			IntValue: ptr.To(int64(d.RenderIndex)),
		},
		"parentDeviceID": {
			StringValue: ptr.To(d.Parent.DeviceID),
		},
		"family": {
			StringValue: ptr.To(d.Parent.Family),
		},
		"productName": {
			StringValue: ptr.To(d.Parent.ProductName),
		},
		"driverVersion": {
			VersionValue: ptr.To(d.Parent.DriverVersion),
		},
		"driverSrcVersion": {
			StringValue: ptr.To(d.Parent.DriverSrcVersion),
		},
		"partitionProfile": {
			StringValue: ptr.To(d.PartitionProfile),
		},
		"numaNode": {
			IntValue: ptr.To(int64(d.NumaNode)),
		},
	}

	// Add PCIe root attribute if available (inherited from parent)
	if d.Parent.pcieRootAttr.Name != "" {
		attributes[d.Parent.pcieRootAttr.Name] = d.Parent.pcieRootAttr.Value
	}

	return resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attributes,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: *resource.NewQuantity(int64(d.MemoryBytes), resource.BinarySI),
			},
			"computeUnits": {
				Value: *resource.NewQuantity(int64(d.ComputeUnits), resource.BinarySI),
			},
			"simdUnits": {
				Value: *resource.NewQuantity(int64(d.SimdUnits), resource.BinarySI),
			},
		},
	}
}

// SyntheticPartitionDevice represents a virtual partition device for synthetic-partition mode.
// Each physical GPU generates one of these for each valid compute+memory combination.
type SyntheticPartitionDevice struct {
	GPUIndex         int
	ComputePartition string
	MemoryPartition  string
	PartitionCount   int
	PCIAddress       string
	ProductName      string
	Family           string
	DriverVersion    string
	DriverSrcVersion string
	MemoryBytes      uint64 // per-partition memory (total / count)
	ComputeUnits     int    // per-partition CUs
	SimdUnits        int    // per-partition SIMDs
	NumaNode         int
	pcieRootAttr     deviceattribute.DeviceAttribute
	// Taints holds any taints applied to this device (e.g. memory partition conflicts).
	// This field is set dynamically and may be updated during runtime.
	Taints []resourceapi.DeviceTaint
}

// CanonicalName returns the canonical name for this synthetic-partition device
func (d *SyntheticPartitionDevice) CanonicalName() string {
	return fmt.Sprintf("gpu-%d-%s-%s", d.GPUIndex, d.ComputePartition, d.MemoryPartition)
}

// GetDevice returns the DRA Device representation for a synthetic-partition device
func (d *SyntheticPartitionDevice) GetDevice() resourceapi.Device {
	// Use the same user-visible type attribute values as real GPU/partition devices:
	// SPX (full GPU, 1 partition) -> "amdgpu", DPX/CPX (partitioned) -> "amdgpu-partition"
	deviceType := AmdPartitionDeviceType
	if d.ComputePartition == consts.ComputePartitionSPX {
		deviceType = AmdGpuDeviceType
	}

	attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(deviceType),
		},
		"computePartition": {
			StringValue: ptr.To(d.ComputePartition),
		},
		"memoryPartition": {
			StringValue: ptr.To(d.MemoryPartition),
		},
		"gpuIndex": {
			IntValue: ptr.To(int64(d.GPUIndex)),
		},
		"productName": {
			StringValue: ptr.To(d.ProductName),
		},
		"pciAddr": {
			StringValue: ptr.To(d.PCIAddress),
		},
		"family": {
			StringValue: ptr.To(d.Family),
		},
		"driverVersion": {
			VersionValue: ptr.To(d.DriverVersion),
		},
		"driverSrcVersion": {
			StringValue: ptr.To(d.DriverSrcVersion),
		},
		"numaNode": {
			IntValue: ptr.To(int64(d.NumaNode)),
		},
	}

	// Add PCIe root attribute if available
	if d.pcieRootAttr.Name != "" {
		attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	// Build partitions capacity entry. For partition types with count > 1,
	// use RequestPolicy so each allocation consumes 1 partition by default.
	// For SPX (count=1), omit RequestPolicy since the device cannot be
	// shared and AllowMultipleAllocations would be false.
	partitionsCapacity := resourceapi.DeviceCapacity{
		Value: *resource.NewQuantity(int64(d.PartitionCount), resource.DecimalSI),
	}
	if d.PartitionCount > 1 {
		partitionsCapacity.RequestPolicy = &resourceapi.CapacityRequestPolicy{
			Default: resource.NewQuantity(1, resource.DecimalSI),
		}
	}

	// Build capacity entries for memory, computeUnits, simdUnits.
	// d.MemoryBytes/ComputeUnits/SimdUnits are per-partition values.
	// Value = total for device (per-partition * count), Default = per-partition.
	memoryCapacity := resourceapi.DeviceCapacity{
		Value: *resource.NewQuantity(int64(d.MemoryBytes)*int64(d.PartitionCount), resource.BinarySI),
	}
	computeUnitsCapacity := resourceapi.DeviceCapacity{
		Value: *resource.NewQuantity(int64(d.ComputeUnits)*int64(d.PartitionCount), resource.DecimalSI),
	}
	simdUnitsCapacity := resourceapi.DeviceCapacity{
		Value: *resource.NewQuantity(int64(d.SimdUnits)*int64(d.PartitionCount), resource.DecimalSI),
	}
	if d.PartitionCount > 1 {
		memoryCapacity.RequestPolicy = &resourceapi.CapacityRequestPolicy{
			Default: resource.NewQuantity(int64(d.MemoryBytes), resource.BinarySI),
		}
		computeUnitsCapacity.RequestPolicy = &resourceapi.CapacityRequestPolicy{
			Default: resource.NewQuantity(int64(d.ComputeUnits), resource.DecimalSI),
		}
		simdUnitsCapacity.RequestPolicy = &resourceapi.CapacityRequestPolicy{
			Default: resource.NewQuantity(int64(d.SimdUnits), resource.DecimalSI),
		}
	}

	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attributes,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"partitions":   partitionsCapacity,
			"memory":       memoryCapacity,
			"computeUnits": computeUnitsCapacity,
			"simdUnits":    simdUnitsCapacity,
		},
		ConsumesCounters: []resourceapi.DeviceCounterConsumption{
			{
				CounterSet: fmt.Sprintf("gpu-%d-mutex", d.GPUIndex),
				Counters: map[string]resourceapi.Counter{
					"partition-mode": {
						Value: *resource.NewQuantity(1, resource.DecimalSI),
					},
				},
			},
		},
	}

	// AllowMultipleAllocations is true for partition types with count > 1
	if d.PartitionCount > 1 {
		device.AllowMultipleAllocations = ptr.To(true)
	}

	// Apply taints if any (e.g., memory partition conflicts)
	if len(d.Taints) > 0 {
		device.Taints = d.Taints
	}

	return device
}

// mutexCounterSetName returns the shared counter set name for a GPU's partition mutex.
func mutexCounterSetName(gpuIndex int) string {
	return fmt.Sprintf("gpu-%d-mutex", gpuIndex)
}

// buildMutexCounterSet returns the CounterSet for a GPU's partition-mode mutex.
func buildMutexCounterSet(gpuIndex int) resourceapi.CounterSet {
	return resourceapi.CounterSet{
		Name: mutexCounterSetName(gpuIndex),
		Counters: map[string]resourceapi.Counter{
			"partition-mode": {
				Value: *resource.NewQuantity(1, resource.DecimalSI),
			},
		},
	}
}

// IsCompatibleMemoryMode checks whether the given memory mode is compatible
// with the currently active memory mode (or if no mode is active).
func IsCompatibleMemoryMode(activeMode, requestedMode string) bool {
	return activeMode == "" || activeMode == requestedMode
}

// parseSyntheticPartitionDeviceName parses a device name like "gpu-0-cpx-nps4"
// and returns the gpuIndex, compute partition mode, and memory partition mode.
func parseSyntheticPartitionDeviceName(name string) (gpuIndex int, compute, memory string, err error) {
	_, err = fmt.Sscanf(name, "gpu-%d-", &gpuIndex)
	if err != nil {
		return 0, "", "", fmt.Errorf("failed to parse synthetic-partition device name %s: %v", name, err)
	}

	// Parse the remaining parts after "gpu-<index>-"
	prefix := fmt.Sprintf("gpu-%d-", gpuIndex)
	remainder := name[len(prefix):]

	// Valid patterns: "spx-nps1", "dpx-nps2", "cpx-nps1", "cpx-nps4"
	for _, cfg := range consts.ValidPartitionConfigs {
		expected := fmt.Sprintf("%s-%s", cfg.Compute, cfg.Memory)
		if remainder == expected {
			return gpuIndex, cfg.Compute, cfg.Memory, nil
		}
	}

	return 0, "", "", fmt.Errorf("unrecognized synthetic-partition device name: %s", name)
}
