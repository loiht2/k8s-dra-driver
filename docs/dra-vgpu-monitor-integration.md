# DRA vGPUMonitor Integration

## Problem

HAMi provides real-time GPU memory monitoring for containers via the **vGPUMonitor** sidecar and the `Device_memory_desc_of_container` Prometheus metric. However, this monitoring only worked for the traditional **device-plugin (vGPU)** path, not for containers allocated through **Dynamic Resource Allocation (DRA)**.

Two issues prevented monitoring from working with DRA:

1. **Cache directory mismatch**: The DRA driver created HAMi-core cache files under `claims/{claimUID}/`, but the vGPUMonitor's `ContainerLister` only scans `containers/{podUID}_{containerName}/` directories. The monitor never discovered DRA containers.

2. **vGPUMonitor not deployed with DRA**: The entire nvidia DaemonSet (including the vGPUMonitor sidecar) is gated by `(not .Values.dra.enabled)` in the Helm chart. When DRA is enabled, no monitor runs at all.

## Solution

### 1. Cache Directory Alignment (hami_core.go, device_state.go)

Changed the DRA driver to create cache directories using the **same naming convention** as the traditional device plugin:

```
{hostHookPath}/vgpu/containers/{podUID}_{containerName}/
```

Instead of the previous:

```
{hostHookPath}/vgpu/claims/{claimUID}/
```

This makes the existing `ContainerLister` (in `pkg/monitor/nvidia/cudevshr.go`) able to discover and report metrics for DRA-allocated containers with **zero changes to the monitor code**.

#### How pod/container info is resolved

The DRA `NodePrepareResources` call receives the full `ResourceClaim` object. A new method `resolveContainerInfo()` extracts the pod UID and container name by:

1. Reading `claim.Status.ReservedFor[0]` to get the consuming pod's UID and name
2. Fetching the pod via the Kubernetes API
3. Walking `pod.Spec.ResourceClaims` to find which pod-level claim reference matches this `ResourceClaim`
4. Walking `pod.Spec.Containers[].Resources.Claims` to find which container uses that claim reference

If resolution fails (e.g., the pod hasn't been created yet, or a shared claim with ambiguous ownership), the driver falls back to a claim-based directory name. The monitor won't parse the fallback format as `{podUID}_{containerName}`, but the cache files remain functional for HAMi-core's GPU memory limiting.

#### Checkpoint changes

The `DeviceConfigState` struct (checkpointed to survive restarts) now includes a `CacheDir` field that stores the exact host path created during `Prepare()`. The `Cleanup()` function uses this stored path during `Unprepare()` rather than deriving it from the claim UID, ensuring correct cleanup regardless of which naming scheme was used.

### 2. vGPUMonitor Sidecar in DRA DaemonSet (demo/yaml/ds.yaml)

Added `vgpu-monitor` as a sidecar container in the DRA DaemonSet with:

- Command: `vGPUmonitor -v=4`
- Environment variables: `NODE_NAME`, `NVIDIA_VISIBLE_DEVICES`, `NVIDIA_MIG_MONITOR_DEVICES`, `HOOK_PATH=/usr/local/vgpu`
- Volume mounts for: containers directory, /tmp, /run/docker, /run/containerd, /sys, /var

### 3. RBAC for Pod Access (demo/yaml/rbac.yaml)

Added `pods` (get, list, watch, update, patch) to the DRA driver's ClusterRole. The vGPUMonitor needs pod access to correlate container cache files with running pods and emit per-pod/per-container Prometheus metrics.

### 4. Helm Chart Condition Fix (charts/hami/templates/device-plugin/)

Relaxed the rendering condition on the following templates from:

```
{{- if and .Values.devicePlugin.enabled (not .Values.dra.enabled) -}}
```

To:

```
{{- if or (and .Values.devicePlugin.enabled (not .Values.dra.enabled)) .Values.dra.enabled -}}
```

Affected templates:
- `monitorserviceaccount.yaml`
- `monitorrole.yaml`
- `monitorrolebinding.yaml`
- `monitorservice.yaml`
- `servicemonitor.yaml`

This ensures the monitor's ServiceAccount, RBAC, Service, and Prometheus ServiceMonitor are deployed regardless of whether the traditional device plugin or DRA path is active.

## Files Changed

### k8s-dra-driver/

| File | Change |
|------|--------|
| `cmd/hami-kubelet-plugin/hami_core.go` | Added `resolveContainerInfo()`, changed `GetCDIContainerEdits()` to create `containers/{podUID}_{ctrName}/` dirs, changed `Cleanup()` to use stored cache dir, added K8s clientset to `HAMiCoreManager` |
| `cmd/hami-kubelet-plugin/device_state.go` | Added `CacheDir` to `DeviceConfigState`, updated `NewDeviceState` / `applySharingConfig` / `unprepareDevices` call signatures |
| `demo/yaml/ds.yaml` | Added `vgpu-monitor` sidecar container and associated host volumes |
| `demo/yaml/rbac.yaml` | Added pods RBAC permissions to ClusterRole |

### charts/hami/ (parent HAMi repo)

| File | Change |
|------|--------|
| `templates/device-plugin/monitorserviceaccount.yaml` | Relaxed condition to include DRA mode |
| `templates/device-plugin/monitorrole.yaml` | Relaxed condition to include DRA mode |
| `templates/device-plugin/monitorrolebinding.yaml` | Relaxed condition to include DRA mode |
| `templates/device-plugin/monitorservice.yaml` | Relaxed condition to include DRA mode |
| `templates/device-plugin/servicemonitor.yaml` | Relaxed condition to include DRA mode |

## Data Flow

```
Pod with DRA ResourceClaim scheduled to node
         │
         ▼
Kubelet calls PrepareResourceClaims()
         │
         ▼
DRA driver resolveContainerInfo():
  claim.Status.ReservedFor → pod UID
  K8s API → pod spec → container name
         │
         ▼
Creates /usr/local/vgpu/containers/{podUID}_{containerName}/
  └─ {uuid}.cache  (shared-memory mapped by HAMi-core)
         │
         ▼
CDI spec sets CUDA_DEVICE_MEMORY_SHARED_CACHE → cache file path
Container starts with HAMi-core writing GPU usage to .cache
         │
         ▼
vGPUMonitor (ContainerLister) scans containers/ directory
  ├─ Parses {podUID}_{containerName} from dir name
  ├─ Mmaps .cache file → UsageInfo
  └─ Emits Prometheus metrics:
       ├─ Device_memory_desc_of_container
       ├─ vGPU_device_memory_usage_in_bytes
       ├─ vGPU_device_memory_limit_in_bytes
       ├─ vGPU_device_memory_context_size_bytes
       ├─ vGPU_device_memory_module_size_bytes
       └─ vGPU_device_memory_buffer_size_bytes
```

## Known Limitations

- **Shared claims**: If a single `ResourceClaim` is reserved by multiple pods (via `AllowMultipleAllocations`), only the first consumer's pod/container is resolved. All sharing containers get the same CDI mounts. The monitor attributes all usage to one container. This is an uncommon pattern for GPU workloads but can be addressed if needed.

- **Fallback directory**: If the pod cannot be looked up (e.g., API server unreachable during `Prepare`), the directory name won't match the `{podUID}_{containerName}` convention and the monitor will skip it. HAMi-core GPU limiting still works; only monitoring is degraded.
