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
	"errors"
	"fmt"
	"maps"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	klog "k8s.io/klog/v2"

	"github.com/ROCm/k8s-gpu-dra-driver/pkg/amdsmi"
	"github.com/ROCm/k8s-gpu-dra-driver/pkg/consts"
)

type driver struct {
	client                   coreclientset.Interface
	helper                   *kubeletplugin.Helper
	state                    *DeviceState
	healthcheck              *healthcheck
	cancelCtx                func(error)
	enableSyntheticPartition bool
	nodeName                 string
	partitionableGPUs        []int
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	d := &driver{
		client:                   config.coreclient,
		cancelCtx:                config.cancelMainCtx,
		enableSyntheticPartition: config.flags.enableSyntheticPartition,
		nodeName:                 config.flags.nodeName,
	}

	state, err := NewDeviceState(config)
	if err != nil {
		return nil, err
	}
	d.state = state

	// Copy partitionable GPU indices from partition state for counter set building
	if state.partitionState != nil {
		d.partitionableGPUs = state.partitionState.partitionableGPUs
	}

	// Initialize AMD SMI library for GPU partition operations
	if config.flags.enableSyntheticPartition {
		if err := amdsmi.Init(); err != nil {
			return nil, fmt.Errorf("failed to initialize AMD SMI: %v", err)
		}
	}

	helper, err := kubeletplugin.Start(
		ctx,
		d,
		kubeletplugin.KubeClient(config.coreclient),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(consts.DriverName),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
	)
	if err != nil {
		return nil, err
	}
	d.helper = helper
	// Store helper reference in state for re-publishing from Prepare/Unprepare
	d.state.driver = d

	var resources resourceslice.DriverResources

	if config.flags.enableSyntheticPartition && d.state.partitionState != nil {
		// Synthetic-partition mode: build two slices (shared counters + devices)
		resources = d.buildSyntheticPartitionResources()
	} else {
		// Standard mode: single slice with all devices
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

	d.healthcheck, err = startHealthcheck(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}

	if err := helper.PublishResources(ctx, resources); err != nil {
		return nil, err
	}

	return d, nil
}

// buildSyntheticPartitionResources builds DriverResources for synthetic-partition mode.
// Counter sets and devices are placed in separate slices within the same pool.
// The API requires that a ResourceSlice contains either sharedCounters or devices, not both.
func (d *driver) buildSyntheticPartitionResources() resourceslice.DriverResources {
	// Build counter sets for partitionable GPUs
	counterSets := make([]resourceapi.CounterSet, 0, len(d.partitionableGPUs))
	for _, gpuIndex := range d.partitionableGPUs {
		counterSets = append(counterSets, buildMutexCounterSet(gpuIndex))
	}

	// Build device list
	devices := make([]resourceapi.Device, 0, len(d.state.allocatable))
	for device := range maps.Values(d.state.allocatable) {
		devices = append(devices, device.GetDevice())
	}

	// Use separate slices: one for shared counters, one for devices.
	slices := []resourceslice.Slice{
		{Devices: devices},
	}
	if len(counterSets) > 0 {
		slices = append(slices, resourceslice.Slice{
			SharedCounters: counterSets,
		})
	}

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			d.nodeName: {
				Slices: slices,
			},
		},
	}
}

// republishResources re-publishes ResourceSlices with updated taints.
// Called from Prepare (to add memory partition conflict taints) and
// Unprepare (to remove taints when all allocations are released).
func (d *driver) republishResources(ctx context.Context) error {
	if !d.enableSyntheticPartition || d.state.partitionState == nil {
		return nil
	}

	resources := d.buildSyntheticPartitionResources()
	if err := d.helper.PublishResources(ctx, resources); err != nil {
		return fmt.Errorf("error re-publishing resources: %v", err)
	}
	klog.Infof("Re-published ResourceSlices with updated taints")
	return nil
}

func (d *driver) Shutdown(logger klog.Logger) error {
	if d.healthcheck != nil {
		d.healthcheck.Stop(logger)
	}
	if d.enableSyntheticPartition {
		amdsmi.Shutdown()
	}
	d.helper.Stop()
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
			CDIDeviceIDs: preparedPB.GetCDIDeviceIDs(),
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
