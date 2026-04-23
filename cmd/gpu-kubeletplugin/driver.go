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
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/ROCm/k8s-gpu-dra-driver/pkg/amdsmi"
	"github.com/ROCm/k8s-gpu-dra-driver/pkg/consts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	klog "k8s.io/klog/v2"
)

type driver struct {
	client                    coreclientset.Interface
	helper                    *kubeletplugin.Helper
	state                     *DeviceState
	healthcheck               *healthcheck
	cancelCtx                 func(error)
	enableSyntheticPartition  bool
	nodeName                  string
	partitionableGPUs         []int
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	driver := &driver{
		client:                   config.coreclient,
		cancelCtx:                config.cancelMainCtx,
		enableSyntheticPartition: config.flags.enableSyntheticPartition,
		nodeName:                 config.flags.nodeName,
	}

	// Initialize AMD SMI if auto-partition is enabled
	if config.flags.enableSyntheticPartition {
		if err := amdsmi.Init(); err != nil {
			return nil, fmt.Errorf("failed to initialize AMD SMI for auto-partition: %v", err)
		}
		klog.Infof("Auto-partition mode enabled, AMD SMI initialized")
	}

	state, err := NewDeviceState(config)
	if err != nil {
		return nil, err
	}
	driver.state = state
	driver.partitionableGPUs = state.partitionableGPUs
	state.driver = driver

	helper, err := kubeletplugin.Start(
		ctx,
		driver,
		kubeletplugin.KubeClient(config.coreclient),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(consts.DriverName),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
	)
	if err != nil {
		return nil, err
	}
	driver.helper = helper

	var resources resourceslice.DriverResources
	if config.flags.enableSyntheticPartition {
		resources = driver.buildSyntheticPartitionResources(config.flags.nodeName)
	} else {
		devices := make([]resourceapi.Device, 0, len(state.allocatable))
		for device := range maps.Values(state.allocatable) {
			devices = append(devices, device.GetDevice())
		}
		resources = resourceslice.DriverResources{
			Pools: map[string]resourceslice.Pool{
				config.flags.nodeName: {
					Slices: []resourceslice.Slice{
						{
							Devices: devices,
						},
					},
				},
			},
		}
	}

	if resourcesJSON, err := json.MarshalIndent(resources, "", "  "); err != nil {
		klog.Warningf("Failed to marshal ResourceSlice to JSON: %v", err)
	} else {
		klog.Infof("Publishing ResourceSlice:\n%s", string(resourcesJSON))
	}

	driver.healthcheck, err = startHealthcheck(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}

	if err := helper.PublishResources(ctx, resources); err != nil {
		return nil, err
	}

	return driver, nil
}

// buildSyntheticPartitionResources builds the DriverResources for auto-partition mode.
// It includes mutex counter sets for each partitionable GPU so that only one
// partition config can be allocated per physical GPU at a time.
func (d *driver) buildSyntheticPartitionResources(nodeName string) resourceslice.DriverResources {
	devices := make([]resourceapi.Device, 0, len(d.state.allocatable))
	for device := range maps.Values(d.state.allocatable) {
		devices = append(devices, device.GetDevice())
	}

	// Build counter sets for mutual exclusion on partitionable GPUs.
	// Each partitionable GPU gets a counter set with a single "gpuSlots" counter
	// set to 1, so only one synthetic partition device per GPU can be allocated.
	var counterSets []resourceapi.CounterSet
	for _, gpuIdx := range d.partitionableGPUs {
		counterSetName := fmt.Sprintf("gpu-%d-mutex", gpuIdx)
		counterSets = append(counterSets, resourceapi.CounterSet{
			Name: counterSetName,
			Counters: map[string]resourceapi.Counter{
				"gpuSlots": {
					Value: *resource.NewQuantity(1, resource.DecimalSI),
				},
			},
		})
	}

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {
				Slices: []resourceslice.Slice{
					{
						Devices:        devices,
						SharedCounters: counterSets,
					},
				},
			},
		},
	}
}

// republishResources rebuilds and republishes the ResourceSlice. This is called
// after partition state changes to update taints or device availability.
func (d *driver) republishResources(ctx context.Context) error {
	var resources resourceslice.DriverResources
	if d.enableSyntheticPartition {
		resources = d.buildSyntheticPartitionResources(d.nodeName)
	} else {
		devices := make([]resourceapi.Device, 0, len(d.state.allocatable))
		for device := range maps.Values(d.state.allocatable) {
			devices = append(devices, device.GetDevice())
		}
		resources = resourceslice.DriverResources{
			Pools: map[string]resourceslice.Pool{
				d.nodeName: {
					Slices: []resourceslice.Slice{
						{
							Devices: devices,
						},
					},
				},
			},
		}
	}

	if resourcesJSON, err := json.MarshalIndent(resources, "", "  "); err != nil {
		klog.Warningf("Failed to marshal ResourceSlice to JSON: %v", err)
	} else {
		klog.Infof("Republishing ResourceSlice:\n%s", string(resourcesJSON))
	}

	return d.helper.PublishResources(ctx, resources)
}

func (d *driver) Shutdown(logger klog.Logger) error {
	if d.healthcheck != nil {
		d.healthcheck.Stop(logger)
	}
	d.helper.Stop()
	if d.enableSyntheticPartition {
		amdsmi.Shutdown()
		logger.Info("AMD SMI shut down")
	}
	return nil
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.Infof("PrepareResourceClaims is called: number of claims: %d", len(claims))
	result := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		result[claim.UID] = d.prepareResourceClaim(ctx, claim)
	}

	return result, nil
}

func (d *driver) prepareResourceClaim(_ context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	preparedPBs, err := d.state.Prepare(claim)
	if err != nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
		}
	}
	var prepared []kubeletplugin.Device
	for _, preparedPB := range preparedPBs {
		prepared = append(prepared, kubeletplugin.Device{
			Requests:     preparedPB.GetRequestNames(),
			PoolName:     preparedPB.GetPoolName(),
			DeviceName:   preparedPB.GetDeviceName(),
			CDIDeviceIDs: preparedPB.GetCdiDeviceIds(),
		})
	}

	klog.Infof("Returning newly prepared devices for claim '%v': %v", claim.UID, prepared)
	return kubeletplugin.PrepareResult{Devices: prepared}
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.Infof("UnprepareResourceClaims is called: number of claims: %d", len(claims))
	result := make(map[types.UID]error)

	for _, claim := range claims {
		result[claim.UID] = d.unprepareResourceClaim(ctx, claim)
	}

	return result, nil
}

func (d *driver) unprepareResourceClaim(_ context.Context, claim kubeletplugin.NamespacedObject) error {
	if err := d.state.Unprepare(string(claim.UID)); err != nil {
		return fmt.Errorf("error unpreparing devices for claim %v: %w", claim.UID, err)
	}

	return nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) && d.cancelCtx != nil {
		d.cancelCtx(fmt.Errorf("fatal background error: %w", err))
	}
}
