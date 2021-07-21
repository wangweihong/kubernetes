/*
Copyright 2018 The Kubernetes Authors.

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

// Package nodeinfomanager includes internal functions used to add/delete labels to
// kubernetes nodes for corresponding CSI drivers
package nodeinfomanager // import "k8s.io/kubernetes/pkg/volume/csi/nodeinfomanager"

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"math"
	"strings"

	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/features"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
)

const (
	// Name of node annotation that contains JSON map of driver names to node
	annotationKeyNodeID = "csi.volume.kubernetes.io/nodeid" //在kubelet注册CSIdriver的信息将会存储到node.annotation的该键中
)

// 这个包主要功能是
// 1. 提供kubelet创建CSINode的功能
// 2. 在添加CSIDriver时，将CSIDriver信息添加到CSINode.spec以及node的annotaion["csi.volume.kubernetes.io/nodeid"]
//		将CSIDriver的Topology键值更新node的Label中
// 3. 卸载CSIDriver时，将以上信息从CSINode以及node中移除。
var (
	//node的GVK
	nodeKind      = v1.SchemeGroupVersion.WithKind("Node")
	updateBackoff = wait.Backoff{
		Steps:    4,                     //Duration剩余更新次数
		Duration: 10 * time.Millisecond, //10毫秒
		Factor:   5.0,                   //每次退避后，Duration=Duration*5
		Jitter:   0.1,                   //误差，
	}
)

// nodeInfoManager contains necessary common dependencies to update node info on both
// the Node and CSINode objects.·
type nodeInfoManager struct {
	nodeName        types.NodeName           //kubelet节点名（通过这个名字可以获取node/CSINode对象)
	volumeHost      volume.VolumeHost        //这里有好几种实现：如果是kubelet,这里的实现是volume.KubeletVolumeHost
	migratedPlugins map[string](func() bool) //用来迁移旧版本插件到CSI
}

// If no updates is needed, the function must return the same Node object as the input.
type nodeUpdateFunc func(*v1.Node) (newNode *v1.Node, updated bool, err error)

// Interface implements an interface for managing labels of a node
type Interface interface {
	//创建CSINode
	CreateCSINode() (*storagev1.CSINode, error)

	// Updates or Creates the CSINode object with annotations for CSI Migration
	// 创建CSINode(如果不存在), 并把迁移信息添加到CSINode的annotation中
	InitializeCSINodeWithAnnotation() error

	// Record in the cluster the given node information from the CSI driver with the given name.
	// Concurrent calls to InstallCSIDriver() is allowed, but they should not be intertwined with calls
	// to other methods in this interface.
	//1. 将CSIDriver的driverName以及driverNode记录到node的Annotation中，将 accessibleTopology更新到node的label中
	//2. 将CSIDriver的driverName以及driverNode，accessibleTopology更新到CSINode.spec(不存在CSINode则创建)中
	InstallCSIDriver(driverName string, driverNodeID string, maxVolumeLimit int64, topology map[string]string) error

	// Remove in the cluster node information from the CSI driver with the given name.
	// Concurrent calls to UninstallCSIDriver() is allowed, but they should not be intertwined with calls
	// to other methods in this interface.
	//1.将特定的CSIDriver从CSINode.spec列表中移除
	//2.移除node.status.allocatable以及node.status.capacity中指定驱动最大卷挂载数量信息
	//3.将csi driver从node的annotation "csi.volume.kubernetes.io/nodeid"中移除
	UninstallCSIDriver(driverName string) error
}

// NewNodeInfoManager initializes nodeInfoManager
// 在CSIPluginManager初始化时创建
func NewNodeInfoManager(
	nodeName types.NodeName,
	volumeHost volume.VolumeHost,
	migratedPlugins map[string](func() bool)) Interface {
	return &nodeInfoManager{
		nodeName:        nodeName,
		volumeHost:      volumeHost,
		migratedPlugins: migratedPlugins,
	}
}

// InstallCSIDriver updates the node ID annotation in the Node object and CSIDrivers field in the
// CSINode object. If the CSINode object doesn't yet exist, it will be created.
// If multiple calls to InstallCSIDriver() are made in parallel, some calls might receive Node or
// CSINode update conflicts, which causes the function to retry the corresponding update.
//1. 将CSIDriver的driverName以及driverNode记录到node的Annotation中，将 topology更新到node的label中
//2. 将CSIDriver的driverName以及driverNode，topology更新到CSINode.spec中
func (nim *nodeInfoManager) InstallCSIDriver(driverName string, driverNodeID string, maxAttachLimit int64, topology map[string]string) error {
	if driverNodeID == "" {
		return fmt.Errorf("error adding CSI driver node info: driverNodeID must not be empty")
	}

	nodeUpdateFuncs := []nodeUpdateFunc{
		//将csiDriver以及csiDriverNodeID信息保存在node的Annotation中
		updateNodeIDInNode(driverName, driverNodeID),
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.CSINodeInfo) {
		//1.检测topology的键值对和node的label是否有冲突（键一致，值不一致) 2.将topology的键值更新到node的label上
		nodeUpdateFuncs = append(nodeUpdateFuncs, updateTopologyLabels(topology))
	}

	//更新节点。如果失败，退避一段时间后再重试（每次退避后，退避时间将会延长)
	// 1.将csiDriver以及csiDriverNodeID信息保存在node的Annotation中
	// 2.检测topology的键值对和node的label是否有冲突（键一致，值不一致) ,将topology的键值更新到node的label上
	err := nim.updateNode(nodeUpdateFuncs...)
	if err != nil {
		return fmt.Errorf("error updating Node object with CSI driver node info: %v", err)
	}

	//1.17版本后这个特性默认是启动的
	if utilfeature.DefaultFeatureGate.Enabled(features.CSINodeInfo) {
		//将csiDriver的信息更新到CSINode中, 如果失败退避一段时间后再重试。
		err = nim.updateCSINode(driverName, driverNodeID, maxAttachLimit, topology)
		if err != nil {
			return fmt.Errorf("error updating CSINode object with CSI driver node info: %v", err)
		}
	}
	return nil
}

// UninstallCSIDriver removes the node ID annotation from the Node object and CSIDrivers field from the
// CSINode object. If the CSINOdeInfo object contains no CSIDrivers, it will be deleted.
// If multiple calls to UninstallCSIDriver() are made in parallel, some calls might receive Node or
// CSINode update conflicts, which causes the function to retry the corresponding update.
//1.将特定的CSIDriver从CSINode.spec列表中移除
//2.移除node.status.allocatable以及node.status.capacity中指定驱动最大卷挂载数量信息
//3.将csi driver从node的annotation "csi.volume.kubernetes.io/nodeid"中移除
func (nim *nodeInfoManager) UninstallCSIDriver(driverName string) error {
	if utilfeature.DefaultFeatureGate.Enabled(features.CSINodeInfo) {
		//将特定的CSIDriver从CSINode.spec列表中移除
		err := nim.uninstallDriverFromCSINode(driverName)
		if err != nil {
			return fmt.Errorf("error uninstalling CSI driver from CSINode object %v", err)
		}
	}

	err := nim.updateNode(
		//移除node.status.allocatable以及node.status.capacity中指定驱动最大卷挂载数量信息
		removeMaxAttachLimit(driverName),
		// 将csi driver从node的annotation "csi.volume.kubernetes.io/nodeid"中移除
		removeNodeIDFromNode(driverName),
	)
	if err != nil {
		return fmt.Errorf("error removing CSI driver node info from Node object %v", err)
	}
	return nil
}

//更新节点。如果失败，退避一段时间后再重试（每次退避后，退避时间将会延长)
func (nim *nodeInfoManager) updateNode(updateFuncs ...nodeUpdateFunc) error {
	var updateErrs []error
	//对nim中节点名对应的节点进行updateFuncs
	//如果失败，退避一段时间后再重试（每次退避后，退避时间将会延长)
	err := wait.ExponentialBackoff(updateBackoff, func() (bool, error) {
		if err := nim.tryUpdateNode(updateFuncs...); err != nil {
			updateErrs = append(updateErrs, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("error updating node: %v; caused by: %v", err, utilerrors.NewAggregate(updateErrs))
	}
	return nil
}

// updateNode repeatedly attempts to update the corresponding node object
// which is modified by applying the given update functions sequentially.
// Because updateFuncs are applied sequentially, later updateFuncs should take into account
// the effects of previous updateFuncs to avoid potential conflicts. For example, if multiple
// functions update the same field, updates in the last function are persisted.
// 通过updateFuncs来更新apiserver的node对象
func (nim *nodeInfoManager) tryUpdateNode(updateFuncs ...nodeUpdateFunc) error {
	// Retrieve the latest version of Node before attempting update, so that
	// existing changes are not overwritten.

	kubeClient := nim.volumeHost.GetKubeClient()
	if kubeClient == nil {
		return fmt.Errorf("error getting kube client")
	}

	nodeClient := kubeClient.CoreV1().Nodes()
	originalNode, err := nodeClient.Get(context.TODO(), string(nim.nodeName), metav1.GetOptions{})
	if err != nil {
		return err
	}
	node := originalNode.DeepCopy()

	needUpdate := false
	for _, update := range updateFuncs {
		newNode, updated, err := update(node)
		if err != nil {
			return err
		}
		node = newNode
		needUpdate = needUpdate || updated
	}

	if needUpdate {
		// PatchNodeStatus can update both node's status and labels or annotations
		// Updating status by directly updating node does not work
		_, _, updateErr := nodeutil.PatchNodeStatus(kubeClient.CoreV1(), types.NodeName(node.Name), originalNode, node)
		return updateErr
	}

	return nil
}

// Guarantees the map is non-nil if no error is returned.
///从v1.Node中提取出注册的驱动
func buildNodeIDMapFromAnnotation(node *v1.Node) (map[string]string, error) {
	var previousAnnotationValue string
	//csi.volume.kubernetes.io/nodeid
	// CSINode.spec中驱动信息会记录在node.Annotations中
	//csi.volume.kubernetes.io/nodeid: '{"rbd.csi.ceph.com":"haaaa-10-30-100-27","topsc.topke.io":"{\"HostName\":\"topke\"}"}'
	if node.ObjectMeta.Annotations != nil {
		previousAnnotationValue =
			node.ObjectMeta.Annotations[annotationKeyNodeID]
	}

	var existingDriverMap map[string]string
	if previousAnnotationValue != "" {
		// Parse previousAnnotationValue as JSON
		if err := json.Unmarshal([]byte(previousAnnotationValue), &existingDriverMap); err != nil {
			return nil, fmt.Errorf(
				"failed to parse node's %q annotation value (%q) err=%v",
				annotationKeyNodeID,
				previousAnnotationValue,
				err)
		}
	}

	if existingDriverMap == nil {
		return make(map[string]string), nil
	}
	return existingDriverMap, nil
}

// updateNodeIDInNode returns a function that updates a Node object with the given
// Node ID information.
// 如果csiDriver已经存在node的Annotation中，从中提取对应的csiDriverNodeID
// 否则将新的csiDriver以及csiDriverNodeID信息保存在node的Annotation中
func updateNodeIDInNode(
	csiDriverName string,
	csiDriverNodeID string) nodeUpdateFunc {
	return func(node *v1.Node) (*v1.Node, bool, error) {
		//从node.Annotation中获取节点上csi driver信息
		existingDriverMap, err := buildNodeIDMapFromAnnotation(node)
		if err != nil {
			return nil, false, err
		}
		// 指定驱动是否已经存在，如果存在，返回csiNodeID
		if val, ok := existingDriverMap[csiDriverName]; ok {
			if val == csiDriverNodeID {
				// Value already exists in node annotation, nothing more to do
				return node, false, nil
			}
		}

		// Add/update annotation value
		//将新的csi driver的信息同样保存在node的Annotation中
		existingDriverMap[csiDriverName] = csiDriverNodeID
		jsonObj, err := json.Marshal(existingDriverMap)
		if err != nil {
			return nil, false, fmt.Errorf(
				"error while marshalling node ID map updated with driverName=%q, nodeID=%q: %v",
				csiDriverName,
				csiDriverNodeID,
				err)
		}

		if node.ObjectMeta.Annotations == nil {
			node.ObjectMeta.Annotations = make(map[string]string)
		}
		node.ObjectMeta.Annotations[annotationKeyNodeID] = string(jsonObj)

		return node, true, nil
	}
}

// removeNodeIDFromNode returns a function that removes node ID information matching the given
// driver name from a Node object.
// 将csi driver从node的annotation "csi.volume.kubernetes.io/nodeid"中移除
func removeNodeIDFromNode(csiDriverName string) nodeUpdateFunc {
	return func(node *v1.Node) (*v1.Node, bool, error) {
		var previousAnnotationValue string
		if node.ObjectMeta.Annotations != nil {
			previousAnnotationValue =
				node.ObjectMeta.Annotations[annotationKeyNodeID]
		}

		if previousAnnotationValue == "" {
			return node, false, nil
		}

		// Parse previousAnnotationValue as JSON
		existingDriverMap := map[string]string{}
		if err := json.Unmarshal([]byte(previousAnnotationValue), &existingDriverMap); err != nil {
			return nil, false, fmt.Errorf(
				"failed to parse node's %q annotation value (%q) err=%v",
				annotationKeyNodeID,
				previousAnnotationValue,
				err)
		}

		if _, ok := existingDriverMap[csiDriverName]; !ok {
			// Value is already missing in node annotation, nothing more to do
			return node, false, nil
		}

		// Delete annotation value
		delete(existingDriverMap, csiDriverName)
		if len(existingDriverMap) == 0 {
			delete(node.ObjectMeta.Annotations, annotationKeyNodeID)
		} else {
			jsonObj, err := json.Marshal(existingDriverMap)
			if err != nil {
				return nil, false, fmt.Errorf(
					"failed while trying to remove key %q from node %q annotation. Existing data: %v",
					csiDriverName,
					annotationKeyNodeID,
					previousAnnotationValue)
			}

			node.ObjectMeta.Annotations[annotationKeyNodeID] = string(jsonObj)
		}

		return node, true, nil
	}
}

// updateTopologyLabels returns a function that updates labels of a Node object with the given
// topology information.
//1.检测topology的键值对和node的label是否有冲突（键一致，值不一致) 2.将topololy的键值更新到node的label上
func updateTopologyLabels(topology map[string]string) nodeUpdateFunc {
	return func(node *v1.Node) (*v1.Node, bool, error) {
		if topology == nil || len(topology) == 0 {
			return node, false, nil
		}

		//如果拓扑不为空,比对拓扑和node的标签是否冲突
		for k, v := range topology {
			//比对拓扑中的键值对和node.labels是否一致（冲突则报错）
			if curVal, exists := node.Labels[k]; exists && curVal != v {
				return nil, false, fmt.Errorf("detected topology value collision: driver reported %q:%q but existing label is %q:%q", k, v, k, curVal)
			}
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		//将拓扑键值对添加到节点中
		for k, v := range topology {
			node.Labels[k] = v
		}
		return node, true, nil
	}
}

//将csiDriver的信息更新到CSINode中, 如果失败退避一段时间后再重试。
func (nim *nodeInfoManager) updateCSINode(
	driverName string,
	driverNodeID string,
	maxAttachLimit int64,
	topology map[string]string) error {

	csiKubeClient := nim.volumeHost.GetKubeClient()
	if csiKubeClient == nil {
		return fmt.Errorf("error getting CSI client")
	}

	var updateErrs []error
	err := wait.ExponentialBackoff(updateBackoff, func() (bool, error) {
		//将csiDriver的信息更新到CSINode中
		if err := nim.tryUpdateCSINode(csiKubeClient, driverName, driverNodeID, maxAttachLimit, topology); err != nil {
			updateErrs = append(updateErrs, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("error updating CSINode: %v; caused by: %v", err, utilerrors.NewAggregate(updateErrs))
	}
	return nil
}

//将csi driver的信息更新到CSINode中。
func (nim *nodeInfoManager) tryUpdateCSINode(
	csiKubeClient clientset.Interface,
	driverName string,
	driverNodeID string,
	maxAttachLimit int64,
	topology map[string]string) error {

	nodeInfo, err := csiKubeClient.StorageV1().CSINodes().Get(context.TODO(), string(nim.nodeName), metav1.GetOptions{})
	if nodeInfo == nil || errors.IsNotFound(err) {
		nodeInfo, err = nim.CreateCSINode()
	}
	if err != nil {
		return err
	}

	//将csiDriver的信息更新到CsiNode中
	return nim.installDriverToCSINode(nodeInfo, driverName, driverNodeID, maxAttachLimit, topology)
}

//1. 如果对应的CSINode不存在，则创建CSINode
func (nim *nodeInfoManager) InitializeCSINodeWithAnnotation() error {
	//获取kubelet的apiserver clientset
	csiKubeClient := nim.volumeHost.GetKubeClient()
	if csiKubeClient == nil {
		return goerrors.New("error getting CSI client")
	}

	var lastErr error
	err := wait.ExponentialBackoff(updateBackoff, func() (bool, error) {
		//1. 如果对应的CSINode不存在，则创建CSINode
		if lastErr = nim.tryInitializeCSINodeWithAnnotation(csiKubeClient); lastErr != nil {
			klog.V(2).Infof("Failed to publish CSINode: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("error updating CSINode annotation: %v; caused by: %v", err, lastErr)
	}

	return nil
}

func (nim *nodeInfoManager) tryInitializeCSINodeWithAnnotation(csiKubeClient clientset.Interface) error {
	nodeInfo, err := csiKubeClient.StorageV1().CSINodes().Get(context.TODO(), string(nim.nodeName), metav1.GetOptions{})
	if nodeInfo == nil || errors.IsNotFound(err) {
		// CreateCSINode will set the annotation
		_, err = nim.CreateCSINode()
		return err
	} else if err != nil {
		return err
	}

	annotationModified := setMigrationAnnotation(nim.migratedPlugins, nodeInfo)

	if annotationModified {
		_, err := csiKubeClient.StorageV1().CSINodes().Update(context.TODO(), nodeInfo, metav1.UpdateOptions{})
		return err
	}
	return nil

}

//提取kubelet corev1 node对象信息，并提取信息向Apiserver创建CSINode对象。
func (nim *nodeInfoManager) CreateCSINode() (*storagev1.CSINode, error) {

	kubeClient := nim.volumeHost.GetKubeClient()
	if kubeClient == nil {
		return nil, fmt.Errorf("error getting kube client")
	}

	csiKubeClient := nim.volumeHost.GetKubeClient()
	if csiKubeClient == nil {
		return nil, fmt.Errorf("error getting CSI client")
	}
	//获取corev1 node信息
	node, err := kubeClient.CoreV1().Nodes().Get(context.TODO(), string(nim.nodeName), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	nodeInfo := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nim.nodeName),
			//引用的节点信息，将corev1.Node信息记录到CSINode中
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: nodeKind.Version,
					Kind:       nodeKind.Kind,
					Name:       node.Name,
					UID:        node.UID,
				},
			},
		},
		Spec: storagev1.CSINodeSpec{
			Drivers: []storagev1.CSINodeDriver{},
		},
	}

	setMigrationAnnotation(nim.migratedPlugins, nodeInfo)
	//创建CSI Node
	return csiKubeClient.StorageV1().CSINodes().Create(context.TODO(), nodeInfo, metav1.CreateOptions{})
}

func setMigrationAnnotation(migratedPlugins map[string](func() bool), nodeInfo *storagev1.CSINode) (modified bool) {
	if migratedPlugins == nil {
		return false
	}

	nodeInfoAnnotations := nodeInfo.GetAnnotations()
	if nodeInfoAnnotations == nil {
		nodeInfoAnnotations = map[string]string{}
	}

	var oldAnnotationSet sets.String
	mpa := nodeInfoAnnotations[v1.MigratedPluginsAnnotationKey]
	tok := strings.Split(mpa, ",")
	if len(mpa) == 0 {
		oldAnnotationSet = sets.NewString()
	} else {
		oldAnnotationSet = sets.NewString(tok...)
	}

	newAnnotationSet := sets.NewString()
	for pluginName, migratedFunc := range migratedPlugins {
		if migratedFunc() {
			newAnnotationSet.Insert(pluginName)
		}
	}

	if oldAnnotationSet.Equal(newAnnotationSet) {
		return false
	}

	nas := strings.Join(newAnnotationSet.List(), ",")
	if len(nas) != 0 {
		nodeInfoAnnotations[v1.MigratedPluginsAnnotationKey] = nas
	} else {
		delete(nodeInfoAnnotations, v1.MigratedPluginsAnnotationKey)
	}

	nodeInfo.Annotations = nodeInfoAnnotations
	return true
}

//将csiDriver的信息更新到CsiNode中
func (nim *nodeInfoManager) installDriverToCSINode(
	nodeInfo *storagev1.CSINode,
	driverName string,
	driverNodeID string,
	maxAttachLimit int64,
	topology map[string]string) error {

	csiKubeClient := nim.volumeHost.GetKubeClient()
	if csiKubeClient == nil {
		return fmt.Errorf("error getting CSI client")
	}

	topologyKeys := make(sets.String)
	for k := range topology {
		topologyKeys.Insert(k)
	}

	specModified := true
	// Clone driver list, omitting the driver that matches the given driverName
	newDriverSpecs := []storagev1.CSINodeDriver{}
	for _, driverInfoSpec := range nodeInfo.Spec.Drivers {
		if driverInfoSpec.Name == driverName {
			if driverInfoSpec.NodeID == driverNodeID &&
				sets.NewString(driverInfoSpec.TopologyKeys...).Equal(topologyKeys) {
				specModified = false
			}
		} else {
			// Omit driverInfoSpec matching given driverName
			newDriverSpecs = append(newDriverSpecs, driverInfoSpec)
		}
	}

	annotationModified := setMigrationAnnotation(nim.migratedPlugins, nodeInfo)
	//如果不需要修改CSINode的spec以及annotation, 直接返回
	if !specModified && !annotationModified {
		return nil
	}

	// Append new driver
	driverSpec := storagev1.CSINodeDriver{
		Name:         driverName,
		NodeID:       driverNodeID,
		TopologyKeys: topologyKeys.List(),
	}

	if maxAttachLimit > 0 {
		if maxAttachLimit > math.MaxInt32 {
			klog.Warningf("Exceeded max supported attach limit value, truncating it to %d", math.MaxInt32)
			maxAttachLimit = math.MaxInt32
		}
		m := int32(maxAttachLimit)
		driverSpec.Allocatable = &storagev1.VolumeNodeResources{Count: &m}
	} else {
		klog.Errorf("Invalid attach limit value %d cannot be added to CSINode object for %q", maxAttachLimit, driverName)
	}

	newDriverSpecs = append(newDriverSpecs, driverSpec)
	nodeInfo.Spec.Drivers = newDriverSpecs

	_, err := csiKubeClient.StorageV1().CSINodes().Update(context.TODO(), nodeInfo, metav1.UpdateOptions{})
	return err
}

//将特定的CSIDriver从CSINode.spec列表中移除
func (nim *nodeInfoManager) uninstallDriverFromCSINode(
	csiDriverName string) error {

	csiKubeClient := nim.volumeHost.GetKubeClient()
	if csiKubeClient == nil {
		return fmt.Errorf("error getting CSI client")
	}

	var updateErrs []error
	err := wait.ExponentialBackoff(updateBackoff, func() (bool, error) {
		//将特定的CSIDriver从CSINode.spec列表中移除
		if err := nim.tryUninstallDriverFromCSINode(csiKubeClient, csiDriverName); err != nil {
			updateErrs = append(updateErrs, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("error updating CSINode: %v; caused by: %v", err, utilerrors.NewAggregate(updateErrs))
	}
	return nil
}

//将特定的CSIDriver从CSINode.spec列表中移除
func (nim *nodeInfoManager) tryUninstallDriverFromCSINode(
	csiKubeClient clientset.Interface,
	csiDriverName string) error {

	nodeInfoClient := csiKubeClient.StorageV1().CSINodes()
	nodeInfo, err := nodeInfoClient.Get(context.TODO(), string(nim.nodeName), metav1.GetOptions{})
	if err != nil && errors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	hasModified := false
	// Uninstall CSINodeDriver with name csiDriverName
	drivers := nodeInfo.Spec.Drivers[:0]
	for _, driver := range nodeInfo.Spec.Drivers {
		if driver.Name != csiDriverName {
			drivers = append(drivers, driver)
		} else {
			// Found a driver with name csiDriverName
			// Set hasModified to true because it will be removed
			hasModified = true
		}
	}

	if !hasModified {
		// No changes, don't update
		return nil
	}
	nodeInfo.Spec.Drivers = drivers

	_, err = nodeInfoClient.Update(context.TODO(), nodeInfo, metav1.UpdateOptions{})

	return err // do not wrap error

}

//移除node.status.allocatable以及node.status.capacity中指定驱动最大卷挂载数量信息
func removeMaxAttachLimit(driverName string) nodeUpdateFunc {
	return func(node *v1.Node) (*v1.Node, bool, error) {
		limitKey := v1.ResourceName(util.GetCSIAttachLimitKey(driverName))

		capacityExists := false
		if node.Status.Capacity != nil {
			_, capacityExists = node.Status.Capacity[limitKey]
		}

		allocatableExists := false
		if node.Status.Allocatable != nil {
			_, allocatableExists = node.Status.Allocatable[limitKey]
		}

		if !capacityExists && !allocatableExists {
			return node, false, nil
		}

		delete(node.Status.Capacity, limitKey)
		if len(node.Status.Capacity) == 0 {
			node.Status.Capacity = nil
		}

		delete(node.Status.Allocatable, limitKey)
		if len(node.Status.Allocatable) == 0 {
			node.Status.Allocatable = nil
		}

		return node, true, nil
	}
}
