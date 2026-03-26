/*
Copyright 2025 The HAMi Authors.

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

package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/Masterminds/semver"
	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/Project-HAMi/k8s-dra-driver/pkg/featuregates"
	"github.com/spf13/pflag"
	"github.com/urfave/cli/v2"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)


// For deviceinfo.goh
type HAMiGpuInfo struct {
	GpuInfo
}

func (d *HAMiGpuInfo) CanonicalName() string {
	// return fmt.Sprintf("hami-gpu-%d-%d", d.minor, d.hamiIndex)
	return fmt.Sprintf("hami-gpu-%d", d.minor)
}

func (d *HAMiGpuInfo) GetDevice() resourceapi.Device {
	allowed := true
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(string(HAMiGpuDeviceType)),
			},
			"uuid": {
				StringValue: &d.UUID,
			},
			"minor": {
				IntValue: ptr.To(int64(d.minor)),
			},
			"productName": {
				StringValue: &d.productName,
			},
			"brand": {
				StringValue: &d.brand,
			},
			"architecture": {
				StringValue: &d.architecture,
			},
			"cudaComputeCapability": {
				VersionValue: ptr.To(semver.MustParse(d.cudaComputeCapability).String()),
			},
			"driverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.driverVersion).String()),
			},
			"cudaDriverVersion": {
				VersionValue: ptr.To(semver.MustParse(d.cudaDriverVersion).String()),
			},
			"pcieBusID": {
				StringValue: &d.pcieBusID,
			},
			d.pcieRootAttr.Name: d.pcieRootAttr.Value,
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"cores": {
				Value: *resource.NewQuantity(int64(100), resource.DecimalSI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(int64(100), resource.DecimalSI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min: resource.NewQuantity(int64(0), resource.DecimalSI),
						Max: resource.NewQuantity(int64(100), resource.DecimalSI),
						Step: resource.NewQuantity(int64(1), resource.DecimalSI),
					},
				},
			},
			"memory": {
				Value: *resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min: resource.NewQuantity(int64(1048576), resource.BinarySI),
						Max: resource.NewQuantity(int64(d.memoryBytes), resource.BinarySI),
						Step: resource.NewQuantity(int64(1048576), resource.BinarySI),
					},
				},
			},
		},
		AllowMultipleAllocations: &allowed,
	}
	return device
}


// For nvlib.go
func (l deviceLib) enumerateGpusDevicesForHAMiCore(config *Config) (AllocatableDevices, error) {
	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	// splitCount := config.flags.hamiCoreDevSplitCount
	devices := make(AllocatableDevices)
	err := l.VisitDevices(func(i int, d nvdev.Device) error {
		gpuInfo, err := l.getGpuInfo(i, d)
		if err != nil {
			return fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		// for idx := range splitCount {
		hamiGpuInfo := &HAMiGpuInfo{
			GpuInfo: GpuInfo{
				UUID:                  gpuInfo.UUID,
				minor:                 gpuInfo.minor,
				migEnabled:            gpuInfo.migEnabled,
				memoryBytes:           gpuInfo.memoryBytes,
				productName:           gpuInfo.productName,
				brand:                 gpuInfo.brand,
				architecture:          gpuInfo.architecture,
				cudaComputeCapability: gpuInfo.cudaComputeCapability,
				driverVersion:         gpuInfo.driverVersion,
				cudaDriverVersion:     gpuInfo.cudaDriverVersion,
				pcieBusID:             gpuInfo.pcieBusID,
				pcieRootAttr:          gpuInfo.pcieRootAttr,
				migProfiles:           gpuInfo.migProfiles,
			},
		}
		deviceInfo := &AllocatableDevice{
			HAMiGpu: hamiGpuInfo,
		}
		name := hamiGpuInfo.CanonicalName()
		devices[name] = deviceInfo
		// }

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error visiting devices: %w", err)
	}

	// Debug:
	for name := range devices {
		klog.Infof("enumerateGpusDevicesForHAMiCore -- CanonicalName: %s", name)
	}

	return devices, nil
}

// For prepared.go
type PreparedHAMiGpu struct {
	Info   *HAMiGpuInfo          `json:"info"`
	Device *kubeletplugin.Device `json:"device"`
}

func (l PreparedDeviceList) HAMiGpus() PreparedDeviceList {
	var devices PreparedDeviceList
	for _, device := range l {
		if device.Type() == HAMiGpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (l PreparedDeviceList) HAMiGpuUUIDs() []string {
	var uuids []string
	for _, device := range l.HAMiGpus() {
		uuids = append(uuids, device.HAMiGpu.Info.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func (g *PreparedDeviceGroup) HAMIGpuUUIDs() []string {
	return g.Devices.HAMiGpus().UUIDs()
}

// For sharing.go
type HAMiCoreManager struct {
	hostHookPath string
	nvdevlib     *deviceLib
	clientset    coreclientset.Interface
}

func NewHAMiCoreManager(deviceLib *deviceLib, clientset coreclientset.Interface) *HAMiCoreManager {
	return &HAMiCoreManager{
		nvdevlib:     deviceLib,
		hostHookPath: "/usr/local",
		clientset:    clientset,
	}
}

func (m *HAMiCoreManager) getConsumableCapacityMap(claim *resourceapi.ResourceClaim) map[string]map[resourceapi.QualifiedName]resource.Quantity {
	resMap := map[string]map[resourceapi.QualifiedName]resource.Quantity{}
	for _, result := range claim.Status.Allocation.Devices.Results {
		devName := result.Device
		if _, exists := resMap[devName]; !exists {
			resMap[devName] = map[resourceapi.QualifiedName]resource.Quantity{}
		}
		maps.Copy(resMap[devName], result.ConsumedCapacity)
	}
	return resMap
}

// resolveContainerInfo looks up the pod and container that reserved this claim.
// Returns (podUID, containerName, error). If the claim is reserved by multiple
// consumers or the container cannot be determined, containerName will be empty.
func (m *HAMiCoreManager) resolveContainerInfo(ctx context.Context, claim *resourceapi.ResourceClaim) (string, string, error) {
	if len(claim.Status.ReservedFor) == 0 {
		return "", "", fmt.Errorf("claim %s has no consumers in ReservedFor", claim.Name)
	}

	consumer := claim.Status.ReservedFor[0]
	podUID := string(consumer.UID)

	// Fetch the pod to find which container references this claim
	pod, err := m.clientset.CoreV1().Pods(claim.Namespace).Get(ctx, consumer.Name, metav1.GetOptions{})
	if err != nil {
		return podUID, "", fmt.Errorf("failed to get pod %s/%s: %w", claim.Namespace, consumer.Name, err)
	}

	// Find which pod-level claim reference name corresponds to this ResourceClaim.
	// Two cases:
	//   1) Directly referenced claims: rc.ResourceClaimName == claim.Name
	//   2) Template-based claims:      pod.Status.ResourceClaimStatuses maps
	//      the template ref name to the auto-generated claim name.
	var podClaimName string
	for _, rc := range pod.Spec.ResourceClaims {
		if rc.ResourceClaimName != nil && *rc.ResourceClaimName == claim.Name {
			podClaimName = rc.Name
			break
		}
	}
	// If not found via ResourceClaimName, check template-based claims via pod status
	if podClaimName == "" {
		for _, rcs := range pod.Status.ResourceClaimStatuses {
			if rcs.ResourceClaimName != nil && *rcs.ResourceClaimName == claim.Name {
				podClaimName = rcs.Name
				break
			}
		}
	}
	if podClaimName == "" {
		return podUID, "", fmt.Errorf("could not find claim reference for %s in pod %s/%s", claim.Name, pod.Namespace, pod.Name)
	}

	// Find the container that uses this claim
	for _, ctr := range pod.Spec.Containers {
		for _, rc := range ctr.Resources.Claims {
			if rc.Name == podClaimName {
				return podUID, ctr.Name, nil
			}
		}
	}
	for _, ctr := range pod.Spec.InitContainers {
		for _, rc := range ctr.Resources.Claims {
			if rc.Name == podClaimName {
				return podUID, ctr.Name, nil
			}
		}
	}

	return podUID, "", fmt.Errorf("no container found referencing claim %s in pod %s/%s", claim.Name, pod.Namespace, pod.Name)
}

// GetCDIContainerEdits creates the CDI container edits for HAMi-core.
// It creates the cache directory under containers/{podUID}_{containerName}/ so
// that the existing vGPU monitor (ContainerLister) can discover and report
// real-time GPU memory usage for DRA-allocated containers.
// Returns the container edits, the host-side cache directory path, and an error.
func (m *HAMiCoreManager) GetCDIContainerEdits(ctx context.Context, claim *resourceapi.ResourceClaim, devs AllocatableDevices) (*cdiapi.ContainerEdits, string, error) {
	// Resolve pod UID and container name from the claim's ReservedFor field
	podUID, containerName, err := m.resolveContainerInfo(ctx, claim)

	var cacheFileHostDirectory string
	if err != nil || containerName == "" {
		// Fallback: use claim UID under containers/ dir.
		// The monitor won't fully parse this, but it keeps the cache files
		// in a discoverable location.
		klog.Warningf("Could not resolve container info for claim %s (pod %s), falling back to claim-based directory: %v", claim.Name, podUID, err)
		cacheFileHostDirectory = fmt.Sprintf("%s/vgpu/containers/%s", m.hostHookPath, claim.UID)
	} else {
		cacheFileHostDirectory = fmt.Sprintf("%s/vgpu/containers/%s_%s", m.hostHookPath, podUID, containerName)
	}

	if removeErr := os.RemoveAll(cacheFileHostDirectory); removeErr != nil {
		klog.Warningf("Failed to remove host directory for cachefile %s: %s", cacheFileHostDirectory, removeErr)
	}
	if mkdirErr := os.MkdirAll(cacheFileHostDirectory, 0777); mkdirErr != nil {
		klog.Warningf("Failed to create host directory for cachefile %s: %s", cacheFileHostDirectory, mkdirErr)
	}
	if chmodErr := os.Chmod(cacheFileHostDirectory, 0777); chmodErr != nil {
		klog.Warningf("Failed to change mod of host directory for cachefile %s: %s", cacheFileHostDirectory, chmodErr)
	}
	os.MkdirAll("/tmp/vgpulock", 0777)
	os.Chmod("/tmp/vgpulock", 0777)

	hamiEnvs := []string{}
	hamiEnvs = append(hamiEnvs, fmt.Sprintf("CUDA_DEVICE_MEMORY_SHARED_CACHE=%s/%v.cache", cacheFileHostDirectory, uuid.New().String()))

	devCapMap := m.getConsumableCapacityMap(claim)
	idx := 0
	for name, dev := range devs {
		klog.Infof("HAMiCoreManager GetCDIContainerEdits for dev: %s", name)
		capNameSMLimit := resourceapi.QualifiedName("cores")
		capNameMemoryLimit := resourceapi.QualifiedName("memory")
		SMLimitEnv := fmt.Sprintf("CUDA_DEVICE_SM_LIMIT_%d=%s", idx, "60")
		memoryLimit := strconv.FormatUint(dev.HAMiGpu.memoryBytes/1024/1024, 10) + "m"
		MemoryLimitEnv := fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%d=%s", idx, memoryLimit)
		if _, ok := devCapMap[name]; ok {
			if _, ok := devCapMap[name][capNameSMLimit]; ok {
				q := devCapMap[name][capNameSMLimit]
				val, succ := q.AsInt64()
				if succ {
					SMLimitEnv = fmt.Sprintf("CUDA_DEVICE_SM_LIMIT_%d=%s", idx, strconv.FormatInt(val, 10))
				}
			}
			if _, ok := devCapMap[name][capNameMemoryLimit]; ok {
				q := devCapMap[name][capNameMemoryLimit]
				val, succ := q.AsInt64()
				if succ {
					MemoryLimitEnv = fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%d=%s", idx, strconv.FormatInt(val/1024/1024, 10)+"m")
				}
			}
		}
		hamiEnvs = append(hamiEnvs, SMLimitEnv, MemoryLimitEnv)
		idx++
	}

	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: hamiEnvs,
			Mounts: []*cdispec.Mount{
				{
					ContainerPath: cacheFileHostDirectory,
					HostPath:      cacheFileHostDirectory,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: m.hostHookPath + "/vgpu/libvgpu.so",
					HostPath:      m.hostHookPath + "/vgpu/libvgpu.so",
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				// TODO: Check CUDA_DISABLE_CONTROL env before mount ld.so.preload
				{
					ContainerPath: "/etc/ld.so.preload",
					HostPath:      m.hostHookPath + "/vgpu/ld.so.preload",
					Options:       []string{"ro", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: "/tmp/vgpulock",
					HostPath:      "/tmp/vgpulock",
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				// Memory resizer DaemonSet UDS socket
				{
					ContainerPath: "/var/run/hami",
					HostPath:      "/var/run/hami",
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}, cacheFileHostDirectory, nil
}

// Cleanup removes the cache directory created during Prepare.
// cacheDir is the host-side directory path stored in DeviceConfigState.CacheDir.
func (m *HAMiCoreManager) Cleanup(cacheDir string) error {
	if cacheDir == "" {
		klog.Warning("Cleanup called with empty cacheDir, skipping")
		return nil
	}
	klog.Infof("Cleaning up HAMi-core cache directory: %s", cacheDir)
	_ = os.RemoveAll(cacheDir)
	return nil
}

// For types.go
const HAMiGpuDeviceType = "hami-gpu"

// For FeatureGates
type FeatureGateConfig struct{}

// NewFeatureGateConfig creates a new unified feature gate configuration.
func newFeatureGateConfig() *FeatureGateConfig {
	return &FeatureGateConfig{}
}

// Flags returns the CLI flags for the unified feature gate configuration.
func (f *FeatureGateConfig) Flags() []cli.Flag {
	var fs pflag.FlagSet

	// Add the unified feature gates flag containing both project and logging features
	fs.AddFlag(&pflag.Flag{
		Name: "feature-gates",
		Usage: "A set of key=value pairs that describe feature gates for alpha/experimental features. " +
			"Options are:\n     " + strings.Join(featuregates.KnownFeatures(), "\n     "),
		Value: featuregates.FeatureGates.(pflag.Value), //nolint:forcetypeassert // No need for type check: FeatureGates is a *featuregate.featureGate, which implements pflag.Value.
	})

	var flags []cli.Flag
	fs.VisitAll(func(flag *pflag.Flag) {
		flags = append(flags, pflagToCLI(flag, "Feature Gates:"))
	})
	return flags
}

func pflagToCLI(flag *pflag.Flag, category string) cli.Flag {
	return &cli.GenericFlag{
		Name:        flag.Name,
		Category:    category,
		Usage:       flag.Usage,
		Value:       flag.Value,
		Destination: flag.Value,
		EnvVars:     []string{strings.ToUpper(strings.ReplaceAll(flag.Name, "-", "_"))},
	}
}
