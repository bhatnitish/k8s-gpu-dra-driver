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
	resourceapi "k8s.io/api/resource/v1"
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

func getPcieInfo(gpuInfoMap map[string]interface{}) (deviceattribute.DeviceAttribute, deviceattribute.DeviceAttribute, string, error) {
	pciAddr := gpuInfoMap["pciAddr"].(string)
	pcieRootAttr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(pciAddr)
	if err != nil {
		return pcieRootAttr, deviceattribute.DeviceAttribute{}, "", fmt.Errorf("Failed to get PCIe root attribute for device %s: %v", pciAddr, err)
	}
	pciBusIDAttr, err := deviceattribute.GetPCIBusIDAttribute(pciAddr)
	if err != nil {
		return pcieRootAttr, pciBusIDAttr, "", fmt.Errorf("Failed to get PCI Bus ID attribute for device %s: %v", pciAddr, err)
	}
	return pcieRootAttr, pciBusIDAttr, pciAddr, nil
}

func enumerateAllPossibleDevices(enableSyntheticPartition bool) (AllocatableDevices, map[int]string, []int, error) {
	alldevices := make(AllocatableDevices)
	allAMDGPUs := amdgpu.GetAMDGPUs()
	gpuPCIAddresses := make(map[int]string)
	var partitionableGPUs []int

	if enableSyntheticPartition {
		return enumerateSyntheticPartitionDevices(allAMDGPUs, alldevices, gpuPCIAddresses, partitionableGPUs)
	}

	for pciAddr, gpuInfoMap := range allAMDGPUs {
		// Get PCIe root attribute for this device using the PCI address from the device info
		pcieRootAttr, pciBusIDAttr, pciAddrFromMap, err := getPcieInfo(gpuInfoMap)
		if err != nil {
			// Continue without PCIe root attribute rather than failing completely
			klog.Warning(err.Error())
		}

		// Check compute partition type to determine device type
		computePartitionType := gpuInfoMap["computePartitionType"].(string)
		memoryPartitionType := gpuInfoMap["memoryPartitionType"].(string)

		// Extract common topology information
		simdUnits, computeUnits := extractTopologyInfo(gpuInfoMap)

		if computePartitionType == consts.ComputePartitionSPX || computePartitionType == "" {
			// This is a full AMD GPU (either explicitly "spx" or no partition support)
			partitionProfile := ""
			if computePartitionType != "" && memoryPartitionType != "" {
				partitionProfile = fmt.Sprintf("%s_%s", computePartitionType, memoryPartitionType)
			}

			amdGpuInfo := &AmdGpuInfo{
				PCIAddress:       pciAddr,
				cardIndex:        gpuInfoMap["card"].(int),
				renderIndex:      gpuInfoMap["renderD"].(int),
				KFDID:            gpuInfoMap["kfdID"].(string),
				DeviceID:         gpuInfoMap["deviceID"].(string),
				DriverVersion:    gpuInfoMap["driverVersion"].(string),
				PartitionProfile: partitionProfile,
				ProductName:      gpuInfoMap["productName"].(string),
				pcieRootAttr:     pcieRootAttr,
				pciBusIDAttr:     pciBusIDAttr,
				SimdUnits:        simdUnits,
				ComputeUnits:     computeUnits,
				NumaNode:         gpuInfoMap["numaNode"].(int),
				MemoryBytes:      getMemoryBytes(gpuInfoMap, 80*1024*1024*1024, "device", pciAddr),
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
				PCIAddress:    pciAddrFromMap,
				KFDID:         gpuInfoMap["kfdID"].(string),
				DeviceID:      gpuInfoMap["deviceID"].(string),
				DriverVersion: gpuInfoMap["driverVersion"].(string),
				ProductName:   gpuInfoMap["productName"].(string),
				pcieRootAttr:  pcieRootAttr,
				pciBusIDAttr:  pciBusIDAttr,
			}

			// Create partition info
			partitionInfo := &AmdPartitionInfo{
				cardIndex:        gpuInfoMap["card"].(int),
				renderIndex:      gpuInfoMap["renderD"].(int),
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

	klog.Infof("Discovered %d AMD GPU devices", len(alldevices))
	return alldevices, gpuPCIAddresses, partitionableGPUs, nil
}

// enumerateSyntheticPartitionDevices generates virtual SyntheticPartitionDevice entries
// for all valid compute+memory partition combinations on partitionable GPUs.
// Non-partitionable GPUs are advertised as normal full GPUs.
func enumerateSyntheticPartitionDevices(
	allAMDGPUs map[string]map[string]interface{},
	alldevices AllocatableDevices,
	gpuPCIAddresses map[int]string,
	partitionableGPUs []int,
) (AllocatableDevices, map[int]string, []int, error) {

	// Sort PCI addresses for deterministic GPU index assignment
	pciAddresses := make([]string, 0, len(allAMDGPUs))
	for pciAddr := range allAMDGPUs {
		pciAddresses = append(pciAddresses, pciAddr)
	}
	sort.Strings(pciAddresses)

	// Build a map from kfdID to GPU index for deduplication
	// (partitioned GPUs appear multiple times with same kfdID)
	kfdIDToIndex := make(map[string]int)
	gpuIndex := 0

	for _, pciAddr := range pciAddresses {
		gpuInfoMap := allAMDGPUs[pciAddr]

		kfdID := gpuInfoMap["kfdID"].(string)

		// Skip duplicate entries (same physical GPU seen through multiple XCP partitions)
		if _, exists := kfdIDToIndex[kfdID]; exists {
			continue
		}
		kfdIDToIndex[kfdID] = gpuIndex

		computePartitionType := gpuInfoMap["computePartitionType"].(string)
		memoryPartitionType := gpuInfoMap["memoryPartitionType"].(string)

		pcieRootAttr, pciBusIDAttr, _, err := getPcieInfo(gpuInfoMap)
		if err != nil {
			klog.Warning(err.Error())
		}

		simdUnits, computeUnits := extractTopologyInfo(gpuInfoMap)
		totalMemory := getMemoryBytes(gpuInfoMap, 80*1024*1024*1024, "device", pciAddr)
		driverVersion := gpuInfoMap["driverVersion"].(string)
		productName := gpuInfoMap["productName"].(string)
		deviceID := gpuInfoMap["deviceID"].(string)
		numaNode := gpuInfoMap["numaNode"].(int)

		gpuPCIAddresses[gpuIndex] = pciAddr

		// Check if GPU supports partitioning
		isPartitionable := computePartitionType != ""

		if !isPartitionable {
			// Non-partitionable GPU: advertise as normal full GPU
			amdGpuInfo := &AmdGpuInfo{
				PCIAddress:    pciAddr,
				cardIndex:     gpuInfoMap["card"].(int),
				renderIndex:   gpuInfoMap["renderD"].(int),
				KFDID:         kfdID,
				DeviceID:      deviceID,
				DriverVersion: driverVersion,
				ProductName:   productName,
				pcieRootAttr:  pcieRootAttr,
				pciBusIDAttr:  pciBusIDAttr,
				SimdUnits:     simdUnits,
				ComputeUnits:  computeUnits,
				NumaNode:      numaNode,
				MemoryBytes:   totalMemory,
			}

			device := &AllocatableDevice{AmdGpu: amdGpuInfo}
			alldevices[device.CanonicalName()] = device

			klog.Infof("Found non-partitionable AMD GPU: %s (index %d), compute type: %s, memory type: %s",
				device.CanonicalName(), gpuIndex, computePartitionType, memoryPartitionType)
		} else {
			// Partitionable GPU: generate synthetic devices for each valid config
			partitionableGPUs = append(partitionableGPUs, gpuIndex)

			for _, config := range consts.ValidPartitionConfigs {
				// Calculate per-partition resources based on partition count
				partitionMemory := totalMemory / uint64(config.PartitionCount)
				partitionCUs := computeUnits / config.PartitionCount
				partitionSIMDs := simdUnits / config.PartitionCount

				// Build taints for non-default memory modes
				var taints []resourceapi.DeviceTaint
				if config.MemoryMode != consts.MemoryPartitionNPS1 {
					taints = append(taints, resourceapi.DeviceTaint{
						Key:    consts.MemoryPartitionTaintKey,
						Value:  config.MemoryMode,
						Effect: resourceapi.DeviceTaintEffectNoSchedule,
					})
				}

				syntheticDevice := &SyntheticPartitionDevice{
					GPUIndex:         gpuIndex,
					ComputePartition: config.ComputeMode,
					MemoryPartition:  config.MemoryMode,
					PartitionCount:   config.PartitionCount,
					PCIAddress:       pciAddr,
					ProductName:      productName,
					DeviceID:         deviceID,
					DriverVersion:    driverVersion,
					MemoryBytes:      partitionMemory,
					ComputeUnits:     partitionCUs,
					SimdUnits:        partitionSIMDs,
					NumaNode:         numaNode,
					pcieRootAttr:     pcieRootAttr,
					pciBusIDAttr:     pciBusIDAttr,
					Taints:           taints,
				}

				device := &AllocatableDevice{SyntheticPartition: syntheticDevice}
				alldevices[device.CanonicalName()] = device

				klog.Infof("Generated synthetic partition device: %s (GPU %d, %s_%s, %d partitions)",
					device.CanonicalName(), gpuIndex, config.ComputeMode, config.MemoryMode, config.PartitionCount)
			}
		}

		gpuIndex++
	}

	klog.Infof("Discovered %d AMD GPU devices (%d partitionable GPUs)", len(alldevices), len(partitionableGPUs))
	return alldevices, gpuPCIAddresses, partitionableGPUs, nil
}
