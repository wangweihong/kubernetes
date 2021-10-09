/*
Copyright 2016 The Kubernetes Authors.

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

package volumemanager

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog"
	"k8s.io/utils/mount"

	v1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/metrics"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/populator"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/reconciler"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
)

const (
	// reconcilerLoopSleepPeriod is the amount of time the reconciler loop waits
	// between successive executions
	reconcilerLoopSleepPeriod = 100 * time.Millisecond //调和间隔 100毫秒

	// desiredStateOfWorldPopulatorLoopSleepPeriod is the amount of time the
	// DesiredStateOfWorldPopulator loop waits between successive executions
	desiredStateOfWorldPopulatorLoopSleepPeriod = 100 * time.Millisecond

	// desiredStateOfWorldPopulatorGetPodStatusRetryDuration is the amount of
	// time the DesiredStateOfWorldPopulator loop waits between successive pod
	// cleanup calls (to prevent calling containerruntime.GetPodStatus too
	// frequently).
	desiredStateOfWorldPopulatorGetPodStatusRetryDuration = 2 * time.Second

	// podAttachAndMountTimeout is the maximum amount of time the
	// WaitForAttachAndMount call will wait for all volumes in the specified pod
	// to be attached and mounted. Even though cloud operations can take several
	// minutes to complete, we set the timeout to 2 minutes because kubelet
	// will retry in the next sync iteration. This frees the associated
	// goroutine of the pod to process newer updates if needed (e.g., a delete
	// request to the pod).
	// Value is slightly offset from 2 minutes to make timeouts due to this
	// constant recognizable.
	podAttachAndMountTimeout = 2*time.Minute + 3*time.Second //轮询Pod所有期待的卷挂载到Pod内部的超时时间

	// podAttachAndMountRetryInterval is the amount of time the GetVolumesForPod
	// call waits before retrying
	podAttachAndMountRetryInterval = 300 * time.Millisecond // 轮询Pod所有期待的卷挂载到Pod内部的间隔时间

	// waitForAttachTimeout is the maximum amount of time a
	// operationexecutor.Mount call will wait for a volume to be attached.
	// Set to 10 minutes because we've seen attach operations take several
	// minutes to complete for some volume plugins in some cases. While this
	// operation is waiting it only blocks other operations on the same device,
	// other devices are not affected.
	waitForAttachTimeout = 10 * time.Minute
)

// VolumeManager runs a set of asynchronous loops that figure out which volumes
// need to be attached/mounted/unmounted/detached based on the pods scheduled on
// this node and makes it so.
type VolumeManager interface {
	// Starts the volume manager and all the asynchronous loops that it controls
	Run(sourcesReady config.SourcesReady, stopCh <-chan struct{})

	// WaitForAttachAndMount processes the volumes referenced in the specified
	// pod and blocks until they are all attached and mounted (reflected in
	// actual state of the world).
	// An error is returned if all volumes are not attached and mounted within
	// the duration defined in podAttachAndMountTimeout.
	WaitForAttachAndMount(pod *v1.Pod) error // 间隔300毫秒轮询直到Pod中所有期待的卷都挂载到Pod内部，或者超时（2分3秒）

	// GetMountedVolumesForPod returns a VolumeMap containing the volumes
	// referenced by the specified pod that are successfully attached and
	// mounted. The key in the map is the OuterVolumeSpecName (i.e.
	// pod.Spec.Volumes[x].Name). It returns an empty VolumeMap if pod has no
	// volumes.
	GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap //获取已经挂载到Pod内部的卷表

	// GetExtraSupplementalGroupsForPod returns a list of the extra
	// supplemental groups for the Pod. These extra supplemental groups come
	// from annotations on persistent volumes that the pod depends on.
	GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64 //Pod的额外补充组

	// GetVolumesInUse returns a list of all volumes that implement the volume.Attacher
	// interface and are currently in use according to the actual and desired
	// state of the world caches. A volume is considered "in use" as soon as it
	// is added to the desired state of world, indicating it *should* be
	// attached to this node and remains "in use" until it is removed from both
	// the desired state of the world and the actual state of the world, or it
	// has been unmounted (as indicated in actual state of world).
	GetVolumesInUse() []v1.UniqueVolumeName

	// ReconcilerStatesHasBeenSynced returns true only after the actual states in reconciler
	// has been synced at least once after kubelet starts so that it is safe to update mounted
	// volume list retrieved from actual state.
	ReconcilerStatesHasBeenSynced() bool

	// VolumeIsAttached returns true if the given volume is attached to this
	// node.
	VolumeIsAttached(volumeName v1.UniqueVolumeName) bool

	// Marks the specified volume as having successfully been reported as "in
	// use" in the nodes's volume status.
	MarkVolumesAsReportedInUse(volumesReportedAsInUse []v1.UniqueVolumeName)
}

// NewVolumeManager returns a new concrete instance implementing the
// VolumeManager interface.
//
// kubeClient - kubeClient is the kube API client used by DesiredStateOfWorldPopulator
//   to communicate with the API server to fetch PV and PVC objects
// volumePluginMgr - the volume plugin manager used to access volume plugins.
//   Must be pre-initialized.
func NewVolumeManager(
	controllerAttachDetachEnabled bool,
	nodeName k8stypes.NodeName,
	podManager pod.Manager,
	podStatusProvider status.PodStatusProvider,
	kubeClient clientset.Interface,
	volumePluginMgr *volume.VolumePluginMgr,
	kubeContainerRuntime container.Runtime,
	mounter mount.Interface,
	hostutil hostutil.HostUtils,
	kubeletPodsDir string,
	recorder record.EventRecorder,
	checkNodeCapabilitiesBeforeMount bool,
	keepTerminatedPodVolumes bool,
	blockVolumePathHandler volumepathhandler.BlockVolumePathHandler) VolumeManager {

	vm := &volumeManager{
		kubeClient:          kubeClient,
		volumePluginMgr:     volumePluginMgr,
		desiredStateOfWorld: cache.NewDesiredStateOfWorld(volumePluginMgr),
		actualStateOfWorld:  cache.NewActualStateOfWorld(nodeName, volumePluginMgr),
		operationExecutor: operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
			kubeClient,
			volumePluginMgr,
			recorder,
			checkNodeCapabilitiesBeforeMount,
			blockVolumePathHandler)),
	}

	intreeToCSITranslator := csitrans.New()
	csiMigratedPluginManager := csimigration.NewPluginManager(intreeToCSITranslator)

	vm.intreeToCSITranslator = intreeToCSITranslator
	vm.csiMigratedPluginManager = csiMigratedPluginManager
	vm.desiredStateOfWorldPopulator = populator.NewDesiredStateOfWorldPopulator(
		kubeClient,
		desiredStateOfWorldPopulatorLoopSleepPeriod,
		desiredStateOfWorldPopulatorGetPodStatusRetryDuration,
		podManager,
		podStatusProvider,
		vm.desiredStateOfWorld,
		vm.actualStateOfWorld,
		kubeContainerRuntime,
		keepTerminatedPodVolumes,
		csiMigratedPluginManager,
		intreeToCSITranslator)
	vm.reconciler = reconciler.NewReconciler(
		kubeClient,
		controllerAttachDetachEnabled,
		reconcilerLoopSleepPeriod,
		waitForAttachTimeout,
		nodeName,
		vm.desiredStateOfWorld,
		vm.actualStateOfWorld,
		vm.desiredStateOfWorldPopulator.HasAddedPods,
		vm.operationExecutor,
		mounter,
		hostutil,
		volumePluginMgr,
		kubeletPodsDir)

	return vm
}

// volumeManager implements the VolumeManager interface
type volumeManager struct {
	// kubeClient is the kube API client used by DesiredStateOfWorldPopulator to
	// communicate with the API server to fetch PV and PVC objects
	kubeClient clientset.Interface

	// volumePluginMgr is the volume plugin manager used to access volume
	// plugins. It must be pre-initialized.
	volumePluginMgr *volume.VolumePluginMgr

	// desiredStateOfWorld is a data structure containing the desired state of
	// the world according to the volume manager: i.e. what volumes should be
	// attached and which pods are referencing the volumes).
	// The data structure is populated by the desired state of the world
	// populator using the kubelet pod manager.
	desiredStateOfWorld cache.DesiredStateOfWorld

	// actualStateOfWorld is a data structure containing the actual state of
	// the world according to the manager: i.e. which volumes are attached to
	// this node and what pods the volumes are mounted to.
	// The data structure is populated upon successful completion of attach,
	// detach, mount, and unmount actions triggered by the reconciler.
	actualStateOfWorld cache.ActualStateOfWorld

	// operationExecutor is used to start asynchronous attach, detach, mount,
	// and unmount operations.
	operationExecutor operationexecutor.OperationExecutor

	// reconciler runs an asynchronous periodic loop to reconcile the
	// desiredStateOfWorld with the actualStateOfWorld by triggering attach,
	// detach, mount, and unmount operations using the operationExecutor.
	reconciler reconciler.Reconciler

	// desiredStateOfWorldPopulator runs an asynchronous periodic loop to
	// populate the desiredStateOfWorld using the kubelet PodManager.
	desiredStateOfWorldPopulator populator.DesiredStateOfWorldPopulator

	// csiMigratedPluginManager keeps track of CSI migration status of plugins
	csiMigratedPluginManager csimigration.PluginManager

	// intreeToCSITranslator translates in-tree volume specs to CSI
	intreeToCSITranslator csimigration.InTreeToCSITranslator
}

//1. 启动一个协程处理
func (vm *volumeManager) Run(sourcesReady config.SourcesReady, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	//异步运行期待挂载卷填充器
	go vm.desiredStateOfWorldPopulator.Run(sourcesReady, stopCh)
	klog.V(2).Infof("The desired_state_of_world populator starts")
	//异步运行调和器
	klog.Infof("Starting Kubelet Volume Manager")
	go vm.reconciler.Run(stopCh)

	metrics.Register(vm.actualStateOfWorld, vm.desiredStateOfWorld, vm.volumePluginMgr)
	//启动
	if vm.kubeClient != nil {
		// start informer for CSIDriver
		vm.volumePluginMgr.Run(stopCh)
	}

	<-stopCh
	klog.Infof("Shutting down Kubelet Volume Manager")
}

//获取已经挂载到Pod内部的卷表
func (vm *volumeManager) GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap {
	podVolumes := make(container.VolumeMap)
	//获取实际已附属到节点上的卷中哪些卷已经挂载到Pod内部
	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		//key为对外名字，即pod.spec.volume中的名字， 而不是pod.spec.container.volumeMount中的名字
		podVolumes[mountedVolume.OuterVolumeSpecName] = container.VolumeInfo{
			Mounter:             mountedVolume.Mounter,
			BlockVolumeMapper:   mountedVolume.BlockVolumeMapper,
			ReadOnly:            mountedVolume.VolumeSpec.ReadOnly,
			InnerVolumeSpecName: mountedVolume.InnerVolumeSpecName,
		}
	}
	return podVolumes
}

func (vm *volumeManager) GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64 {
	podName := util.GetUniquePodName(pod)
	supplementalGroups := sets.NewString()

	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		if mountedVolume.VolumeGidValue != "" {
			supplementalGroups.Insert(mountedVolume.VolumeGidValue)
		}
	}

	result := make([]int64, 0, supplementalGroups.Len())
	for _, group := range supplementalGroups.List() {
		iGroup, extra := getExtraSupplementalGid(group, pod)
		if !extra {
			continue
		}

		result = append(result, int64(iGroup))
	}

	return result
}

//
func (vm *volumeManager) GetVolumesInUse() []v1.UniqueVolumeName {
	// Report volumes in desired state of world and actual state of world so
	// that volumes are marked in use as soon as the decision is made that the
	// volume *should* be attached to this node until it is safely unmounted.
	desiredVolumes := vm.desiredStateOfWorld.GetVolumesToMount()
	allAttachedVolumes := vm.actualStateOfWorld.GetAttachedVolumes()
	volumesToReportInUse := make([]v1.UniqueVolumeName, 0, len(desiredVolumes)+len(allAttachedVolumes))
	desiredVolumesMap := make(map[v1.UniqueVolumeName]bool, len(desiredVolumes)+len(allAttachedVolumes))

	for _, volume := range desiredVolumes {
		if volume.PluginIsAttachable {
			if _, exists := desiredVolumesMap[volume.VolumeName]; !exists {
				desiredVolumesMap[volume.VolumeName] = true
				volumesToReportInUse = append(volumesToReportInUse, volume.VolumeName)
			}
		}
	}

	for _, volume := range allAttachedVolumes {
		if volume.PluginIsAttachable {
			if _, exists := desiredVolumesMap[volume.VolumeName]; !exists {
				volumesToReportInUse = append(volumesToReportInUse, volume.VolumeName)
			}
		}
	}

	sort.Slice(volumesToReportInUse, func(i, j int) bool {
		return string(volumesToReportInUse[i]) < string(volumesToReportInUse[j])
	})
	return volumesToReportInUse
}

func (vm *volumeManager) ReconcilerStatesHasBeenSynced() bool {
	return vm.reconciler.StatesHasBeenSynced()
}

func (vm *volumeManager) VolumeIsAttached(
	volumeName v1.UniqueVolumeName) bool {
	return vm.actualStateOfWorld.VolumeExists(volumeName)
}

func (vm *volumeManager) MarkVolumesAsReportedInUse(
	volumesReportedAsInUse []v1.UniqueVolumeName) {
	vm.desiredStateOfWorld.MarkVolumesReportedInUse(volumesReportedAsInUse)
}

// 轮询直到Pod中所有期待的卷都挂载到Pod内部，或者超时
func (vm *volumeManager) WaitForAttachAndMount(pod *v1.Pod) error {
	if pod == nil {
		return nil
	}
	// 从Pod.spec.containers中提取VolumeMounts以及VolumeDevices(块设备)的名字
	// Pod期待挂载的卷
	expectedVolumes := getExpectedVolumes(pod)
	//Pod没有想要挂载的卷
	if len(expectedVolumes) == 0 {
		// No volumes to verify
		return nil
	}

	klog.V(3).Infof("Waiting for volumes to attach and mount for pod %q", format.Pod(pod))
	uniquePodName := util.GetUniquePodName(pod) //Pod的UID

	// Some pods expect to have Setup called over and over again to update.
	// Remount plugins for which this is true. (Atomically updating volumes,
	// like Downward API, depend on this to update the contents of the volume).
	vm.desiredStateOfWorldPopulator.ReprocessPod(uniquePodName) //标志Pod挂载失败

	//300毫秒轮询，检测pod中所有期待的卷是否全部正确挂载到Pod内部。最多轮询2分3秒
	err := wait.PollImmediate(
		podAttachAndMountRetryInterval,                              //300毫秒
		podAttachAndMountTimeout,                                    //超时2分3秒
		vm.verifyVolumesMountedFunc(uniquePodName, expectedVolumes)) //1.pod的卷挂载是否有出错，2. pod所有卷是否都已经挂载
	//	1. 超时仍有卷没有挂载完成 2.有卷挂载出现错误
	if err != nil {
		// 找到pod中期待挂载的文件系统/块设备卷中有哪些还没有挂载到Pod内部
		unmountedVolumes :=
			vm.getUnmountedVolumes(uniquePodName, expectedVolumes)
		// Also get unattached volumes for error message
		// 找到pod中期待挂载的文件系统/块设备卷中有哪些卷还没有附属到节点上
		unattachedVolumes :=
			vm.getUnattachedVolumes(expectedVolumes)
		// 所有pod期待挂载的卷都已经挂载
		// 处理到这里，挂载又完成了?
		if len(unmountedVolumes) == 0 {
			return nil
		}
		//umounted volume: 卷已经附属到节点上，但还没有挂载到Pod内部
		//unattached volume: 卷还没有附属到节点上
		return fmt.Errorf(
			"unmounted volumes=%v, unattached volumes=%v: %s",
			unmountedVolumes,
			unattachedVolumes,
			err)
	}

	klog.V(3).Infof("All volumes are attached and mounted for pod %q", format.Pod(pod))
	return nil
}

// getUnattachedVolumes returns a list of the volumes that are expected to be attached but
// are not currently attached to the node
// 找到expectedVolume中有哪些卷还没有附属到节点上
func (vm *volumeManager) getUnattachedVolumes(expectedVolumes []string) []string {
	unattachedVolumes := []string{}
	for _, volume := range expectedVolumes {
		if !vm.actualStateOfWorld.VolumeExists(v1.UniqueVolumeName(volume)) {
			unattachedVolumes = append(unattachedVolumes, volume)
		}
	}
	return unattachedVolumes
}

// verifyVolumesMountedFunc returns a method that returns true when all expected
// volumes are mounted.
//1.pod的卷挂载是否有出错，2.expectedVolume是否都已经全部挂载到Pod内部
func (vm *volumeManager) verifyVolumesMountedFunc(podName types.UniquePodName, expectedVolumes []string) wait.ConditionFunc {
	return func() (done bool, err error) {
		//存在错误
		if errs := vm.desiredStateOfWorld.PopPodErrors(podName); len(errs) > 0 {
			return true, errors.New(strings.Join(errs, "; "))
		}
		//expectedVolume是否都已经全部挂载
		return len(vm.getUnmountedVolumes(podName, expectedVolumes)) == 0, nil
	}
}

// getUnmountedVolumes fetches the current list of mounted volumes from
// the actual state of the world, and uses it to process the list of
// expectedVolumes. It returns a list of unmounted volumes.
// The list also includes volume that may be mounted in uncertain state.
// 从实际已挂载到pod内部的卷，从中找到哪些expectedVolumes没有挂载
func (vm *volumeManager) getUnmountedVolumes(podName types.UniquePodName, expectedVolumes []string) []string {
	mountedVolumes := sets.NewString()
	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		mountedVolumes.Insert(mountedVolume.OuterVolumeSpecName)
	}
	//对比实际已挂载卷(mountedVolume)以及期待挂载卷(expectedVolumes),找到哪些期待挂载卷还没有挂载
	return filterUnmountedVolumes(mountedVolumes, expectedVolumes)
}

// filterUnmountedVolumes adds each element of expectedVolumes that is not in
// mountedVolumes to a list of unmountedVolumes and returns it.
//对比实际已挂载卷(mountedVolume)以及期待挂载卷(expectedVolumes),找到哪些期待挂载卷还没有挂载
func filterUnmountedVolumes(mountedVolumes sets.String, expectedVolumes []string) []string {
	unmountedVolumes := []string{}
	for _, expectedVolume := range expectedVolumes {
		if !mountedVolumes.Has(expectedVolume) {
			unmountedVolumes = append(unmountedVolumes, expectedVolume)
		}
	}
	return unmountedVolumes
}

// getExpectedVolumes returns a list of volumes that must be mounted in order to
// consider the volume setup step for this pod satisfied.
// 从Pod.spec.containers中提取VolumeMounts以及VolumeDevices(块设备)的名字
func getExpectedVolumes(pod *v1.Pod) []string {
	mounts, devices := util.GetPodVolumeNames(pod)
	return mounts.Union(devices).UnsortedList()
}

// getExtraSupplementalGid returns the value of an extra supplemental GID as
// defined by an annotation on a volume and a boolean indicating whether the
// volume defined a GID that the pod doesn't already request.
func getExtraSupplementalGid(volumeGidValue string, pod *v1.Pod) (int64, bool) {
	if volumeGidValue == "" {
		return 0, false
	}

	gid, err := strconv.ParseInt(volumeGidValue, 10, 64)
	if err != nil {
		return 0, false
	}

	if pod.Spec.SecurityContext != nil {
		for _, existingGid := range pod.Spec.SecurityContext.SupplementalGroups {
			if gid == int64(existingGid) {
				return 0, false
			}
		}
	}

	return gid, true
}
