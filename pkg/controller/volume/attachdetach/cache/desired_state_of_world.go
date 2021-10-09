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

/*
Package cache implements data structures used by the attach/detach controller
to keep track of volumes, the nodes they are attached to, and the pods that
reference them.
*/
package cache

import (
	"fmt"
	"sync"

	"k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/types"
)

// DesiredStateOfWorld defines a set of thread-safe operations supported on
// the attach/detach controller's desired state of the world cache.
// This cache contains nodes->volumes->pods where nodes are all the nodes
// managed by the attach/detach controller, volumes are all the volumes that
// should be attached to the specified node, and pods are the pods that
// reference the volume and are scheduled to that node.
// Note: This is distinct from the DesiredStateOfWorld implemented by the
// kubelet volume manager. They both keep track of different objects. This
// contains attach/detach controller specific state.
type DesiredStateOfWorld interface {
	// AddNode adds the given node to the list of nodes managed by the attach/
	// detach controller.
	// If the node already exists this is a no-op.
	// keepTerminatedPodVolumes is a property of the node that determines
	// if volumes should be mounted and attached for terminated pods.
	//初始化节点Attach描述对象信息，添加到节点表中， 已添加则忽略
	//keepTerminatedPodVolumes用来表示当pod终止时,在节点上Attached/挂载的卷仍然保留。通过volumes.kubernetes.io/keep-terminated-pod-volumes来指定。这个标志调试用
	//attach-detach controller监听节点添加/删除事件, 在节点创建/更新时会将controller管理的node添加到期待表中
	AddNode(nodeName k8stypes.NodeName, keepTerminatedPodVolumes bool)

	// AddPod adds the given pod to the list of pods that reference the
	// specified volume and is scheduled to the specified node.
	// A unique volumeName is generated from the volumeSpec and returned on
	// success.
	// If the pod already exists under the specified volume, this is a no-op.
	// If volumeSpec is not an attachable volume plugin, an error is returned.
	// If no volume with the name volumeName exists in the list of volumes that
	// should be attached to the specified node, the volume is implicitly added.
	// If no node with the name nodeName exists in list of nodes managed by the
	// attach/detach attached controller, an error is returned.
	// 如果volumeSpec的插件支持Attach/Detach操作, 则将volume记录到nodeName的AttachedVolume表中，并把podName记录到volume的ScheduledPod表中
	// 注意： podName是已经调度到由controller负责attach/detach操作的节点上的Pod, volumeSpec是Pod引用卷生成的。
	// nodeName是pod调度的节点
	AddPod(podName types.UniquePodName, pod *v1.Pod, volumeSpec *volume.Spec, nodeName k8stypes.NodeName) (v1.UniqueVolumeName, error)

	// DeleteNode removes the given node from the list of nodes managed by the
	// attach/detach controller.
	// If the node does not exist this is a no-op.
	// If the node exists but has 1 or more child volumes, an error is returned.
	//将节点从节点attach管理表中移除，如果节点仍有attached的卷，报错不进行移除操作
	DeleteNode(nodeName k8stypes.NodeName) error

	// DeletePod removes the given pod from the list of pods that reference the
	// specified volume and are scheduled to the specified node.
	// If no pod exists in the list of pods that reference the specified volume
	// and are scheduled to the specified node, this is a no-op.
	// If a node with the name nodeName does not exist in the list of nodes
	// managed by the attach/detach attached controller, this is a no-op.
	// If no volume with the name volumeName exists in the list of managed
	// volumes under the specified node, this is a no-op.
	// If after deleting the pod, the specified volume contains no other child
	// pods, the volume is also deleted.
	//找到在指定节点上Attach的卷, 然后将pod从卷的ScheduledPod移除。scheduledPods最后一个Pod被删除时。
	// volume也会从nodeManaged对应的节点volumesToAttach表中移除。
	DeletePod(podName types.UniquePodName, volumeName v1.UniqueVolumeName, nodeName k8stypes.NodeName)

	// NodeExists returns true if the node with the specified name exists in
	// the list of nodes managed by the attach/detach controller.
	NodeExists(nodeName k8stypes.NodeName) bool

	// VolumeExists returns true if the volume with the specified name exists
	// in the list of volumes that should be attached to the specified node by
	// the attach detach controller.
	VolumeExists(volumeName v1.UniqueVolumeName, nodeName k8stypes.NodeName) bool

	// GetVolumesToAttach generates and returns a list of volumes to attach
	// and the nodes they should be attached to based on the current desired
	// state of the world.
	//获取所有节点上Attach的卷，有些卷多节点Attach,同样加上
	GetVolumesToAttach() []VolumeToAttach

	// GetPodToAdd generates and returns a map of pods based on the current desired
	// state of world
	GetPodToAdd() map[types.UniquePodName]PodToAdd

	// GetKeepTerminatedPodVolumesForNode determines if node wants volumes to be
	// mounted and attached for terminated pods
	GetKeepTerminatedPodVolumesForNode(k8stypes.NodeName) bool

	// Mark multi-attach error as reported to prevent spamming multiple
	// events for same error
	//设置卷Attach到多个节点错误, 发生在卷不再支持多节点Attached（更改了access Mode), 之前已经Attached了多个节点
	SetMultiAttachError(v1.UniqueVolumeName, k8stypes.NodeName)

	// GetPodsOnNodes returns list of pods ("namespace/name") that require
	// given volume on given nodes.
	GetVolumePodsOnNodes(nodes []k8stypes.NodeName, volumeName v1.UniqueVolumeName) []*v1.Pod
}

// VolumeToAttach represents a volume that should be attached to a node.
type VolumeToAttach struct {
	operationexecutor.VolumeToAttach
}

// PodToAdd represents a pod that references the underlying volume and is
// scheduled to the underlying node.
// 用来指向pod调度的节点以及pod中需要附加到节点的卷？
type PodToAdd struct {
	// pod contains the api object of pod
	Pod *v1.Pod

	// volumeName contains the unique identifier for this volume.
	VolumeName v1.UniqueVolumeName

	// nodeName contains the name of this node.
	NodeName k8stypes.NodeName
}

// NewDesiredStateOfWorld returns a new instance of DesiredStateOfWorld.
func NewDesiredStateOfWorld(volumePluginMgr *volume.VolumePluginMgr) DesiredStateOfWorld {
	return &desiredStateOfWorld{
		nodesManaged:    make(map[k8stypes.NodeName]nodeManaged),
		volumePluginMgr: volumePluginMgr,
	}
}

// 期待Attached的卷/节点/Pod关系管理
type desiredStateOfWorld struct {
	// nodesManaged is a map containing the set of nodes managed by the attach/
	// detach controller. The key in this map is the name of the node and the
	// value is a node object containing more information about the node.
	nodesManaged map[k8stypes.NodeName]nodeManaged //记录每个node上Attached的卷
	//attach-detach controller会监听node的创建/更新，
	//如果node由attach-detach controller进行attach-detach,
	//则会添加到这个表中
	// volumePluginMgr is the volume plugin manager used to create volume
	// plugin objects.
	volumePluginMgr *volume.VolumePluginMgr // k8s支持的卷插件管理器
	sync.RWMutex
}

// nodeManaged represents a node that is being managed by the attach/detach
// controller.
// 记录一个节点上attach的卷列表
type nodeManaged struct {
	// nodeName contains the name of this node.
	nodeName k8stypes.NodeName //节点名

	// volumesToAttach is a map containing the set of volumes that should be
	// attached to this node. The key in the map is the name of the volume and
	// the value is a volumeToAttach object containing more information about the volume.
	volumesToAttach map[v1.UniqueVolumeName]volumeToAttach //附加到节点上的卷列表。 注意这里的UniqueVolumeName是 基于插件/卷/驱动生成的唯一名

	// keepTerminatedPodVolumes determines if for terminated pods(on this node) - volumes
	// should be kept mounted and attached.
	keepTerminatedPodVolumes bool //这个值的设置取决于node的volumes.kubernetes.io/keep-terminated-pod-volumes
	// 这个标志来自kubelet参数，只是用来调试用。
}

// The volumeToAttach object represents a volume that should be attached to a node.
// 用来描述卷，并且记录引用了该卷的已经调度到某个节点的Pod表
type volumeToAttach struct {
	// multiAttachErrorReported indicates whether the multi-attach error has been reported for the given volume.
	// It is used to prevent reporting the error from being reported more than once for a given volume.
	multiAttachErrorReported bool //当卷不支持Attach到多个节点，但之前已经多节点Attach时会设置该标志

	// volumeName contains the unique identifier for this volume.
	volumeName v1.UniqueVolumeName // 注意，这里的卷名是通过 插件/驱动(csi)/卷名生成唯一识别符

	// spec is the volume spec containing the specification for this volume.
	// Used to generate the volume plugin object, and passed to attach/detach
	// methods.
	spec *volume.Spec

	// scheduledPods is a map containing the set of pods that reference this
	// volume and are scheduled to the underlying node. The key in the map is
	// the name of the pod and the value is a pod object containing more
	// information about the pod.
	scheduledPods map[types.UniquePodName]pod //引用了attached的卷，且已经调度某个节点上的Pod.在scheduledPods最后一个Pod被删除时
	// volume也会从nodeManaged对应的节点volumesToAttach表中移除。
}

// The pod represents a pod that references the underlying volume and is
// scheduled to the underlying node.
type pod struct {
	// podName contains the unique identifier for this pod
	podName types.UniquePodName

	// pod object contains the api object of pod
	podObj *v1.Pod
}

//初始化节点Attach描述对象信息，添加到节点表中， 已添加则忽略
func (dsw *desiredStateOfWorld) AddNode(nodeName k8stypes.NodeName, keepTerminatedPodVolumes bool) {
	dsw.Lock()
	defer dsw.Unlock()

	if _, nodeExists := dsw.nodesManaged[nodeName]; !nodeExists {
		dsw.nodesManaged[nodeName] = nodeManaged{
			nodeName:                 nodeName,
			volumesToAttach:          make(map[v1.UniqueVolumeName]volumeToAttach),
			keepTerminatedPodVolumes: keepTerminatedPodVolumes,
		}
	}
}

// 如果volumeSpec的插件支持Attach/Detach操作, 则将volume记录到nodeName的AttachedVolume表中，并把podName记录到volume的ScheduledPod表中
// 注意只有已经调度且使用了卷的Pod才会调用这个接口。
func (dsw *desiredStateOfWorld) AddPod(
	podName types.UniquePodName,
	podToAdd *v1.Pod,
	volumeSpec *volume.Spec,
	nodeName k8stypes.NodeName) (v1.UniqueVolumeName, error) {
	dsw.Lock()
	defer dsw.Unlock()

	nodeObj, nodeExists := dsw.nodesManaged[nodeName]
	if !nodeExists {
		return "", fmt.Errorf(
			"no node with the name %q exists in the list of managed nodes",
			nodeName)
	}
	// 根据卷规格找到卷插件，检测卷的插件是否能够支持Attach/Detach操作。 如果能Attach, 返回插件；否则返回nil
	attachableVolumePlugin, err := dsw.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	//找不到插件信息或者插件不支持Attachment, 则报错
	if err != nil || attachableVolumePlugin == nil {
		if attachableVolumePlugin == nil {
			err = fmt.Errorf("plugin do not support attachment")
		}
		return "", fmt.Errorf(
			"failed to get AttachablePlugin from volumeSpec for volume %q err=%v",
			volumeSpec.Name(),
			err)
	}
	//基于卷的插件/注册驱动类型/卷名生成唯一卷名
	volumeName, err := util.GetUniqueVolumeNameFromSpec(
		attachableVolumePlugin, volumeSpec)
	if err != nil {
		return "", fmt.Errorf(
			"failed to get UniqueVolumeName from volumeSpec for plugin=%q and volume=%q err=%v",
			attachableVolumePlugin.GetPluginName(),
			volumeSpec.Name(),
			err)
	}
	// 卷是否已经附加到指定节点上
	volumeObj, volumeExists := nodeObj.volumesToAttach[volumeName]
	// 卷还没有附加到当前节点上
	if !volumeExists {
		volumeObj = volumeToAttach{
			multiAttachErrorReported: false,
			volumeName:               volumeName,
			spec:                     volumeSpec,
			scheduledPods:            make(map[types.UniquePodName]pod),
		}
		// 记录该卷在node的附加信息
		dsw.nodesManaged[nodeName].volumesToAttach[volumeName] = volumeObj
	}
	// 如果Pod未在volume的Pod表中，记录
	if _, podExists := volumeObj.scheduledPods[podName]; !podExists {
		dsw.nodesManaged[nodeName].volumesToAttach[volumeName].scheduledPods[podName] =
			pod{
				podName: podName,
				podObj:  podToAdd,
			}
	}

	return volumeName, nil
}

//将节点从节点attach管理表中移除，如果节点仍有attached的卷，报错不进行移除操作
func (dsw *desiredStateOfWorld) DeleteNode(nodeName k8stypes.NodeName) error {
	dsw.Lock()
	defer dsw.Unlock()

	nodeObj, nodeExists := dsw.nodesManaged[nodeName]
	if !nodeExists {
		return nil
	}

	if len(nodeObj.volumesToAttach) > 0 {
		return fmt.Errorf(
			"failed to delete node %q from list of nodes managed by attach/detach controller--the node still contains %v volumes in its list of volumes to attach",
			nodeName,
			len(nodeObj.volumesToAttach))
	}

	delete(
		dsw.nodesManaged,
		nodeName)
	return nil
}

//找到在指定节点上Attach的卷, 然后将pod从卷的ScheduledPod移除。scheduledPods最后一个Pod被删除时。
// volume也会从nodeManaged对应的节点volumesToAttach表中移除。
func (dsw *desiredStateOfWorld) DeletePod(
	podName types.UniquePodName,
	volumeName v1.UniqueVolumeName,
	nodeName k8stypes.NodeName) {
	dsw.Lock()
	defer dsw.Unlock()

	nodeObj, nodeExists := dsw.nodesManaged[nodeName]
	if !nodeExists {
		return
	}

	volumeObj, volumeExists := nodeObj.volumesToAttach[volumeName]
	if !volumeExists {
		return
	}
	if _, podExists := volumeObj.scheduledPods[podName]; !podExists {
		return
	}

	delete(
		dsw.nodesManaged[nodeName].volumesToAttach[volumeName].scheduledPods,
		podName)
	//如果当前卷在当前节点没有引用的Pod, volume也会从nodeManaged对应的节点volumesToAttach表中移除
	if len(volumeObj.scheduledPods) == 0 {
		delete(
			dsw.nodesManaged[nodeName].volumesToAttach,
			volumeName)
	}
}

func (dsw *desiredStateOfWorld) NodeExists(nodeName k8stypes.NodeName) bool {
	dsw.RLock()
	defer dsw.RUnlock()

	_, nodeExists := dsw.nodesManaged[nodeName]
	return nodeExists
}

func (dsw *desiredStateOfWorld) VolumeExists(
	volumeName v1.UniqueVolumeName, nodeName k8stypes.NodeName) bool {
	dsw.RLock()
	defer dsw.RUnlock()

	nodeObj, nodeExists := dsw.nodesManaged[nodeName]
	if nodeExists {
		if _, volumeExists := nodeObj.volumesToAttach[volumeName]; volumeExists {
			return true
		}
	}

	return false
}

//当卷不支持Attach到多个节点，但之前已经多节点Attach时会设置该标志
func (dsw *desiredStateOfWorld) SetMultiAttachError(
	volumeName v1.UniqueVolumeName,
	nodeName k8stypes.NodeName) {
	dsw.Lock()
	defer dsw.Unlock()

	nodeObj, nodeExists := dsw.nodesManaged[nodeName]
	if nodeExists {
		if volumeObj, volumeExists := nodeObj.volumesToAttach[volumeName]; volumeExists {
			volumeObj.multiAttachErrorReported = true
			dsw.nodesManaged[nodeName].volumesToAttach[volumeName] = volumeObj
		}
	}
}

// GetKeepTerminatedPodVolumesForNode determines if node wants volumes to be
// mounted and attached for terminated pods
func (dsw *desiredStateOfWorld) GetKeepTerminatedPodVolumesForNode(nodeName k8stypes.NodeName) bool {
	dsw.RLock()
	defer dsw.RUnlock()

	if nodeName == "" {
		return false
	}
	if node, ok := dsw.nodesManaged[nodeName]; ok {
		return node.keepTerminatedPodVolumes
	}
	return false
}

//获取所有节点上Attach的卷，有些卷多节点Attach,同样加上
func (dsw *desiredStateOfWorld) GetVolumesToAttach() []VolumeToAttach {
	dsw.RLock()
	defer dsw.RUnlock()

	volumesToAttach := make([]VolumeToAttach, 0 /* len */, len(dsw.nodesManaged) /* cap */)
	for nodeName, nodeObj := range dsw.nodesManaged {
		for volumeName, volumeObj := range nodeObj.volumesToAttach {
			volumesToAttach = append(volumesToAttach,
				VolumeToAttach{
					VolumeToAttach: operationexecutor.VolumeToAttach{
						MultiAttachErrorReported: volumeObj.multiAttachErrorReported,
						VolumeName:               volumeName,
						VolumeSpec:               volumeObj.spec,
						NodeName:                 nodeName,
						ScheduledPods:            getPodsFromMap(volumeObj.scheduledPods),
					}})
		}
	}

	return volumesToAttach
}

// Construct a list of v1.Pod objects from the given pod map
func getPodsFromMap(podMap map[types.UniquePodName]pod) []*v1.Pod {
	pods := make([]*v1.Pod, 0, len(podMap))
	for _, pod := range podMap {
		pods = append(pods, pod.podObj)
	}
	return pods
}

func (dsw *desiredStateOfWorld) GetPodToAdd() map[types.UniquePodName]PodToAdd {
	dsw.RLock()
	defer dsw.RUnlock()

	pods := make(map[types.UniquePodName]PodToAdd)
	for nodeName, nodeObj := range dsw.nodesManaged {
		for volumeName, volumeObj := range nodeObj.volumesToAttach {
			for podUID, pod := range volumeObj.scheduledPods {
				pods[podUID] = PodToAdd{
					Pod:        pod.podObj,
					VolumeName: volumeName,
					NodeName:   nodeName,
				}
			}
		}
	}
	return pods
}

//获得引用了nodes节点列表上attached的卷volumeName且已经调度到某个节点上的Pod列表
func (dsw *desiredStateOfWorld) GetVolumePodsOnNodes(nodes []k8stypes.NodeName, volumeName v1.UniqueVolumeName) []*v1.Pod {
	dsw.RLock()
	defer dsw.RUnlock()

	pods := []*v1.Pod{}
	for _, nodeName := range nodes {
		node, ok := dsw.nodesManaged[nodeName]
		if !ok {
			continue
		}
		volume, ok := node.volumesToAttach[volumeName]
		if !ok {
			continue
		}
		for _, pod := range volume.scheduledPods {
			pods = append(pods, pod.podObj)
		}
	}
	return pods
}
