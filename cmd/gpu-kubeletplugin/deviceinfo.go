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
	"strings"

	"github.com/ROCm/k8s-gpu-dra-driver/pkg/consts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

// AmdGpuInfo represents a full AMD GPU device
type AmdGpuInfo struct {
	UUID             string
	ProductName      string
	KFDID            string // KFD-derived PCI address for internal parent-child tracking
	DeviceID         string // sysfs PCI device ID (e.g., "0x740f")
	DriverVersion    string
	PCIAddress       string
	PartitionProfile string
	MemoryBytes      uint64
	ComputeUnits     int
	SimdUnits        int
	NumaNode         int
	cardIndex        int // unexported: for CanonicalName and CDI path derivation
	renderIndex      int // unexported: for CanonicalName and CDI path derivation
	pcieRootAttr     deviceattribute.DeviceAttribute
	pciBusIDAttr     deviceattribute.DeviceAttribute
}

// AmdPartitionInfo represents a partition of an AMD GPU
type AmdPartitionInfo struct {
	Parent           *AmdGpuInfo
	UUID             string
	PartitionProfile string
	MemoryBytes      uint64
	ComputeUnits     int
	SimdUnits        int
	NumaNode         int
	cardIndex        int // unexported: for CanonicalName and CDI path derivation
	renderIndex      int // unexported: for CanonicalName and CDI path derivation
}

// CanonicalName returns the canonical name for this GPU
func (d *AmdGpuInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%v-%v", d.cardIndex, d.renderIndex)
}

// GetDevice returns the DRA Device representation for a full AMD GPU
func (d *AmdGpuInfo) GetDevice() resourceapi.Device {
	attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type":          {StringValue: ptr.To(AmdGpuDeviceType)},
		"productName":   {StringValue: ptr.To(d.ProductName)},
		"driverVersion": {VersionValue: ptr.To(d.DriverVersion)},
		"numaNode":      {IntValue: ptr.To(int64(d.NumaNode))},
	}
	if d.DeviceID != "" {
		attributes["deviceID"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.DeviceID)}
	}
	if d.PartitionProfile != "" {
		attributes["partitionProfile"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.PartitionProfile)}
	}
	if d.pciBusIDAttr.Name != "" {
		attributes[d.pciBusIDAttr.Name] = d.pciBusIDAttr.Value
	}
	if d.pcieRootAttr.Name != "" {
		attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}
	return resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attributes,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory":       {Value: *resource.NewQuantity(int64(d.MemoryBytes), resource.BinarySI)},
			"computeUnits": {Value: *resource.NewQuantity(int64(d.ComputeUnits), resource.BinarySI)},
			"simdUnits":    {Value: *resource.NewQuantity(int64(d.SimdUnits), resource.BinarySI)},
		},
	}
}

// CanonicalName returns the canonical name for this partition
func (d *AmdPartitionInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-%v-%v", d.cardIndex, d.renderIndex)
}

// GetDevice returns the DRA Device representation for an AMD GPU partition
func (d *AmdPartitionInfo) GetDevice() resourceapi.Device {
	attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type":             {StringValue: ptr.To(AmdPartitionDeviceType)},
		"productName":      {StringValue: ptr.To(d.Parent.ProductName)},
		"driverVersion":    {VersionValue: ptr.To(d.Parent.DriverVersion)},
		"partitionProfile": {StringValue: ptr.To(d.PartitionProfile)},
		"numaNode":         {IntValue: ptr.To(int64(d.NumaNode))},
	}
	if d.Parent.DeviceID != "" {
		attributes["deviceID"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.Parent.DeviceID)}
	}
	if d.Parent.pciBusIDAttr.Name != "" {
		attributes[d.Parent.pciBusIDAttr.Name] = d.Parent.pciBusIDAttr.Value
	}
	if d.Parent.pcieRootAttr.Name != "" {
		attributes[d.Parent.pcieRootAttr.Name] = d.Parent.pcieRootAttr.Value
	}
	return resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attributes,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory":       {Value: *resource.NewQuantity(int64(d.MemoryBytes), resource.BinarySI)},
			"computeUnits": {Value: *resource.NewQuantity(int64(d.ComputeUnits), resource.BinarySI)},
			"simdUnits":    {Value: *resource.NewQuantity(int64(d.SimdUnits), resource.BinarySI)},
		},
	}
}

// SyntheticPartitionDevice represents a virtual device advertising a possible
// compute+memory partition configuration that the driver can dynamically apply.
type SyntheticPartitionDevice struct {
	GPUIndex           int
	ComputePartition   string
	MemoryPartition    string
	PartitionCount     int
	PCIAddress         string
	ProductName        string
	DeviceID           string
	DriverVersion      string
	MemoryBytes        uint64
	ComputeUnits       int
	SimdUnits          int
	NumaNode           int
	pcieRootAttr       deviceattribute.DeviceAttribute
	pciBusIDAttr       deviceattribute.DeviceAttribute
	Taints             []resourceapi.DeviceTaint
}

// CanonicalName returns the canonical name for this synthetic partition device.
// Format: gpu-<gpuIndex>-<computePartition>-<memoryPartition>
func (d *SyntheticPartitionDevice) CanonicalName() string {
	return fmt.Sprintf("gpu-%d-%s-%s", d.GPUIndex, d.ComputePartition, d.MemoryPartition)
}

// GetDevice returns the DRA Device representation for a synthetic partition device
func (d *SyntheticPartitionDevice) GetDevice() resourceapi.Device {
	attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type":              {StringValue: ptr.To(SyntheticPartitionDeviceType)},
		"computePartition":  {StringValue: ptr.To(d.ComputePartition)},
		"memoryPartition":   {StringValue: ptr.To(d.MemoryPartition)},
		"gpuIndex":          {IntValue: ptr.To(int64(d.GPUIndex))},
		"productName":       {StringValue: ptr.To(d.ProductName)},
		"driverVersion":     {VersionValue: ptr.To(d.DriverVersion)},
		"numaNode":          {IntValue: ptr.To(int64(d.NumaNode))},
	}
	if d.DeviceID != "" {
		attributes["deviceID"] = resourceapi.DeviceAttribute{StringValue: ptr.To(d.DeviceID)}
	}
	if d.pciBusIDAttr.Name != "" {
		attributes[d.pciBusIDAttr.Name] = d.pciBusIDAttr.Value
	}
	if d.pcieRootAttr.Name != "" {
		attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attributes,
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory":       {Value: *resource.NewQuantity(int64(d.MemoryBytes), resource.BinarySI)},
			"computeUnits": {Value: *resource.NewQuantity(int64(d.ComputeUnits), resource.BinarySI)},
			"simdUnits":    {Value: *resource.NewQuantity(int64(d.SimdUnits), resource.BinarySI)},
		},
	}

	// Build mutual exclusion counter set so only one config per physical GPU
	// can be allocated at a time.
	counterSet := d.buildMutexCounterSet()
	if counterSet != nil {
		device.ConsumesCounters = []resourceapi.DeviceCounterConsumption{*counterSet}
	}

	if len(d.Taints) > 0 {
		device.Taints = d.Taints
	}

	return device
}

// mutexCounterSetName returns the name of the counter set used to enforce
// mutual exclusion among synthetic partition configs on the same physical GPU.
func (d *SyntheticPartitionDevice) mutexCounterSetName() string {
	return fmt.Sprintf("gpu-%d-mutex", d.GPUIndex)
}

// buildMutexCounterSet returns the DeviceCounterConsumption entry that consumes
// the mutex counter for this GPU, ensuring that at most one partition config
// can be allocated per physical GPU.
func (d *SyntheticPartitionDevice) buildMutexCounterSet() *resourceapi.DeviceCounterConsumption {
	return &resourceapi.DeviceCounterConsumption{
		CounterSet: d.mutexCounterSetName(),
		Counters: map[string]resourceapi.Counter{
			"gpuSlots": {
				Value: *resource.NewQuantity(1, resource.DecimalSI),
			},
		},
	}
}

// IsCompatibleMemoryMode checks if the desired memory partition mode is
// compatible with the current active memory mode on the node. All GPUs share
// the same memory partition mode, so a request for nps1 is incompatible with
// an active nps4 mode.
func (d *SyntheticPartitionDevice) IsCompatibleMemoryMode(activeMode string) bool {
	if activeMode == "" {
		return true
	}
	return strings.EqualFold(d.MemoryPartition, activeMode)
}

// parseSyntheticPartitionDeviceName parses a synthetic partition device canonical name
// and returns (gpuIndex, computePartition, memoryPartition, error).
func parseSyntheticPartitionDeviceName(name string) (int, string, string, error) {
	// Format: gpu-<gpuIndex>-<computePartition>-<memoryPartition>
	parts := strings.SplitN(name, "-", 4)
	if len(parts) != 4 || parts[0] != "gpu" {
		return 0, "", "", fmt.Errorf("invalid synthetic partition device name: %s", name)
	}
	var gpuIndex int
	_, err := fmt.Sscanf(parts[1], "%d", &gpuIndex)
	if err != nil {
		return 0, "", "", fmt.Errorf("failed to parse GPU index from device name %s: %v", name, err)
	}
	computePartition := parts[2]
	memoryPartition := parts[3]

	// Validate compute partition
	switch computePartition {
	case consts.ComputePartitionSPX, consts.ComputePartitionDPX, consts.ComputePartitionQPX, consts.ComputePartitionCPX:
	default:
		return 0, "", "", fmt.Errorf("invalid compute partition %q in device name %s", computePartition, name)
	}

	// Validate memory partition
	switch memoryPartition {
	case consts.MemoryPartitionNPS1, consts.MemoryPartitionNPS2, consts.MemoryPartitionNPS4:
	default:
		return 0, "", "", fmt.Errorf("invalid memory partition %q in device name %s", memoryPartition, name)
	}

	return gpuIndex, computePartition, memoryPartition, nil
}
