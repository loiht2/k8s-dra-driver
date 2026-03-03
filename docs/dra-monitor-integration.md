# DRA Monitor Integration

## Overview

This document describes how the **k8s-dra-driver** integrates with the **HAMi-DRA-monitor** (a separate component from the [HAMi-DRA](https://github.com/loiht2/HAMi-DRA) project) to provide real-time GPU memory monitoring for DRA-allocated containers.

> **Important:** The monitor is **not** a sidecar within the DRA DaemonSet. It is deployed as a **separate DaemonSet** via the HAMi-DRA Helm chart (`monitor.nodeLevel.enabled: true`).

## Problem

HAMi provides real-time GPU memory monitoring via HAMi-core shared memory files. However, two issues prevented monitoring from working with DRA-allocated containers:

1. **Cache directory mismatch**: The DRA driver originally created HAMi-core cache files under `claims/{claimUID}/`, but the HAMi-DRA-monitor's `ContainerLister` only scans `containers/{podUID}_{containerName}/` directories. The monitor never discovered DRA containers.

2. **Monitor not deployed for DRA path**: When DRA is enabled in the HAMi Helm chart, the traditional device-plugin monitor was gated out. A separate monitor deployment was needed.

## Solution

### 1. Cache Directory Alignment (k8s-dra-driver)

The DRA driver was changed to create cache directories using the **same naming convention** as the traditional device plugin:

```
/usr/local/vgpu/containers/{podUID}_{containerName}/
```

Instead of the previous:

```
/usr/local/vgpu/claims/{claimUID}/
```

This makes the HAMi-DRA-monitor's `ContainerLister` able to discover and report metrics for DRA-allocated containers.

#### How pod/container info is resolved

The `resolveContainerInfo()` method in `cmd/hami-kubelet-plugin/hami_core.go` extracts the pod UID and container name by:

1. Reading `claim.Status.ReservedFor[0]` to get the consuming pod's UID and name
2. Fetching the pod via the Kubernetes API
3. Walking `pod.Spec.ResourceClaims` to find which pod-level claim reference matches this `ResourceClaim`
4. Walking `pod.Spec.Containers[].Resources.Claims` to find which container uses that claim reference

**Template-based ResourceClaims:** When pods use `ResourceClaimTemplates`, the claim names are auto-generated. The driver also checks `pod.Status.ResourceClaimStatuses` to match the auto-generated claim name back to the pod-level template ref name.

If resolution fails, the driver falls back to a claim-based directory name. The monitor won't parse the fallback format, but HAMi-core GPU limiting still works.

#### Checkpoint changes

The `DeviceConfigState` struct now includes a `CacheDir` field that stores the exact host path created during `Prepare()`. The `Cleanup()` function uses this stored path during `Unprepare()` rather than deriving it from the claim UID.

### 2. HAMi-DRA-monitor (separate DaemonSet)

The monitoring is handled by the **HAMi-DRA-monitor**, deployed as a separate DaemonSet from the [HAMi-DRA](https://github.com/loiht2/HAMi-DRA) Helm chart. The monitor:

- Runs on every GPU node (DaemonSet with `nodeLevel.enabled: true`)
- Mounts the host's `/usr/local/vgpu/containers/` directory
- Scans HAMi-core shared memory (`.cache`) files for each `{podUID}_{containerName}` directory
- Reads `monitorused[]` from shared memory for real NVML-reported GPU memory
- Exposes per-container Prometheus metrics

### 3. RBAC for Pod Access

The DRA driver's ClusterRole includes `pods` permissions (get, list, watch, update, patch) so that `resolveContainerInfo()` can look up pod specs.

## Architecture

```
Pod with DRA ResourceClaim scheduled to node
         │
         ▼
Kubelet calls NodePrepareResources()
         │
         ▼
k8s-dra-driver resolveContainerInfo():
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
HAMi-DRA-monitor (separate DaemonSet) scans containers/ directory
  ├─ Parses {podUID}_{containerName} from dir name
  ├─ Mmaps .cache file → UsageInfo
  ├─ Reads monitorused[] for real NVML memory
  └─ Exposes Prometheus metrics
```

## Files Changed in k8s-dra-driver

| File | Change |
|------|--------|
| `cmd/hami-kubelet-plugin/hami_core.go` | Added `resolveContainerInfo()`, changed `GetCDIContainerEdits()` to create `containers/{podUID}_{ctrName}/` dirs, changed `Cleanup()` to use stored cache dir, added K8s clientset. Also handles template-based `ResourceClaims` via `pod.Status.ResourceClaimStatuses`. |
| `cmd/hami-kubelet-plugin/device_state.go` | Added `CacheDir` to `DeviceConfigState`, updated `NewDeviceState` / `applySharingConfig` / `unprepareDevices` call signatures. |
| `demo/yaml/rbac.yaml` | Added pods RBAC permissions to ClusterRole. |

## Related Projects

| Project | Role |
|---------|------|
| [HAMi-core](https://github.com/loiht2/HAMi-core-fix-memory) | LD_PRELOAD CUDA interceptor; writes GPU usage to shared memory; `memory_monitor_watcher` thread writes NVML data to `monitorused[]` |
| [HAMi-DRA](https://github.com/loiht2/HAMi-DRA) | Webhook + Monitor; the monitor DaemonSet reads shared memory and exposes Prometheus metrics |
| [k8s-dra-driver](https://github.com/loiht2/k8s-dra-driver) | DRA kubelet plugin; creates `containers/{podUID}_{containerName}/` cache dirs |
| [HAMi](https://github.com/loiht2/HAMi) | Parent Helm chart; embeds HAMi-DRA as subchart |

## Known Limitations

- **Shared claims**: If a single `ResourceClaim` is reserved by multiple pods, only the first consumer's pod/container is resolved. The monitor attributes all usage to one container.

- **Fallback directory**: If the pod cannot be looked up during `Prepare`, the directory name won't match the `{podUID}_{containerName}` convention and the monitor will skip it. HAMi-core GPU limiting still works; only monitoring is degraded.
