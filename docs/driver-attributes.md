# AMD GPU DRA Driver — Device Attributes and Capabilities

This document summarizes what the AMD GPU DRA driver exposes today through
Kubernetes Dynamic Resource Allocation (DRA) ResourceSlices and how to
interpret those attributes when selecting devices.

The driver discovers AMD GPUs present on a node and advertises them as DRA
Devices. It supports:
- Full, unpartitioned GPUs
- Pre-partitioned devices (for platforms that expose partitions)
- Auto-partition mode (virtual partition devices created on-demand)

Device selection can then use DRA attributes to target either full GPUs or
partitions.

## Device identity and naming

- Full GPU / partition canonical name: `gpu-<cardIndex>-<renderIndex>`
- Auto-partition canonical name: `gpu-<gpuIndex>-<computeMode>-<memoryMode>`
  (e.g., `gpu-0-cpx-nps4`)

## Device types (full GPU vs partition)

The driver distinguishes full GPUs from partitions via the `type` attribute:
- Full GPU: `type = amdgpu`
- Partition: `type = amdgpu-partition`

You can use this attribute in a claim’s `DeviceSelector` to select only
full GPUs or only partitions.

## Attributes for a full GPU

The following attributes are attached to each full GPU device:
- `type` (string): `amdgpu`
- `pciAddr` (string): PCI bus address of the device
- `cardIndex` (int): DRM card index (e.g., `card0` → 0)
- `renderIndex` (int): DRM render node index (e.g., `renderD128` → 128)
- `deviceID` (string): PCI device identifier (from sysfs)
- `family` (string): AMD GPU family string
- `productName` (string): Product name (normalized)
- `driverVersion` (semver): Kernel driver version
- `driverSrcVersion` (string): Kernel driver source version hash
- `partitionProfile` (string): For platforms that support partitioning, the
  current compute+memory profile (e.g., `spx_<mem>`); may be empty on devices
  that do not use partitioning
- `numaNode` (int): NUMA node the GPU is attached to (read from sysfs)
- Topology attribute: a PCIe root attribute is included when
  derivable; its qualified name and value come from the Kubernetes
  `deviceattribute` library and can be used by schedulers/topology-aware logic

Capacity values for full GPUs:
- `memory` (quantity, bytes): Advertised VRAM size; if the underlying topology
  inspection cannot determine VRAM precisely, a conservative default is used
- `computeUnits` (quantity): Number of compute units (CUs)
- `simdUnits` (quantity): Number of SIMD units

## Attributes for a partition

The following attributes are attached to each GPU partition device:
- `type` (string): `amdgpu-partition`
- `parentPciAddr` (string): PCI address of the parent GPU
- `cardIndex` (int): partition’s DRM card index
- `renderIndex` (int): partition’s DRM render node index
- `parentDeviceID` (string): parent GPU PCI device ID
- Note: `parentDeviceID` is identical for all partitions that belong to the
  same physical GPU. You can leverage this to target multiple partitions from
  the same parent device when co-location is desirable for performance or
  topology reasons.
- `family` (string): parent GPU family
- `productName` (string): parent product name
- `driverVersion` (semver): inherited from parent
- `driverSrcVersion` (string): inherited from parent
- `partitionProfile` (string): compute+memory profile of the partition
- `numaNode` (int): NUMA node inherited from the parent GPU
- Optional topology attribute: the parent’s PCIe root attribute is propagated

Capacity values for partitions:
- `memory` (quantity, bytes): VRAM capacity attributed to the partition; may
  use a conservative default when the exact value isn’t available
- `computeUnits` (quantity): number of CUs attributed to the partition
- `simdUnits` (quantity): number of SIMD units attributed to the partition

## How to select full GPUs vs partitions in claims

Use the `type` attribute selector in your ResourceClass/Claim to differentiate.
Examples (simplified):

Select only full GPUs:
```yaml
spec:
  devices:
    requests:
    - name: gpu
      deviceClassName: gpu.amd.com
      selectors:
        - cel:
            expression: 'device.attributes["gpu.amd.com"].type == "amdgpu"'
```

Select only partitions:
```yaml
spec:
  devices:
    requests:
    - name: gpu
      deviceClassName: gpu.amd.com
      selectors:
        - cel:
            expression: 'device.attributes["gpu.amd.com"].type == "amdgpu-partition"'
```

You may also combine with other attributes (e.g., `memory`, `family`,
`productName`, or the PCIe topology attribute) depending on scheduling needs.

### Request multiple partitions from the same parent GPU

To ensure two (or more) partitions come from the SAME physical GPU, use
`constraints.matchAttribute: parentDeviceID` across multiple named requests.
Each request selects a single partition, and the constraint enforces that the
`parentDeviceID` matches across those requests:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: two-partitions-same-parent
spec:
  devices:
    requests:
    - name: p0
      exactly:
        deviceClassName: gpu.amd.com
        allocationMode: ExactCount
        count: 1
        selectors:
          - cel:
              expression: 'device.attributes["gpu.amd.com"].type == "amdgpu-partition"'
    - name: p1
      exactly:
        deviceClassName: gpu.amd.com
        allocationMode: ExactCount
        count: 1
        selectors:
          - cel:
              expression: 'device.attributes["gpu.amd.com"].type == "amdgpu-partition"'
    constraints:
    - matchAttribute: gpu.amd.com/parentDeviceID
      requests: ["p0", "p1"]
```

Notes:
- This does not require hard-coding a specific `parentDeviceID`; the scheduler
  will choose a parent that has enough partitions to satisfy all listed
  requests where possible.
- If you instead want partitions from DIFFERENT parents, use
  `constraints.distinctAttribute: parentDeviceID` across the requests.

## NUMA-aware GPU scheduling

The `numaNode` attribute reports the NUMA node each GPU is attached to. Use it
to co-locate GPUs on the same NUMA node and reduce memory-access latency for
CPU–GPU workloads.

The recommended pattern is `constraints.matchAttribute` — the scheduler picks
any NUMA node but guarantees every matched request lands on the same one:

```yaml
spec:
  devices:
    requests:
      - name: g0
        deviceClassName: gpu.amd.com
      - name: g1
        deviceClassName: gpu.amd.com
      - name: g2
        deviceClassName: gpu.amd.com
      - name: g3
        deviceClassName: gpu.amd.com
    constraints:
      - matchAttribute: gpu.amd.com/numaNode
        requests: ["g0", "g1", "g2", "g3"]
```

See `example/example-numa-aligned-gpus.yaml` for a complete working example
that uses this pattern to run two tensor-parallel vLLM replicas, each pinned to
a single NUMA node.

If you need GPUs from a *specific* NUMA node, add a CEL selector instead:

```yaml
selectors:
  - cel:
      expression: 'device.attributes["gpu.amd.com"].numaNode == 0'
```

## Auto-partition mode (virtual partition devices) [Beta]

When auto-partition is enabled (`--enable-auto-partition` flag or
`ENABLE_AUTO_PARTITION=true` environment variable), the driver advertises
virtual partition devices for every valid compute+memory partition combination
on each partitionable GPU. Non-partitionable GPUs are still advertised as
normal full GPU devices.

This mode requires Kubernetes 1.36+ with DRA beta features enabled.

### How it works

1. On startup the driver discovers physical GPUs.
2. For each GPU that supports compute partitioning, it generates one virtual
   device per valid compute+memory combination:
   - `spx-nps1` -- full GPU (1 partition)
   - `dpx-nps2` -- dual partition (2 partitions)
   - `cpx-nps1` -- 8-way compute partition, single memory domain
   - `cpx-nps4` -- 8-way compute partition, 4-way memory domain
3. When a ResourceClaim is allocated and prepared, the driver calls `amd-smi`
   to set the requested compute and memory partition modes on the physical GPU.
4. Per-GPU shared counters (mutex) prevent conflicting partition modes from
   being allocated on the same GPU simultaneously.
5. Device taints (`NoExecute`) are applied to devices whose memory mode
   conflicts with the currently active memory mode on the node. Taints are
   removed when all allocations are released.

### Auto-partition device naming

Virtual devices are named `gpu-<gpuIndex>-<computeMode>-<memoryMode>`, for
example `gpu-0-cpx-nps4` or `gpu-1-spx-nps1`.

### Device types in auto-partition mode

The `type` attribute on auto-partition devices uses the same values as normal
devices to maintain compatibility with existing selectors:
- `type = amdgpu` for SPX mode (full GPU, 1 partition)
- `type = amdgpu-partition` for DPX/CPX modes (partitioned)

### Attributes for an auto-partition device

- `type` (string): `amdgpu` or `amdgpu-partition` (see above)
- `computePartition` (string): compute partition mode -- one of `spx`, `dpx`,
  `cpx`
- `memoryPartition` (string): memory partition mode -- one of `nps1`, `nps2`,
  `nps4`
- `gpuIndex` (int): physical GPU index on the node (0-based, sorted by PCI
  address)
- `pciAddr` (string): PCI bus address of the physical GPU
- `productName` (string): product name of the physical GPU
- `family` (string): AMD GPU family string
- `driverVersion` (semver): kernel driver version
- `driverSrcVersion` (string): kernel driver source version hash
- `numaNode` (int): NUMA node the GPU is attached to
- Topology attribute: PCIe root attribute (when derivable)

### Capacity values for auto-partition devices

- `partitions` (quantity): number of partitions this configuration creates
  (1 for SPX, 2 for DPX, 8 for CPX). This is a consumable capacity --
  multiple allocations can share the same partition configuration up to this
  count. A default request of 1 is applied when no explicit request is made.
- `memory` (quantity, bytes): per-partition VRAM (total GPU VRAM divided by
  partition count)
- `computeUnits` (quantity): per-partition CUs
- `simdUnits` (quantity): per-partition SIMD units

### Shared counters

Each partitionable GPU has a shared counter set named `gpu-<gpuIndex>-mutex`
with a single counter `partition-mode` of capacity 1. Every virtual partition
device for that GPU consumes this counter, so the scheduler ensures only one
partition configuration can be active per GPU at a time.

### Memory partition taints

When the first partition allocation on a node sets the memory mode (e.g.,
`nps4`), all virtual partition devices with a different memory mode receive a
`NoExecute` taint with key `gpu.amd.com/memory-partition-conflict`. This
prevents the scheduler from allocating incompatible memory modes. When all
partition allocations are released, taints are removed and any memory mode
becomes available again.

### Enabling auto-partition via Helm

Set `kubeletPlugin.enableAutoPartition` to `true` in your Helm values:

```yaml
kubeletPlugin:
  enableAutoPartition: true
```

Or via command line:
```bash
helm install amd-gpu-driver ./helm-charts-k8s \
  --set kubeletPlugin.enableAutoPartition=true
```

### Selecting auto-partition devices in claims

Request a specific partition configuration using `computePartition` and
`memoryPartition` attribute selectors:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: cpx-partition
spec:
  devices:
    requests:
    - name: gpu
      deviceClassName: gpu.amd.com
      selectors:
        - cel:
            expression: 'device.attributes["gpu.amd.com"].computePartition == "cpx"'
        - cel:
            expression: 'device.attributes["gpu.amd.com"].memoryPartition == "nps4"'
```

Request a full GPU (SPX mode):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: full-gpu
spec:
  devices:
    requests:
    - name: gpu
      deviceClassName: gpu.amd.com
      selectors:
        - cel:
            expression: 'device.attributes["gpu.amd.com"].computePartition == "spx"'
```

## Current capabilities and notes

- Discovery: the driver walks the relevant sysfs paths to find AMD GPUs and
  (when present) additional exposed partitions (e.g., on platforms that publish
  partition nodes). It correlates DRM indices and KFD topology to enrich device
  information (family, VRAM, SIMD/CU counts).
- Pre-partitioned devices: supported and reported as distinct DRA Devices with
  their own identity and capacities, linked back to the parent GPU via
  attributes such as `parentPciAddr` and `parentDeviceID`.
- Auto-partition mode: when enabled, the driver advertises virtual partition
  devices for all valid compute+memory combinations and dynamically partitions
  GPUs at claim-prepare time via `amd-smi`. Shared counters and device taints
  enforce partition mode exclusivity. Requires Kubernetes 1.36+.
- Topology hinting: a PCIe root attribute is added when derivable, enabling
  topology-aware scheduling.
- NUMA node discovery: the driver reads the NUMA node for each GPU from sysfs
  and exposes it as an integer attribute for NUMA-aware scheduling.
- Defaults: when certain metrics (like VRAM) cannot be read reliably, the
  driver falls back to conservative defaults to remain usable. These values can
  differ from the exact hardware amounts and are best used as coarse selectors.

If you need additional attributes or different representations, please open an
issue discussing your use case.
