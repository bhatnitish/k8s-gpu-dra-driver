/*
Copyright (c) Advanced Micro Devices, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package amdsmi provides CGo bindings to libamd_smi for setting GPU
// compute and memory partition modes. It replaces the previous approach
// of shelling out to the amd-smi CLI binary.
package amdsmi

/*
#cgo CFLAGS: -I${SRCDIR}/../../build/assets/amd_smi
#cgo LDFLAGS: -L${SRCDIR}/../../build/assets -lamd_smi
#include "amdsmi.h"
*/
import "C"
import (
	"fmt"
	"strings"
	"sync"
	"time"

	klog "k8s.io/klog/v2"
)

const (
	// retryCount is the number of retry attempts for AMDSMI_STATUS_BUSY.
	retryCount = 3
	// retryBackoff is the delay between retries when GPU is busy.
	retryBackoff = 5 * time.Second

	// statusBusy maps to AMDSMI_STATUS_BUSY (30).
	statusBusy = 30
	// statusSettingUnavailable maps to AMDSMI_STATUS_SETTING_UNAVAILABLE (55).
	statusSettingUnavailable = 55
)

var (
	initMu     sync.Mutex
	initDone   bool
	gpuHandles []C.amdsmi_processor_handle // cached at Init() time
)

// Init initializes the AMD SMI library for GPU operations.
// It is idempotent: subsequent calls after the first successful init are no-ops.
func Init() error {
	initMu.Lock()
	defer initMu.Unlock()

	if initDone {
		return nil
	}

	ret := C.amdsmi_init(C.AMDSMI_INIT_AMD_GPUS)
	if ret != C.AMDSMI_STATUS_SUCCESS {
		return fmt.Errorf("amdsmi_init failed with status %d", int(ret))
	}

	// Enumerate and cache all GPU processor handles before any partitioning
	handles, err := enumerateGPUHandles()
	if err != nil {
		C.amdsmi_shut_down()
		return fmt.Errorf("failed to enumerate GPU handles: %v", err)
	}
	gpuHandles = handles

	initDone = true
	klog.Infof("AMD SMI initialized successfully, discovered %d GPUs", len(gpuHandles))
	return nil
}

// Shutdown shuts down the AMD SMI library.
// It is idempotent: calls when not initialized are no-ops.
func Shutdown() {
	initMu.Lock()
	defer initMu.Unlock()

	if !initDone {
		return
	}

	C.amdsmi_shut_down()
	gpuHandles = nil
	initDone = false
	klog.Infof("AMD SMI shut down successfully")
}

// enumerateGPUHandles enumerates all GPU processor handles via the AMD SMI
// socket/processor hierarchy. Called once during Init() to cache handles
// before any partitioning changes GPU indices.
func enumerateGPUHandles() ([]C.amdsmi_processor_handle, error) {
	// Step 1: get socket count
	var socketCount C.uint32_t
	ret := C.amdsmi_get_socket_handles(&socketCount, nil)
	if ret != C.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("amdsmi_get_socket_handles (count) failed with status %d", int(ret))
	}

	if socketCount == 0 {
		return nil, fmt.Errorf("no AMD SMI sockets found")
	}

	// Step 2: get socket handles
	sockets := make([]C.amdsmi_socket_handle, socketCount)
	ret = C.amdsmi_get_socket_handles(&socketCount, &sockets[0])
	if ret != C.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("amdsmi_get_socket_handles failed with status %d", int(ret))
	}

	// Step 3: iterate sockets and processors to collect all GPU handles
	var handles []C.amdsmi_processor_handle
	for i := 0; i < int(socketCount); i++ {
		// Get processor count for this socket
		var procCount C.uint32_t
		ret = C.amdsmi_get_processor_handles(sockets[i], &procCount, nil)
		if ret != C.AMDSMI_STATUS_SUCCESS {
			klog.Warningf("amdsmi_get_processor_handles (count) failed for socket %d with status %d, skipping", i, int(ret))
			continue
		}

		if procCount == 0 {
			continue
		}

		// Get processor handles
		processors := make([]C.amdsmi_processor_handle, procCount)
		ret = C.amdsmi_get_processor_handles(sockets[i], &procCount, &processors[0])
		if ret != C.AMDSMI_STATUS_SUCCESS {
			klog.Warningf("amdsmi_get_processor_handles failed for socket %d with status %d, skipping", i, int(ret))
			continue
		}

		for j := 0; j < int(procCount); j++ {
			var procType C.processor_type_t
			ret = C.amdsmi_get_processor_type(processors[j], &procType)
			if ret != C.AMDSMI_STATUS_SUCCESS {
				klog.Warningf("amdsmi_get_processor_type failed for socket %d processor %d with status %d, skipping", i, j, int(ret))
				continue
			}

			if procType != C.AMDSMI_PROCESSOR_TYPE_AMD_GPU {
				continue
			}

			handles = append(handles, processors[j])
		}
	}

	return handles, nil
}

// getProcessorHandle returns the cached processor handle for the GPU at the
// given index (0-based). Handles are cached at Init() time before any
// partitioning occurs, so indices remain stable.
func getProcessorHandle(gpuIndex int) (C.amdsmi_processor_handle, error) {
	if gpuIndex < 0 || gpuIndex >= len(gpuHandles) {
		return nil, fmt.Errorf("GPU index %d out of range (have %d GPUs)", gpuIndex, len(gpuHandles))
	}
	return gpuHandles[gpuIndex], nil
}

// computePartitionToC maps a lowercase compute partition mode string to the
// corresponding C enum value.
func computePartitionToC(mode string) (C.amdsmi_compute_partition_type_t, error) {
	switch strings.ToUpper(mode) {
	case "SPX":
		return C.AMDSMI_COMPUTE_PARTITION_SPX, nil
	case "DPX":
		return C.AMDSMI_COMPUTE_PARTITION_DPX, nil
	case "QPX":
		return C.AMDSMI_COMPUTE_PARTITION_QPX, nil
	case "CPX":
		return C.AMDSMI_COMPUTE_PARTITION_CPX, nil
	default:
		return C.AMDSMI_COMPUTE_PARTITION_SPX, fmt.Errorf("unknown compute partition mode: %q", mode)
	}
}

// memoryPartitionToC maps a lowercase memory partition mode string to the
// corresponding C enum value.
func memoryPartitionToC(mode string) (C.amdsmi_memory_partition_type_t, error) {
	switch strings.ToUpper(mode) {
	case "NPS1":
		return C.AMDSMI_MEMORY_PARTITION_NPS1, nil
	case "NPS2":
		return C.AMDSMI_MEMORY_PARTITION_NPS2, nil
	case "NPS4":
		return C.AMDSMI_MEMORY_PARTITION_NPS4, nil
	case "NPS8":
		return C.AMDSMI_MEMORY_PARTITION_NPS8, nil
	default:
		return C.AMDSMI_MEMORY_PARTITION_NPS1, fmt.Errorf("unknown memory partition mode: %q", mode)
	}
}

// SetComputePartition sets the compute partition mode on the GPU at the given
// index. The mode string should be one of: "spx", "dpx", "qpx", "cpx"
// (case-insensitive).
//
// On AMDSMI_STATUS_BUSY (30), retries up to 3 times with a 5-second backoff.
// On AMDSMI_STATUS_SETTING_UNAVAILABLE (55), returns an error immediately.
func SetComputePartition(gpuIndex int, mode string) error {
	handle, err := getProcessorHandle(gpuIndex)
	if err != nil {
		return fmt.Errorf("failed to get processor handle for GPU %d: %v", gpuIndex, err)
	}

	computeType, err := computePartitionToC(mode)
	if err != nil {
		return err
	}

	for attempt := 0; attempt <= retryCount; attempt++ {
		ret := C.amdsmi_set_gpu_compute_partition(handle, computeType)
		if ret == C.AMDSMI_STATUS_SUCCESS {
			klog.Infof("Successfully set compute partition mode %q on GPU %d", strings.ToUpper(mode), gpuIndex)
			return nil
		}

		retCode := int(ret)
		if retCode == statusSettingUnavailable {
			return fmt.Errorf("compute partition mode %q is not available on GPU %d (AMDSMI_STATUS_SETTING_UNAVAILABLE)", mode, gpuIndex)
		}

		if retCode == statusBusy && attempt < retryCount {
			klog.Warningf("GPU %d is busy (AMDSMI_STATUS_BUSY), retrying in %v (attempt %d/%d)",
				gpuIndex, retryBackoff, attempt+1, retryCount)
			time.Sleep(retryBackoff)
			continue
		}

		return fmt.Errorf("amdsmi_set_gpu_compute_partition failed on GPU %d with status %d", gpuIndex, retCode)
	}

	return fmt.Errorf("amdsmi_set_gpu_compute_partition failed on GPU %d after %d retries (GPU busy)", gpuIndex, retryCount)
}

// SetMemoryPartition sets the memory partition mode on the GPU at the given
// index. The mode string should be one of: "nps1", "nps2", "nps4", "nps8"
// (case-insensitive).
//
// After a successful set, calls amdsmi_gpu_driver_reload() to reload the
// AMD GPU driver (required since ROCm 7.0.0).
//
// On AMDSMI_STATUS_BUSY (30), retries up to 3 times with a 5-second backoff.
// On AMDSMI_STATUS_SETTING_UNAVAILABLE (55), returns an error immediately.
func SetMemoryPartition(gpuIndex int, mode string) error {
	handle, err := getProcessorHandle(gpuIndex)
	if err != nil {
		return fmt.Errorf("failed to get processor handle for GPU %d: %v", gpuIndex, err)
	}

	memType, err := memoryPartitionToC(mode)
	if err != nil {
		return err
	}

	for attempt := 0; attempt <= retryCount; attempt++ {
		ret := C.amdsmi_set_gpu_memory_partition(handle, memType)
		if ret == C.AMDSMI_STATUS_SUCCESS {
			klog.Infof("Successfully set memory partition mode %q on GPU %d, reloading driver", strings.ToUpper(mode), gpuIndex)

			// Behavior change from ROCm 7.0.0: amdsmi_set_gpu_memory_partition
			// no longer reloads the driver automatically. Must call reload separately.
			retReload := C.amdsmi_gpu_driver_reload()
			if retReload != C.AMDSMI_STATUS_SUCCESS {
				return fmt.Errorf("amdsmi_gpu_driver_reload failed with status %d after setting memory partition on GPU %d", int(retReload), gpuIndex)
			}
			klog.Infof("Driver reloaded successfully after memory partition change on GPU %d", gpuIndex)
			return nil
		}

		retCode := int(ret)
		if retCode == statusSettingUnavailable {
			return fmt.Errorf("memory partition mode %q is not available on GPU %d (AMDSMI_STATUS_SETTING_UNAVAILABLE)", mode, gpuIndex)
		}

		if retCode == statusBusy && attempt < retryCount {
			klog.Warningf("GPU %d is busy (AMDSMI_STATUS_BUSY), retrying in %v (attempt %d/%d)",
				gpuIndex, retryBackoff, attempt+1, retryCount)
			time.Sleep(retryBackoff)
			continue
		}

		return fmt.Errorf("amdsmi_set_gpu_memory_partition failed on GPU %d with status %d", gpuIndex, retCode)
	}

	return fmt.Errorf("amdsmi_set_gpu_memory_partition failed on GPU %d after %d retries (GPU busy)", gpuIndex, retryCount)
}
