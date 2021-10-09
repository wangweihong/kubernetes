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
	"time"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
)

// ActualStateOfWorld defines a set of thread-safe operations supported on
// the attach/detach controller's actual state of the world cache.
// This cache contains volumes->nodes i.e. a set of all volumes and the nodes
// the attach/detach controller believes are successfully attached.
// Note: This is distinct from the ActualStateOfWorld implemented by the kubelet
// volume manager. They both keep track of different objects. This contains
// attach/detach controller specific state.
type ActualStateOfWorld interface {
	// ActualStateOfWorld must implement the methods required to allow
	// operationexecutor to interact with it.
	operationexecutor.ActualStateOfWorldAttacherUpdater

	// AddVolumeNode adds the given volume and node to the underlying store.
	// If attached is set to true, it indicates the specified volume is already
	// attached to the specified node. If attached set to false, it means that
	// the volume is not confirmed to be attached to the node yet.
	// A unique volume name is generated from the volumeSpec and returned on
	// success.
	// If volumeSpec is not an attachable volume plugin, an error is returned.
	// If no volume with the name volumeName exists in the store, the volume is
	// added.
	// If no node with the name nodeName exists in list of attached nodes for
	// the specified volume, the node is added.
	AddVolumeNode(uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string, attached bool) (v1.UniqueVolumeName, error)

	// SetVolumeMountedByNode sets the MountedByNode value for the given volume
	// and node. When set to true the mounted parameter indicates the volume
	// is mounted by the given node, indicating it may not be safe to detach.
	// If the forceUnmount is set to true the MountedByNode value would be reset
	// to false even it was not set yet (this is required during a controller
	// crash recovery).
	// If no volume with the name volumeName exists in the store, an error is
	// returned.
	// If no node with the name nodeName exists in list of attached nodes for
	// the specified volume, an error is returned.
	//更新卷/节点的挂载关系
	SetVolumeMountedByNode(volumeName v1.UniqueVolumeName, nodeName types.NodeName, mounted bool) error

	// SetNodeStatusUpdateNeeded sets statusUpdateNeeded for the specified
	// node to true indicating the AttachedVolume field in the Node's Status
	// object needs to be updated by the node updater again.
	// If the specified node does not exist in the nodesToUpdateStatusFor list,
	// log the error and return
	SetNodeStatusUpdateNeeded(nodeName types.NodeName) //设置节点需要更新node.status.AttachedVolume状态。
	//1. 在更新node.status时，获取失败表示

	// ResetDetachRequestTime resets the detachRequestTime to 0 which indicates there is no detach
	// request any more for the volume
	//清掉卷/节点的Detach请求发起时间
	ResetDetachRequestTime(volumeName v1.UniqueVolumeName, nodeName types.NodeName)

	// SetDetachRequestTime sets the detachRequestedTime to current time if this is no
	// previous request (the previous detachRequestedTime is zero) and return the time elapsed
	// since last request
	//当前时间距离Detach请求发起时间过去了多久，如果没有设置Detach发起时间，设置为当前时间
	SetDetachRequestTime(volumeName v1.UniqueVolumeName, nodeName types.NodeName) (time.Duration, error)

	// DeleteVolumeNode removes the given volume and node from the underlying
	// store indicating the specified volume is no longer attached to the
	// specified node.
	// If the volume/node combo does not exist, this is a no-op.
	// If after deleting the node, the specified volume contains no other child
	// nodes, the volume is also deleted.
	DeleteVolumeNode(volumeName v1.UniqueVolumeName, nodeName types.NodeName)

	// GetAttachState returns the attach state for the given volume-node
	// combination.
	// Returns AttachStateAttached if the specified volume/node combo exists in
	// the underlying store indicating the specified volume is attached to the
	// specified node, AttachStateDetached if the combo does not exist, or
	// AttachStateUncertain if the attached state is marked as uncertain.
	//获取卷在指定节点的Attach状态。Attached/Uncertain/Detached
	GetAttachState(volumeName v1.UniqueVolumeName, nodeName types.NodeName) AttachState

	// GetAttachedVolumes generates and returns a list of volumes/node pairs
	// reflecting which volumes might attached to which nodes based on the
	// current actual state of the world. This list includes all the volumes which return successful
	// attach and also the volumes which return errors during attach.
	//获取已经Attached到各个节点上的卷表。Attach多个节点的卷，多次添加到列表中
	GetAttachedVolumes() []AttachedVolume

	// GetAttachedVolumesForNode generates and returns a list of volumes that added to
	// the specified node reflecting which volumes are/or might be attached to that node
	// based on the current actual state of the world. This function is currently used by
	// attach_detach_controller to process VolumeInUse
	GetAttachedVolumesForNode(nodeName types.NodeName) []AttachedVolume

	// GetAttachedVolumesPerNode generates and returns a map of nodes and volumes that added to
	// the specified node reflecting which volumes are attached to that node
	// based on the current actual state of the world. This function is currently used by
	// reconciler to verify whether the volume is still attached to the node.
	//获取actualStateOfWorld中节点上确认Attached的卷列表
	GetAttachedVolumesPerNode() map[types.NodeName][]operationexecutor.AttachedVolume

	// GetNodesForAttachedVolume returns the nodes on which the volume is attached.
	// This function is used by reconciler for multi-attach check.
	// 检测卷Attach的节点列表
	GetNodesForAttachedVolume(volumeName v1.UniqueVolumeName) []types.NodeName

	// GetVolumesToReportAttached returns a map containing the set of nodes for
	// which the VolumesAttached Status field in the Node API object should be
	// updated. The key in this map is the name of the node to update and the
	// value is list of volumes that should be reported as attached (note that
	// this may differ from the actual list of attached volumes for the node
	// since volumes should be removed from this list as soon a detach operation
	// is considered, before the detach operation is triggered).
	GetVolumesToReportAttached() map[types.NodeName][]v1.AttachedVolume

	// GetNodesToUpdateStatusFor returns the map of nodeNames to nodeToUpdateStatusFor
	GetNodesToUpdateStatusFor() map[types.NodeName]nodeToUpdateStatusFor
}

// AttachedVolume represents a volume that is attached to a node.
//用来描述一个Attached节点上的卷
type AttachedVolume struct {
	operationexecutor.AttachedVolume

	// MountedByNode indicates that this volume has been mounted by the node and
	// is unsafe to detach.
	// The value is set and unset by SetVolumeMountedByNode(...).
	MountedByNode bool //卷挂载到节点上

	// DetachRequestedTime is used to capture the desire to detach this volume.
	// When the volume is newly created this value is set to time zero.
	// It is set to current time, when SetDetachRequestTime(...) is called, if it
	// was previously set to zero (other wise its value remains the same).
	// It is reset to zero on ResetDetachRequestTime(...) calls.
	DetachRequestedTime time.Time // 卷/主机 Detach请求发起时间。如果卷挂载在主机上，会等待卷卸载才进行Detach除非超时（默认6小时)
}

// AttachState represents the attach state of a volume to a node known to the
// Actual State of World.
// This type is used as external representation of attach state (specifically
// as the return type of GetAttachState only); the state is represented
// differently in the internal cache implementation.
type AttachState int

const (
	// AttachStateAttached represents the state in which the volume is attached to
	// the node.
	AttachStateAttached AttachState = iota //卷已经在节点发起了Attach动作且已经确定Attach动作成功

	// AttachStateUncertain represents the state in which the Actual State of World
	// does not know whether the volume is attached to the node.
	AttachStateUncertain //卷已经在节点发起了Attach动作，但不确定Attach动作是否成功

	// AttachStateDetached represents the state in which the volume is not
	// attached to the node.
	AttachStateDetached //卷不在Attached表中。
)

func (s AttachState) String() string {
	return []string{"Attached", "Uncertain", "Detached"}[s]
}

// NewActualStateOfWorld returns a new instance of ActualStateOfWorld.
func NewActualStateOfWorld(volumePluginMgr *volume.VolumePluginMgr) ActualStateOfWorld {
	return &actualStateOfWorld{
		attachedVolumes:        make(map[v1.UniqueVolumeName]attachedVolume),
		nodesToUpdateStatusFor: make(map[types.NodeName]nodeToUpdateStatusFor),
		volumePluginMgr:        volumePluginMgr,
	}
}

//提供一个表用来记录controller认为可能已经attached到节点上的卷以及提供一个表供node.status获取节点确认已经成功Attached的卷
// * 注意卷已经Attached到节点，但并不一定Attached成功。需要通过attachedVolume.nodeAttachedTo.attachedConfirmed来确认。
//（如CSI卷, 如果发起Attach动作, csiPlugin会创建VolumeAttachment对象，如果等待到VolumeAttachment.Status.Bind为True,则Attach动作认为已确定成功.
// 如果超时，这时候不确定Attach动作到底有没有成功)
type actualStateOfWorld struct {
	// attachedVolumes is a map containing the set of volumes the attach/detach
	// controller believes to be successfully attached to the nodes it is
	// managing. The key in this map is the name of the volume and the value is
	// an object containing more information about the attached volume.
	attachedVolumes map[v1.UniqueVolumeName]attachedVolume //控制器认为可能成功Attached到节点的卷。在取消卷在某节点的Attached时,当卷没有Attached的节点，从节点列表移除

	// nodesToUpdateStatusFor is a map containing the set of nodes for which to
	// update the VolumesAttached Status field. The key in this map is the name
	// of the node and the value is an object containing more information about
	// the node (including the list of volumes to report attached).
	nodesToUpdateStatusFor map[types.NodeName]nodeToUpdateStatusFor // 记录节点成功Attached卷信息。Attached卷时添加/ Detached卷时移除
	// 用来更新节点状态的AttachedVolume信息。
	// 只有成功Attached卷才会加入
	// 这个表中的数据将会更新到node.Status.见node_status_updater.go/update
	// volumePluginMgr is the volume plugin manager used to create volume
	// plugin objects.
	volumePluginMgr *volume.VolumePluginMgr //注册的插件列表·

	sync.RWMutex
}

// The volume object represents a volume the attach/detach controller
// believes to be successfully attached to a node it is managing.
//用来描述可能Attached到节点上的卷
type attachedVolume struct {
	// volumeName contains the unique identifier for this volume.
	volumeName v1.UniqueVolumeName //基于卷插件/驱动/卷名生成的唯一名如 csiPlugin: kubernetes.io/csi/<csiDriverName>^volumeName

	// spec is the volume spec containing the specification for this volume.
	// Used to generate the volume plugin object, and passed to attach/detach
	// methods.
	spec *volume.Spec

	// nodesAttachedTo is a map containing the set of nodes this volume has
	// been attached to. The key in this map is the name of the
	// node and the value is a node object containing more information about
	// the node.
	nodesAttachedTo map[types.NodeName]nodeAttachedTo //卷发起attach动作的节点列表
	//需要根据nodeAttachedTo才能知道Attach动作是否确认成功, 也能根据nodeAttachedTo知道卷没有挂载到节点上

	// devicePath contains the path on the node where the volume is attached
	devicePath string //如果是块设备，这里应该是块设备的设备路径
	//CSI插件 devicePath是空的.
}

// The nodeAttachedTo object represents a node that has volumes attached to it
// or trying to attach to it.
//用来描述卷Attached的节点
//通过attachedConfirmed可以判断卷是否确定Attached到节点上, mountedByNode用来确认卷有没有挂载到节点
type nodeAttachedTo struct {
	// nodeName contains the name of this node.
	nodeName types.NodeName

	// mountedByNode indicates that this node/volume combo is mounted by the
	// node and is unsafe to detach
	mountedByNode bool // 卷是否已经挂载到节点上

	// attachConfirmed indicates that the storage system verified the volume has been attached to this node.
	// This value is set to false when an attach  operation fails and the volume may be attached or not.
	attachedConfirmed bool // 确认卷成功完成Attached. 见GetAttachState()根据这个标志来确认是否已经成功Attached。
	// 如CSI插件， attach动作完成指的是csi插件创建volumeAttachment,且在指定时间内volumeAttachment.Status.Bind为true。
	// 如果超时，插件不能确认Attach动作是否成功。 则需要等待之后发起确认
	// detachRequestedTime used to capture the desire to detach this volume
	detachRequestedTime time.Time // 卷/节点Detach请求发起时间
}

// nodeToUpdateStatusFor is an object that reflects a node that has one or more
// volume attached. It keeps track of the volumes that should be reported as
// attached in the Node's Status API object.
// 这个结构体用来告知node status updater是否要更新nodeName节点的status.volumeAttach对象.(用来告知使用者当前节点Attached了哪些卷)
type nodeToUpdateStatusFor struct {
	// nodeName contains the name of this node.
	nodeName types.NodeName

	// statusUpdateNeeded indicates that the value of the VolumesAttached field
	// in the Node's Status API object should be updated. This should be set to
	// true whenever a volume is added or deleted from
	// volumesToReportAsAttached. It should be reset whenever the status is
	// updated.
	statusUpdateNeeded bool //node status updater会根据这个标志来决定是否更新node.status.volumeAttached, 即卷的实际表 1. 当nodeToUpdateStatusFor提交给node_status_updater更新node对象数据后，statusUpdateNeeded设置为 false.
	// 2. node_status_updater如果更新node状态失败，重新设置statusUpdateNeeded为true准备重试

	// volumesToReportAsAttached is the list of volumes that should be reported
	// as attached in the Node's status (note that this may differ from the
	// actual list of attached volumes since volumes should be removed from this
	// list as soon a detach operation is considered, before the detach
	// operation is triggered).
	volumesToReportAsAttached map[v1.UniqueVolumeName]v1.UniqueVolumeName //报告有哪些卷Attached到节点上。通过node.status.volumeAttached可以看到
}

// 添加卷到已Attached表，并记录与节点的Attached关系。但不确认是否真的的Attached
func (asw *actualStateOfWorld) MarkVolumeAsUncertain(
	uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName) error {

	_, err := asw.AddVolumeNode(uniqueName, volumeSpec, nodeName, "", false /* isAttached */)
	return err
}

// 添加卷到已Attached表，并记录与节点的Attached关系。已确认节点Attached
func (asw *actualStateOfWorld) MarkVolumeAsAttached(
	uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string) error {
	_, err := asw.AddVolumeNode(uniqueName, volumeSpec, nodeName, devicePath, true)
	return err
}

//将卷和节点的Attached关系从表中清除。如果卷不再有Attached的节点，也移除卷。
func (asw *actualStateOfWorld) MarkVolumeAsDetached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.DeleteVolumeNode(volumeName, nodeName)
}

func (asw *actualStateOfWorld) RemoveVolumeFromReportAsAttached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) error {
	asw.Lock()
	defer asw.Unlock()
	return asw.removeVolumeFromReportAsAttached(volumeName, nodeName)
}

func (asw *actualStateOfWorld) AddVolumeToReportAsAttached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()
	asw.addVolumeToReportAsAttached(volumeName, nodeName)
}

//如果卷插件不支持Attach, 则报错。记录卷和节点的Attach关系.
//isAttached为true才表示卷确定已经Attached到节点上。
func (asw *actualStateOfWorld) AddVolumeNode(
	uniqueName v1.UniqueVolumeName, volumeSpec *volume.Spec, nodeName types.NodeName, devicePath string, isAttached bool) (v1.UniqueVolumeName, error) {
	volumeName := uniqueName
	if volumeName == "" {
		if volumeSpec == nil {
			return volumeName, fmt.Errorf("volumeSpec cannot be nil if volumeName is empty")
		}
		// 根据卷规格找到卷插件，检测卷的插件是否能够支持Attach/Detach操作。 如果能Attach, 返回插件；否则返回nil
		attachableVolumePlugin, err := asw.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
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
		volumeName, err = util.GetUniqueVolumeNameFromSpec(
			attachableVolumePlugin, volumeSpec)
		if err != nil {
			return "", fmt.Errorf(
				"failed to GetUniqueVolumeNameFromSpec for volumeSpec %q err=%v",
				volumeSpec.Name(),
				err)
		}
	}

	asw.Lock()
	defer asw.Unlock()
	//卷是否已经实际attached到节点上
	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		volumeObj = attachedVolume{
			volumeName:      volumeName,
			spec:            volumeSpec,
			nodesAttachedTo: make(map[types.NodeName]nodeAttachedTo),
			devicePath:      devicePath,
		}
	} else {
		// If volume object already exists, it indicates that the information would be out of date.
		// Update the fields for volume object except the nodes attached to the volumes.
		volumeObj.devicePath = devicePath //
		volumeObj.spec = volumeSpec
		klog.V(2).Infof("Volume %q is already added to attachedVolume list to node %q, update device path %q",
			volumeName,
			nodeName,
			devicePath)
	}
	//卷是否已经Attached到nodeName上
	node, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if !nodeExists {
		// Create object if it doesn't exist.
		node = nodeAttachedTo{
			nodeName:            nodeName,
			mountedByNode:       true,       // Assume mounted, until proven otherwise
			attachedConfirmed:   isAttached, // 是否已经成功Attached
			detachRequestedTime: time.Time{},
		}
	} else {
		node.attachedConfirmed = isAttached
		klog.V(5).Infof("Volume %q is already added to attachedVolume list to the node %q, the current attach state is %t",
			volumeName,
			nodeName,
			isAttached)
	}

	volumeObj.nodesAttachedTo[nodeName] = node
	asw.attachedVolumes[volumeName] = volumeObj
	//如果已经Attached
	if isAttached {
		//加入到节点报告列表，卷的信息将会被更细拿到node.Status.VolumeAttached表中
		asw.addVolumeToReportAsAttached(volumeName, nodeName)
	}
	return volumeName, nil
}

//更新卷/节点的挂载标志：卷已经挂载到节点上
func (asw *actualStateOfWorld) SetVolumeMountedByNode(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName, mounted bool) error {
	asw.Lock()
	defer asw.Unlock()
	//如果卷Attached到节点上，返回卷和节点的信息
	volumeObj, nodeObj, err := asw.getNodeAndVolume(volumeName, nodeName)
	if err != nil {
		return fmt.Errorf("failed to SetVolumeMountedByNode with error: %v", err)
	}

	nodeObj.mountedByNode = mounted
	volumeObj.nodesAttachedTo[nodeName] = nodeObj
	klog.V(4).Infof("SetVolumeMountedByNode volume %v to the node %q mounted %t",
		volumeName,
		nodeName,
		mounted)
	return nil
}

//清掉卷/节点的Detach请求发起时间，不再进行detach
func (asw *actualStateOfWorld) ResetDetachRequestTime(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, nodeObj, err := asw.getNodeAndVolume(volumeName, nodeName)
	if err != nil {
		klog.Errorf("Failed to ResetDetachRequestTime with error: %v", err)
		return
	}
	nodeObj.detachRequestedTime = time.Time{}
	volumeObj.nodesAttachedTo[nodeName] = nodeObj
}

//当前时间距离Detach请求发起时间过去了多久，如果没有设置Detach发起时间，设置为当前时间
func (asw *actualStateOfWorld) SetDetachRequestTime(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) (time.Duration, error) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, nodeObj, err := asw.getNodeAndVolume(volumeName, nodeName)
	if err != nil {
		return 0, fmt.Errorf("failed to set detach request time with error: %v", err)
	}
	// If there is no previous detach request, set it to the current time
	if nodeObj.detachRequestedTime.IsZero() {
		nodeObj.detachRequestedTime = time.Now()
		volumeObj.nodesAttachedTo[nodeName] = nodeObj
		klog.V(4).Infof("Set detach request time to current time for volume %v on node %q",
			volumeName,
			nodeName)
	}
	return time.Since(nodeObj.detachRequestedTime), nil
}

// Get the volume and node object from actual state of world
// This is an internal function and caller should acquire and release the lock
//
// Note that this returns disconnected objects, so if you change the volume object you must set it back with
// `asw.attachedVolumes[volumeName]=volumeObj`.
//
// If you change the node object you must use `volumeObj.nodesAttachedTo[nodeName] = nodeObj`
// This is correct, because if volumeObj is empty this function returns an error, and nodesAttachedTo
// map is a reference type, and thus mutating the copy changes the original map.
//如果卷Attached到节点上，返回卷和节点的信息
func (asw *actualStateOfWorld) getNodeAndVolume(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) (attachedVolume, nodeAttachedTo, error) {

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if volumeExists {
		nodeObj, nodeExists := volumeObj.nodesAttachedTo[nodeName]
		if nodeExists {
			return volumeObj, nodeObj, nil
		}
	}

	return attachedVolume{}, nodeAttachedTo{}, fmt.Errorf("volume %v is no longer attached to the node %q",
		volumeName,
		nodeName)
}

// Remove the volumeName from the node's volumesToReportAsAttached list
// This is an internal function and caller should acquire and release the lock
func (asw *actualStateOfWorld) removeVolumeFromReportAsAttached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) error {

	nodeToUpdate, nodeToUpdateExists := asw.nodesToUpdateStatusFor[nodeName]
	if nodeToUpdateExists {
		_, nodeToUpdateVolumeExists :=
			nodeToUpdate.volumesToReportAsAttached[volumeName]
		if nodeToUpdateVolumeExists {
			nodeToUpdate.statusUpdateNeeded = true
			delete(nodeToUpdate.volumesToReportAsAttached, volumeName)
			asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate
			return nil
		}
	}
	return fmt.Errorf("volume %q does not exist in volumesToReportAsAttached list or node %q does not exist in nodesToUpdateStatusFor list",
		volumeName,
		nodeName)

}

// Add the volumeName to the node's volumesToReportAsAttached list
// This is an internal function and caller should acquire and release the lock
//添加节点/卷Attached信息, 提示要更新node.Status
//statusUpdater会周期将确认Attached的卷的信息更新到对应节点的status.VolumeAttached表中。
func (asw *actualStateOfWorld) addVolumeToReportAsAttached(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	// In case the volume/node entry is no longer in attachedVolume list, skip the rest
	// 是否volume仍然Attached到nodeName
	if _, _, err := asw.getNodeAndVolume(volumeName, nodeName); err != nil {
		klog.V(4).Infof("Volume %q is no longer attached to node %q", volumeName, nodeName)
		return
	}
	nodeToUpdate, nodeToUpdateExists := asw.nodesToUpdateStatusFor[nodeName]
	if !nodeToUpdateExists {
		// Create object if it doesn't exist
		nodeToUpdate = nodeToUpdateStatusFor{
			nodeName:                  nodeName,
			statusUpdateNeeded:        true,
			volumesToReportAsAttached: make(map[v1.UniqueVolumeName]v1.UniqueVolumeName),
		}
		asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate
		klog.V(4).Infof("Add new node %q to nodesToUpdateStatusFor", nodeName)
	}
	_, nodeToUpdateVolumeExists :=
		nodeToUpdate.volumesToReportAsAttached[volumeName]
	if !nodeToUpdateVolumeExists {
		nodeToUpdate.statusUpdateNeeded = true
		nodeToUpdate.volumesToReportAsAttached[volumeName] = volumeName
		asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate
		klog.V(4).Infof("Report volume %q as attached to node %q", volumeName, nodeName)
	}
}

// Update the flag statusUpdateNeeded to indicate whether node status is already updated or
// needs to be updated again by the node status updater.
// If the specified node does not exist in the nodesToUpdateStatusFor list, log the error and return
// This is an internal function and caller should acquire and release the lock
//设置节点的卷Attached信息是否需要更新到node.status的标志
func (asw *actualStateOfWorld) updateNodeStatusUpdateNeeded(nodeName types.NodeName, needed bool) error {
	nodeToUpdate, nodeToUpdateExists := asw.nodesToUpdateStatusFor[nodeName]
	if !nodeToUpdateExists {
		// should not happen
		errMsg := fmt.Sprintf("Failed to set statusUpdateNeeded to needed %t, because nodeName=%q does not exist",
			needed, nodeName)
		return fmt.Errorf(errMsg)
	}

	nodeToUpdate.statusUpdateNeeded = needed
	asw.nodesToUpdateStatusFor[nodeName] = nodeToUpdate

	return nil
}

//设置节点的卷Attached信息是否需要更新到node.status的标志
func (asw *actualStateOfWorld) SetNodeStatusUpdateNeeded(nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()
	if err := asw.updateNodeStatusUpdateNeeded(nodeName, true); err != nil {
		klog.Warningf("Failed to update statusUpdateNeeded field in actual state of world: %v", err)
	}
}

//将卷和节点的Attached关系从表中清除。如果卷不再有Attached的节点，也移除卷。
func (asw *actualStateOfWorld) DeleteVolumeNode(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return
	}

	_, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if nodeExists {
		delete(asw.attachedVolumes[volumeName].nodesAttachedTo, nodeName)
	}

	if len(volumeObj.nodesAttachedTo) == 0 {
		delete(asw.attachedVolumes, volumeName)
	}

	// Remove volume from volumes to report as attached
	asw.removeVolumeFromReportAsAttached(volumeName, nodeName)
}

//获取卷在指定节点的Attach状态。Attached/Uncertain/Detached
func (asw *actualStateOfWorld) GetAttachState(
	volumeName v1.UniqueVolumeName, nodeName types.NodeName) AttachState {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if volumeExists {
		if node, nodeExists := volumeObj.nodesAttachedTo[nodeName]; nodeExists {
			//根据标志来确认成功Attached
			if node.attachedConfirmed {
				return AttachStateAttached
			}
			return AttachStateUncertain
		}
	}
	// 当卷没有与任何节点的Attached时
	return AttachStateDetached
}

//获取已经Attached到各个节点上的卷表。Attach多个节点的卷，多次添加到列表中
func (asw *actualStateOfWorld) GetAttachedVolumes() []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumes := make([]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		for _, nodeObj := range volumeObj.nodesAttachedTo {
			attachedVolumes = append(
				attachedVolumes,
				getAttachedVolume(&volumeObj, &nodeObj))
		}
	}

	return attachedVolumes
}

//
func (asw *actualStateOfWorld) GetAttachedVolumesForNode(
	nodeName types.NodeName) []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumes := make(
		[]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		if nodeObj, nodeExists := volumeObj.nodesAttachedTo[nodeName]; nodeExists {
			attachedVolumes = append(
				attachedVolumes,
				getAttachedVolume(&volumeObj, &nodeObj))
		}
	}

	return attachedVolumes
}

//获取actualStateOfWorld中节点上确认Attached的卷列表
func (asw *actualStateOfWorld) GetAttachedVolumesPerNode() map[types.NodeName][]operationexecutor.AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumesPerNode := make(map[types.NodeName][]operationexecutor.AttachedVolume)
	for _, volumeObj := range asw.attachedVolumes {
		for nodeName, nodeObj := range volumeObj.nodesAttachedTo {
			if nodeObj.attachedConfirmed {
				volumes := attachedVolumesPerNode[nodeName]
				volumes = append(volumes, getAttachedVolume(&volumeObj, &nodeObj).AttachedVolume)
				attachedVolumesPerNode[nodeName] = volumes
			}
		}
	}

	return attachedVolumesPerNode
}

//获取卷已经成功Attached的节点
func (asw *actualStateOfWorld) GetNodesForAttachedVolume(volumeName v1.UniqueVolumeName) []types.NodeName {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists || len(volumeObj.nodesAttachedTo) == 0 {
		return []types.NodeName{}
	}

	nodes := []types.NodeName{}
	for nodeName, nodesAttached := range volumeObj.nodesAttachedTo {
		//卷已经成功Attached
		if nodesAttached.attachedConfirmed {
			nodes = append(nodes, nodeName)
		}
	}
	return nodes
}

//获取所有节点的成功Attached卷的信息，同时设置这些节点的statusUpdateNeeded为 false.因为这个接口是提交给node_status_updater来指定节点进行更新node状态。
//		//如果某个节点更新失败，会重试设置statusUpdateNeeded为true
func (asw *actualStateOfWorld) GetVolumesToReportAttached() map[types.NodeName][]v1.AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	volumesToReportAttached := make(map[types.NodeName][]v1.AttachedVolume)
	for nodeName, nodeToUpdateObj := range asw.nodesToUpdateStatusFor {
		if nodeToUpdateObj.statusUpdateNeeded {
			attachedVolumes := make(
				[]v1.AttachedVolume,
				0,
				len(nodeToUpdateObj.volumesToReportAsAttached) /* len */)
			for _, volume := range nodeToUpdateObj.volumesToReportAsAttached {
				attachedVolumes = append(attachedVolumes,
					v1.AttachedVolume{
						Name:       volume,
						DevicePath: asw.attachedVolumes[volume].devicePath,
					})
			}
			volumesToReportAttached[nodeToUpdateObj.nodeName] = attachedVolumes
		}
		// When GetVolumesToReportAttached is called by node status updater, the current status
		// of this node will be updated, so set the flag statusUpdateNeeded to false indicating
		// the current status is already updated.
		//设置为False因为这个接口是提交给node_status_updater进行更新node状态。
		//如果某个节点更新失败，会重试设置对应节点的statusUpdateNeeded为true
		if err := asw.updateNodeStatusUpdateNeeded(nodeName, false); err != nil {
			klog.Errorf("Failed to update statusUpdateNeeded field when getting volumes: %v", err)
		}
	}

	return volumesToReportAttached
}

func (asw *actualStateOfWorld) GetNodesToUpdateStatusFor() map[types.NodeName]nodeToUpdateStatusFor {
	return asw.nodesToUpdateStatusFor
}

func getAttachedVolume(
	attachedVolume *attachedVolume,
	nodeAttachedTo *nodeAttachedTo) AttachedVolume {
	return AttachedVolume{
		AttachedVolume: operationexecutor.AttachedVolume{
			VolumeName:         attachedVolume.volumeName,
			VolumeSpec:         attachedVolume.spec,
			NodeName:           nodeAttachedTo.nodeName,
			DevicePath:         attachedVolume.devicePath,
			PluginIsAttachable: true,
		},
		MountedByNode:       nodeAttachedTo.mountedByNode,
		DetachRequestedTime: nodeAttachedTo.detachRequestedTime}
}
