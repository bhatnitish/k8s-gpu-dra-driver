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
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"
	"syscall"

	configapi "github.com/ROCm/k8s-gpu-dra-driver/api/amd.com/resource/gpu/v1alpha1"
	"github.com/ROCm/k8s-gpu-dra-driver/pkg/amdgpu"
	"github.com/ROCm/k8s-gpu-dra-driver/pkg/consts"
	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	klog "k8s.io/klog/v2"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

type PreparedDevices []*PreparedDevice
type PreparedClaims map[string]PreparedDevices
type PerDeviceCDIContainerEdits map[string]*cdiapi.ContainerEdits

type OpaqueDeviceConfig struct {
	Requests []string
	Config   runtime.Object
}

type PreparedDevice struct {
	drapbv1.Device
	ContainerEdits *cdiapi.ContainerEdits
}

func (pds PreparedDevices) GetDevices() []*drapbv1.Device {
	var devices []*drapbv1.Device
	for _, pd := range pds {
		devices = append(devices, &pd.Device)
	}
	return devices
}

type DeviceState struct {
	sync.Mutex
	cdi                *CDIHandler
	allocatable        AllocatableDevices
	checkpointManager  checkpointmanager.CheckpointManager
	partitionState     *PartitionState
	driver             *driver // back-reference for re-publishing resources
	syntheticPartition bool    // whether synthetic-partition mode is enabled
}

func NewDeviceState(config *Config) (*DeviceState, error) {
	allocatable, gpuPCIAddresses, partitionableGPUs, err := enumerateAllPossibleDevices(config.flags.enableSyntheticPartition)
	if err != nil {
		return nil, fmt.Errorf("error enumerating devices: %v", err)
	}

	cdi, err := NewCDIHandler(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %v", err)
	}

	err = cdi.CreateCommonSpecFile()
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for common edits: %v", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	state := &DeviceState{
		cdi:                cdi,
		allocatable:        allocatable,
		checkpointManager:  checkpointManager,
		syntheticPartition: config.flags.enableSyntheticPartition,
	}

	// Set up partition state for synthetic-partition mode
	if config.flags.enableSyntheticPartition {
		state.partitionState = NewPartitionState(gpuPCIAddresses, partitionableGPUs, allocatable)
	}

	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}

	checkpointExists := false
	for _, c := range checkpoints {
		if c == DriverPluginCheckpointFile {
			checkpointExists = true
			break
		}
	}

	if checkpointExists {
		// Recover partition state from checkpoint if synthetic-partition is enabled
		if config.flags.enableSyntheticPartition && state.partitionState != nil {
			checkpoint := newCheckpoint()
			if err := state.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
				klog.Warningf("Failed to read checkpoint for partition state recovery: %v", err)
			} else {
				state.partitionState.RecoverFromCheckpoint(
					checkpoint.V1.ActiveMemoryMode,
					checkpoint.V1.GPUComputeModes,
					checkpoint.V1.PreparedClaims,
				)
			}
		}
		return state, nil
	}

	checkpoint := newCheckpoint()
	if err := state.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return state, nil
}

func (s *DeviceState) Prepare(claim *resourceapi.ResourceClaim) ([]*drapbv1.Device, error) {
	s.Lock()
	defer s.Unlock()

	claimUID := string(claim.UID)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] != nil {
		return preparedClaims[claimUID].GetDevices(), nil
	}

	// If synthetic-partition mode is enabled, handle partition setup before preparing devices
	if s.syntheticPartition && s.partitionState != nil && claim.Status.Allocation != nil {
		for _, result := range claim.Status.Allocation.Devices.Results {
			device, exists := s.allocatable[result.Device]
			if !exists {
				continue
			}
			if device.SyntheticPartition != nil {
				if err := s.partitionState.PreparePartition(result.Device); err != nil {
					return nil, fmt.Errorf("error preparing partition for device %s: %v", result.Device, err)
				}
			}
		}
	}

	preparedDevices, err := s.prepareDevices(claim)
	if err != nil {
		return nil, fmt.Errorf("prepare failed: %v", err)
	}

	if err = s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %v", err)
	}

	preparedClaims[claimUID] = preparedDevices

	// Save partition state to checkpoint if synthetic-partition mode is enabled
	if s.syntheticPartition && s.partitionState != nil {
		checkpoint.V1.ActiveMemoryMode = s.partitionState.GetActiveMemoryMode()
		checkpoint.V1.GPUComputeModes = s.partitionState.GetGPUComputeModes()
	}

	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	// Re-publish resources if memory taints changed (first allocation sets memory mode)
	if s.syntheticPartition && s.partitionState != nil && s.partitionState.HasTaints() && s.driver != nil {
		if err := s.driver.republishResources(context.TODO()); err != nil {
			klog.Warningf("Failed to re-publish resources after partition prepare: %v", err)
		}
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

func (s *DeviceState) Unprepare(claimUID string) error {
	s.Lock()
	defer s.Unlock()

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] == nil {
		return nil
	}

	// Track whether taints changed for re-publishing
	taintsChanged := false

	// If synthetic-partition mode, handle partition cleanup
	if s.syntheticPartition && s.partitionState != nil {
		for _, device := range preparedClaims[claimUID] {
			allocDevice, exists := s.allocatable[device.DeviceName]
			if !exists {
				continue
			}
			if allocDevice.SyntheticPartition != nil {
				changed, err := s.partitionState.UnpreparePartition(device.DeviceName)
				if err != nil {
					klog.Warningf("Error unpreparing partition for device %s: %v", device.DeviceName, err)
				}
				taintsChanged = taintsChanged || changed
			}
		}
	}

	if err := s.unprepareDevices(claimUID, preparedClaims[claimUID]); err != nil {
		return fmt.Errorf("unprepare failed: %v", err)
	}

	err := s.cdi.DeleteClaimSpecFile(claimUID)
	if err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %v", err)
	}

	delete(preparedClaims, claimUID)

	// Save partition state to checkpoint if synthetic-partition mode is enabled
	if s.syntheticPartition && s.partitionState != nil {
		checkpoint.V1.ActiveMemoryMode = s.partitionState.GetActiveMemoryMode()
		checkpoint.V1.GPUComputeModes = s.partitionState.GetGPUComputeModes()
	}

	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	// Re-publish resources if taints changed (all allocations released)
	if taintsChanged && s.driver != nil {
		if err := s.driver.republishResources(context.TODO()); err != nil {
			klog.Warningf("Failed to re-publish resources after partition unprepare: %v", err)
		}
	}

	return nil
}

func (s *DeviceState) prepareDevices(claim *resourceapi.ResourceClaim) (PreparedDevices, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	// Retrieve the full set of device configs for the driver.
	configs, err := GetOpaqueDeviceConfigs(
		configapi.Decoder,
		consts.DriverName,
		claim.Status.Allocation.Devices.Config,
	)
	if err != nil {
		return nil, fmt.Errorf("error getting opaque device configs: %v", err)
	}

	// Add the default GPU Config to the front of the config list with the
	// lowest precedence. This guarantees there will be at least one config in
	// the list with len(Requests) == 0 for the lookup below.
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{
		Requests: []string{},
		Config:   configapi.DefaultGpuConfig(),
	})

	// Look through the configs and figure out which one will be applied to
	// each device allocation result based on their order of precedence.
	configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)
	for _, result := range claim.Status.Allocation.Devices.Results {
		if _, exists := s.allocatable[result.Device]; !exists {
			return nil, fmt.Errorf("requested GPU is not allocatable: %v", result.Device)
		}
		for _, c := range slices.Backward(configs) {
			if len(c.Requests) == 0 || slices.Contains(c.Requests, result.Request) {
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
		}
	}

	// Normalize, validate, and apply all configs associated with devices that
	// need to be prepared. Track container edits generated from applying the
	// config to the set of device allocation results.
	perDeviceCDIContainerEdits := make(PerDeviceCDIContainerEdits)
	for c, results := range configResultsMap {
		// Cast the opaque config to a GpuConfig
		var config *configapi.GpuConfig
		switch castConfig := c.(type) {
		case *configapi.GpuConfig:
			config = castConfig
		default:
			return nil, fmt.Errorf("runtime object is not a regognized configuration")
		}

		// Normalize the config to set any implied defaults.
		if err := config.Normalize(); err != nil {
			return nil, fmt.Errorf("error normalizing GPU config: %w", err)
		}

		// Validate the config to ensure its integrity.
		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("error validating GPU config: %w", err)
		}

		// Apply the config to the list of results associated with it.
		containerEdits, err := s.applyConfig(config, results)
		if err != nil {
			return nil, fmt.Errorf("error applying GPU config: %w", err)
		}

		// Merge any new container edits with the overall per device map.
		for k, v := range containerEdits {
			perDeviceCDIContainerEdits[k] = v
		}
	}

	// Walk through each config and its associated device allocation results
	// and construct the list of prepared devices to return.
	var preparedDevices PreparedDevices
	for _, results := range configResultsMap {
		for _, result := range results {
			device := &PreparedDevice{
				Device: drapbv1.Device{
					RequestNames: []string{result.Request},
					PoolName:     result.Pool,
					DeviceName:   result.Device,
					CDIDeviceIDs: s.cdi.GetClaimDevices(string(claim.UID), []string{result.Device}),
				},
				ContainerEdits: perDeviceCDIContainerEdits[result.Device],
			}
			preparedDevices = append(preparedDevices, device)
		}
	}

	return preparedDevices, nil
}

func (s *DeviceState) unprepareDevices(claimUID string, devices PreparedDevices) error {
	return nil
}

// getDeviceAttrs gets the major, minor, type, and permissions for a given device path.
func (s *DeviceState) getDeviceAttrs(path string) (major, minor int64, devType, permissions string, err error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, 0, "", "", fmt.Errorf("failed to stat device %s: %w", path, err)
	}

	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, "", "", fmt.Errorf("failed to get syscall.Stat_t for %s", path)
	}

	// Use unix helpers to extract major and minor device numbers
	major = int64(unix.Major(uint64(stat.Rdev)))
	minor = int64(unix.Minor(uint64(stat.Rdev)))

	// Determine device type
	if (fileInfo.Mode() & os.ModeCharDevice) != 0 {
		devType = "c"
	} else if (fileInfo.Mode() & os.ModeDevice) != 0 {
		devType = "b"
	} else {
		return 0, 0, "", "", fmt.Errorf("unsupported file type for device %s: %v", path, fileInfo.Mode())
	}

	// Simplified permissions
	permissions = "rwm"

	return major, minor, devType, permissions, nil
}

// applyConfig applies a configuration to a set of device allocation results.
func (s *DeviceState) applyConfig(config *configapi.GpuConfig, results []*resourceapi.DeviceRequestAllocationResult) (PerDeviceCDIContainerEdits, error) {
	perDeviceEdits := make(PerDeviceCDIContainerEdits)

	for _, result := range results {
		klog.Infof("received allocation result: %+v", result)

		var card, renderD int
		var err error

		device, exists := s.allocatable[result.Device]
		if !exists {
			return nil, fmt.Errorf("requested device is not allocatable: %v", result.Device)
		}

		if device.SyntheticPartition != nil {
			// Synthetic-partition device: discover card/renderD by re-reading sysfs
			// after GPU has been partitioned by PreparePartition
			card, renderD, err = s.discoverPartitionDeviceNodes(result.Device)
			if err != nil {
				return nil, fmt.Errorf("error discovering device nodes for synthetic-partition device %s: %w", result.Device, err)
			}
		} else {
			card, renderD, err = parseDeviceName(result.Device)
			if err != nil {
				return nil, fmt.Errorf("error parsing device name %s: %w", result.Device, err)
			}
		}

		// TODO implement GPU sharing config when it is available
		//switch {
		//case config.Sharing.IsTimeSlicing():
		//	// TODO implement time slicing config when it is available
		//case config.Sharing.IsSpacePartitioning():
		//	// TODO implement space partitioning config when it is available
		//}

		edits, err := s.buildDeviceCDIEdits(card, renderD)
		if err != nil {
			return nil, fmt.Errorf("error building CDI edits for device %s (card=%d, renderD=%d): %w",
				result.Device, card, renderD, err)
		}

		perDeviceEdits[result.Device] = &cdiapi.ContainerEdits{ContainerEdits: edits}
	}

	return perDeviceEdits, nil
}

// buildDeviceCDIEdits creates CDI container edits for a specific card/renderD pair.
func (s *DeviceState) buildDeviceCDIEdits(card, renderD int) (*cdispec.ContainerEdits, error) {
	cardPath := fmt.Sprintf("/dev/dri/card%d", card)
	renderDPath := fmt.Sprintf("/dev/dri/renderD%d", renderD)
	kfdPath := "/dev/kfd"

	cardMajor, cardMinor, cardDevType, cardPermission, err := s.getDeviceAttrs(cardPath)
	if err != nil {
		return nil, fmt.Errorf("error getting device attrs for %s: %w", cardPath, err)
	}
	renderDMajor, renderDMinor, renderDDevType, renderDPermission, err := s.getDeviceAttrs(renderDPath)
	if err != nil {
		return nil, fmt.Errorf("error getting device attrs for %s: %w", renderDPath, err)
	}
	kfdMajor, kfdMinor, kfdDevType, kfdPermission, err := s.getDeviceAttrs(kfdPath)
	if err != nil {
		return nil, fmt.Errorf("error getting device attrs for %s: %w", kfdPath, err)
	}

	return &cdispec.ContainerEdits{
		DeviceNodes: []*cdispec.DeviceNode{
			{
				Path:        "/dev/kfd",
				HostPath:    "/dev/kfd",
				Type:        kfdDevType,
				Major:       kfdMajor,
				Minor:       kfdMinor,
				Permissions: kfdPermission,
			},
			{
				Path:        fmt.Sprintf("/dev/dri/card%d", card),
				HostPath:    fmt.Sprintf("/dev/dri/card%d", card),
				Type:        cardDevType,
				Major:       cardMajor,
				Minor:       cardMinor,
				Permissions: cardPermission,
			},
			{
				Path:        fmt.Sprintf("/dev/dri/renderD%d", renderD),
				HostPath:    fmt.Sprintf("/dev/dri/renderD%d", renderD),
				Type:        renderDDevType,
				Major:       renderDMajor,
				Minor:       renderDMinor,
				Permissions: renderDPermission,
			},
		},
	}, nil
}

// discoverPartitionDeviceNodes re-reads sysfs to find the card/renderD indices
// for a synthetic-partition device after GPU partitioning has been applied.
// For SPX (full GPU), it uses the parent GPU's card/renderD.
// For partitioned modes, it reads XCP platform devices from sysfs.
func (s *DeviceState) discoverPartitionDeviceNodes(deviceName string) (card, renderD int, err error) {
	gpuIndex, computeMode, _, err := parseSyntheticPartitionDeviceName(deviceName)
	if err != nil {
		return 0, 0, err
	}

	// Re-enumerate GPUs to find current device nodes after partitioning
	allGPUs := amdgpu.GetAMDGPUs()

	// Look up the PCI address for this GPU index
	var gpuPCIAddr string
	if s.partitionState != nil {
		var ok bool
		gpuPCIAddr, ok = s.partitionState.gpuPCIAddresses[gpuIndex]
		if !ok {
			return 0, 0, fmt.Errorf("PCI address for GPU index %d not found for partition device discovery", gpuIndex)
		}
	} else {
		return 0, 0, fmt.Errorf("partition state not available for partition device discovery")
	}

	if computeMode == consts.ComputePartitionSPX {
		// SPX mode - use the physical GPU's card/renderD directly
		for _, gpuInfoMap := range allGPUs {
			pciAddr := gpuInfoMap["pciAddr"].(string)
			cpt := gpuInfoMap["computePartitionType"].(string)
			if pciAddr == gpuPCIAddr && (cpt == consts.ComputePartitionSPX || cpt == "") {
				card = gpuInfoMap["card"].(int)
				renderD = gpuInfoMap["renderD"].(int)
				klog.Infof("Auto-partition SPX device %s mapped to card=%d renderD=%d", deviceName, card, renderD)
				return card, renderD, nil
			}
		}
		return 0, 0, fmt.Errorf("could not find SPX device nodes for GPU %d (PCI: %s)", gpuIndex, gpuPCIAddr)
	}

	// Collect partitions belonging to this GPU
	type partitionDev struct {
		card    int
		renderD int
	}
	var partitions []partitionDev

	for _, gpuInfoMap := range allGPUs {
		pciAddr := gpuInfoMap["pciAddr"].(string)
		cpt := gpuInfoMap["computePartitionType"].(string)
		if pciAddr == gpuPCIAddr && cpt != "" && cpt != consts.ComputePartitionSPX {
			partitions = append(partitions, partitionDev{
				card:    gpuInfoMap["card"].(int),
				renderD: gpuInfoMap["renderD"].(int),
			})
		}
	}

	if len(partitions) == 0 {
		return 0, 0, fmt.Errorf("no partition devices found for GPU %d (PCI: %s) after partitioning",
			gpuIndex, gpuPCIAddr)
	}

	// Sort partitions by card index for deterministic assignment
	sort.Slice(partitions, func(i, j int) bool {
		return partitions[i].card < partitions[j].card
	})

	// Use allocCount-1 as the partition index (allocCount was already incremented in PreparePartition)
	allocIndex := 0
	if s.partitionState != nil {
		s.partitionState.mu.Lock()
		allocCount := s.partitionState.gpuAllocCounts[gpuIndex]
		s.partitionState.mu.Unlock()
		if allocCount > 0 {
			allocIndex = allocCount - 1
		}
	}

	if allocIndex >= len(partitions) {
		return 0, 0, fmt.Errorf("allocation index %d exceeds available partitions %d for GPU %d",
			allocIndex, len(partitions), gpuIndex)
	}

	card = partitions[allocIndex].card
	renderD = partitions[allocIndex].renderD
	klog.Infof("Auto-partition device %s mapped to partition %d: card=%d renderD=%d",
		deviceName, allocIndex, card, renderD)

	return card, renderD, nil
}

// GetOpaqueDeviceConfigs returns an ordered list of the configs contained in possibleConfigs for this driver.
//
// Configs can either come from the resource claim itself or from the device
// class associated with the request. Configs coming directly from the resource
// claim take precedence over configs coming from the device class. Moreover,
// configs found later in the list of configs attached to its source take
// precedence over configs found earlier in the list for that source.
//
// All of the configs relevant to the driver from the list of possibleConfigs
// will be returned in order of precedence (from lowest to highest). If no
// configs are found, nil is returned.
func GetOpaqueDeviceConfigs(
	decoder runtime.Decoder,
	driverName string,
	possibleConfigs []resourceapi.DeviceAllocationConfiguration,
) ([]*OpaqueDeviceConfig, error) {
	// Collect all configs in order of reverse precedence.
	var classConfigs []resourceapi.DeviceAllocationConfiguration
	var claimConfigs []resourceapi.DeviceAllocationConfiguration
	var candidateConfigs []resourceapi.DeviceAllocationConfiguration
	for _, config := range possibleConfigs {
		switch config.Source {
		case resourceapi.AllocationConfigSourceClass:
			classConfigs = append(classConfigs, config)
		case resourceapi.AllocationConfigSourceClaim:
			claimConfigs = append(claimConfigs, config)
		default:
			return nil, fmt.Errorf("invalid config source: %v", config.Source)
		}
	}
	candidateConfigs = append(candidateConfigs, classConfigs...)
	candidateConfigs = append(candidateConfigs, claimConfigs...)

	// Decode all configs that are relevant for the driver.
	var resultConfigs []*OpaqueDeviceConfig
	for _, config := range candidateConfigs {
		// If this is nil, the driver doesn't support some future API extension
		// and needs to be updated.
		if config.DeviceConfiguration.Opaque == nil {
			return nil, fmt.Errorf("only opaque parameters are supported by this driver")
		}

		// Configs for different drivers may have been specified because a
		// single request can be satisfied by different drivers. This is not
		// an error -- drivers must skip over other driver's configs in order
		// to support this.
		if config.DeviceConfiguration.Opaque.Driver != driverName {
			continue
		}

		decodedConfig, err := runtime.Decode(decoder, config.DeviceConfiguration.Opaque.Parameters.Raw)
		if err != nil {
			return nil, fmt.Errorf("error decoding config parameters: %w", err)
		}

		resultConfig := &OpaqueDeviceConfig{
			Requests: config.Requests,
			Config:   decodedConfig,
		}

		resultConfigs = append(resultConfigs, resultConfig)
	}

	return resultConfigs, nil
}
