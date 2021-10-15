/*
Copyright 2019 The Kubernetes Authors.

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

package persistentvolume

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes/scheme"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/reference"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/features"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

const (
	// AnnBindCompleted Annotation applies to PVCs. It indicates that the lifecycle
	// of the PVC has passed through the initial setup. This information changes how
	// we interpret some observations of the state of the objects. Value of this
	// Annotation does not matter.
	//当pvc第一次绑定完成会设置这个annotation。如果pvc带有这个annotation但spec.volumeName为空, 则认为
	//数据丢失. 设置为LOST状态
	AnnBindCompleted = "pv.kubernetes.io/bind-completed"

	// AnnBoundByController annotation applies to PVs and PVCs. It indicates that
	// the binding (PV->PVC or PVC->PV) was installed by the controller. The
	// absence of this annotation means the binding was done by the user (i.e.
	// pre-bound). Value of this annotation does not matter.
	// External PV binders must bind PV the same way as PV controller, otherwise PV
	// controller may not handle it correctly.
	AnnBoundByController = "pv.kubernetes.io/bound-by-controller"

	// AnnSelectedNode annotation is added to a PVC that has been triggered by scheduler to
	// be dynamically provisioned. Its value is the name of the selected node.
	AnnSelectedNode = "volume.kubernetes.io/selected-node" //用来在pvc指定了要绑定的pv落地的节点
	// pod调度时会根据这个字段来判断节点是否满足pvc落地
	// NotSupportedProvisioner is a special provisioner name which can be set
	// in storage class to indicate dynamic provisioning is not supported by
	// the storage.
	NotSupportedProvisioner = "kubernetes.io/no-provisioner"

	// AnnDynamicallyProvisioned annotation is added to a PV that has been dynamically provisioned by
	// Kubernetes. Its value is name of volume plugin that created the volume.
	// It serves both user (to show where a PV comes from) and Kubernetes (to
	// recognize dynamically provisioned PVs in its decisions).
	AnnDynamicallyProvisioned = "pv.kubernetes.io/provisioned-by"

	// AnnMigratedTo annotation is added to a PVC and PV that is supposed to be
	// dynamically provisioned/deleted by by its corresponding CSI driver
	// through the CSIMigration feature flags. When this annotation is set the
	// Kubernetes components will "stand-down" and the external-provisioner will
	// act on the objects
	AnnMigratedTo = "pv.kubernetes.io/migrated-to"

	// AnnStorageProvisioner annotation is added to a PVC that is supposed to be dynamically
	// provisioned. Its value is name of volume plugin that is supposed to provision
	// a volume for this PVC.
	AnnStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
)

// IsDelayBindingProvisioning checks if claim provisioning with selected-node annotation
func IsDelayBindingProvisioning(claim *v1.PersistentVolumeClaim) bool {
	// When feature VolumeScheduling enabled,
	// Scheduler signal to the PV controller to start dynamic
	// provisioning by setting the "AnnSelectedNode" annotation
	// in the PVC
	_, ok := claim.Annotations[AnnSelectedNode]
	return ok
}

// IsDelayBindingMode checks if claim is in delay binding mode
// 延迟绑定模式，创建pvc并不会立即创建pv, 而是要等到pvc被pod使用
// 通过pvc引用的存储类的VolumeBindingMode来判断
func IsDelayBindingMode(claim *v1.PersistentVolumeClaim, classLister storagelisters.StorageClassLister) (bool, error) {
	className := v1helper.GetPersistentVolumeClaimClass(claim)
	if className == "" {
		return false, nil
	}

	class, err := classLister.Get(className)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if class.VolumeBindingMode == nil {
		return false, fmt.Errorf("VolumeBindingMode not set for StorageClass %q", className)
	}

	return *class.VolumeBindingMode == storage.VolumeBindingWaitForFirstConsumer, nil
}

// GetBindVolumeToClaim returns a new volume which is bound to given claim. In
// addition, it returns a bool which indicates whether we made modification on
// original volume.
//bool表示volume是否添加了PVC的引用
// 如果claim和volume没有绑定, 用claim来更新pv.spec.claimRef
func GetBindVolumeToClaim(volume *v1.PersistentVolume, claim *v1.PersistentVolumeClaim) (*v1.PersistentVolume, bool, error) {
	dirty := false

	// Check if the volume was already bound (either by user or by controller)
	shouldSetBoundByController := false
	//volume是否绑定到PVC claim上
	if !IsVolumeBoundToClaim(volume, claim) {
		shouldSetBoundByController = true
	}

	// The volume from method args can be pointing to watcher cache. We must not
	// modify these, therefore create a copy.
	volumeClone := volume.DeepCopy()

	// Bind the volume to the claim if it is not bound yet
	//如果没有绑定或者之前绑定的不是pvc claim, 将claim的信息更新到PV.spec.ClaimRef中
	if volume.Spec.ClaimRef == nil ||
		volume.Spec.ClaimRef.Name != claim.Name ||
		volume.Spec.ClaimRef.Namespace != claim.Namespace ||
		volume.Spec.ClaimRef.UID != claim.UID {

		claimRef, err := reference.GetReference(scheme.Scheme, claim)
		if err != nil {
			return nil, false, fmt.Errorf("Unexpected error getting claim reference: %v", err)
		}
		volumeClone.Spec.ClaimRef = claimRef
		dirty = true
	}

	// Set AnnBoundByController if it is not set yet
	if shouldSetBoundByController && !metav1.HasAnnotation(volumeClone.ObjectMeta, AnnBoundByController) {
		metav1.SetMetaDataAnnotation(&volumeClone.ObjectMeta, AnnBoundByController, "yes")
		dirty = true
	}

	return volumeClone, dirty, nil
}

// IsVolumeBoundToClaim returns true, if given volume is pre-bound or bound
// to specific claim. Both claim.Name and claim.Namespace must be equal.
// If claim.UID is present in volume.Spec.ClaimRef, it must be equal too.
//PV volume是否和PVC claim绑定
func IsVolumeBoundToClaim(volume *v1.PersistentVolume, claim *v1.PersistentVolumeClaim) bool {
	if volume.Spec.ClaimRef == nil {
		return false
	}
	if claim.Name != volume.Spec.ClaimRef.Name || claim.Namespace != volume.Spec.ClaimRef.Namespace {
		return false
	}
	if volume.Spec.ClaimRef.UID != "" && claim.UID != volume.Spec.ClaimRef.UID {
		return false
	}
	return true
}

// FindMatchingVolume goes through the list of volumes to find the best matching volume
// for the claim.
//
// This function is used by both the PV controller and scheduler.
//
// delayBinding is true only in the PV controller path.  When set, prebound PVs are still returned
// as a match for the claim, but unbound PVs are skipped.
//
// node is set only in the scheduler path. When set, the PV node affinity is checked against
// the node's labels.
//
// excludedVolumes is only used in the scheduler path, and is needed for evaluating multiple
// unbound PVCs for a single Pod at one time.  As each PVC finds a matching PV, the chosen
// PV needs to be excluded from future matching.
//从volumes中找到最适合和claim绑定的卷
// 这个函数有两个目标用户: pv controller和scheduler.
//	* pv controller不关心pvc的落地情况，只为完成pvc/pv的绑定，此时并没有pod的参与，所以node为空
//  * scheduler要为延迟绑定模式的pvc查找既满足pvc绑定，又满足调度节点的pv(有些pv指定了落地的节点条件），需要比对pv拓扑和node
func FindMatchingVolume(
	claim *v1.PersistentVolumeClaim,
	volumes []*v1.PersistentVolume,
	node *v1.Node,
	excludedVolumes map[string]*v1.PersistentVolume,
	delayBinding bool) (*v1.PersistentVolume, error) {

	var smallestVolume *v1.PersistentVolume
	var smallestVolumeQty resource.Quantity
	//pvc请求的存储容量
	requestedQty := claim.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	//pvc请求的存储类
	requestedClass := v1helper.GetPersistentVolumeClaimClass(claim)

	var selector labels.Selector
	//pvc指定的标签选择器
	if claim.Spec.Selector != nil {
		internalSelector, err := metav1.LabelSelectorAsSelector(claim.Spec.Selector)
		if err != nil {
			// should be unreachable code due to validation
			return nil, fmt.Errorf("error creating internal label selector for claim: %v: %v", claimToClaimKey(claim), err)
		}
		selector = internalSelector
	}

	// Go through all available volumes with two goals:
	// - find a volume that is either pre-bound by user or dynamically
	//   provisioned for this claim. Because of this we need to loop through
	//   all volumes.
	// - find the smallest matching one if there is no volume pre-bound to
	//   the claim.
	for _, volume := range volumes {
		if _, ok := excludedVolumes[volume.Name]; ok {
			// Skip volumes in the excluded list
			continue
		}
		//pv已经和其他pvc绑定
		if volume.Spec.ClaimRef != nil && !IsVolumeBoundToClaim(volume, claim) {
			continue
		}
		//PV容量是否满足
		volumeQty := volume.Spec.Capacity[v1.ResourceStorage]
		if volumeQty.Cmp(requestedQty) < 0 {
			continue
		}
		// filter out mismatching volumeModes
		//卷模式是否匹配
		if CheckVolumeModeMismatches(&claim.Spec, &volume.Spec) {
			continue
		}

		// check if PV's DeletionTimeStamp is set, if so, skip this volume.
		//PV正在删除
		if utilfeature.DefaultFeatureGate.Enabled(features.StorageObjectInUseProtection) {
			if volume.ObjectMeta.DeletionTimestamp != nil {
				continue
			}
		}
		//PV是否满足节点亲和力
		nodeAffinityValid := true
		if node != nil {
			// Scheduler path, check that the PV NodeAffinity
			// is satisfied by the node
			// volumeutil.CheckNodeAffinity is the most expensive call in this loop.
			// We should check cheaper conditions first or consider optimizing this function.
			err := volumeutil.CheckNodeAffinity(volume, node.Labels)
			if err != nil {
				nodeAffinityValid = false
			}
		}
		// pvc/pv是否相互绑定
		if IsVolumeBoundToClaim(volume, claim) {
			// If PV node affinity is invalid, return no match.
			// This means the prebound PV (and therefore PVC)
			// is not suitable for this node.
			if !nodeAffinityValid {
				return nil, nil
			}

			return volume, nil
		}
		//没有指定节点而且设置了延迟调度模式
		if node == nil && delayBinding {
			// PV controller does not bind this claim.
			// Scheduler will handle binding unbound volumes
			// Scheduler path will have node != nil
			continue
		}

		// filter out:
		// - volumes in non-available phase
		// - volumes whose labels don't match the claim's selector, if specified
		// - volumes in Class that is not requested
		// - volumes whose NodeAffinity does not match the node
		if volume.Status.Phase != v1.VolumeAvailable {
			// We ignore volumes in non-available phase, because volumes that
			// satisfies matching criteria will be updated to available, binding
			// them now has high chance of encountering unnecessary failures
			// due to API conflicts.
			continue
		} else if selector != nil && !selector.Matches(labels.Set(volume.Labels)) {
			continue
		}
		//提取PV的storageclass
		if v1helper.GetPersistentVolumeClass(volume) != requestedClass {
			continue
		}
		if !nodeAffinityValid {
			continue
		}

		if node != nil {
			// Scheduler path
			// Check that the access modes match
			//检测pv支持的访问模式是否满足pvc的访问模式
			if !CheckAccessModes(claim, volume) {
				continue
			}
		}
		// 找到满足容量的最小卷
		if smallestVolume == nil || smallestVolumeQty.Cmp(volumeQty) > 0 {
			smallestVolume = volume
			smallestVolumeQty = volumeQty
		}
	}

	if smallestVolume != nil {
		// Found a matching volume
		return smallestVolume, nil
	}

	return nil, nil
}

// CheckVolumeModeMismatches is a convenience method that checks volumeMode for PersistentVolume
// and PersistentVolumeClaims
//检测pv的卷模式是否满足pvc的卷模式
func CheckVolumeModeMismatches(pvcSpec *v1.PersistentVolumeClaimSpec, pvSpec *v1.PersistentVolumeSpec) bool {
	// In HA upgrades, we cannot guarantee that the apiserver is on a version >= controller-manager.
	// So we default a nil volumeMode to filesystem
	requestedVolumeMode := v1.PersistentVolumeFilesystem
	if pvcSpec.VolumeMode != nil {
		requestedVolumeMode = *pvcSpec.VolumeMode
	}
	pvVolumeMode := v1.PersistentVolumeFilesystem
	if pvSpec.VolumeMode != nil {
		pvVolumeMode = *pvSpec.VolumeMode
	}
	return requestedVolumeMode != pvVolumeMode
}

// CheckAccessModes returns true if PV satisfies all the PVC's requested AccessModes
//检测pv支持的访问模式是否满足pvc的访问模式
func CheckAccessModes(claim *v1.PersistentVolumeClaim, volume *v1.PersistentVolume) bool {
	pvModesMap := map[v1.PersistentVolumeAccessMode]bool{}
	for _, mode := range volume.Spec.AccessModes {
		pvModesMap[mode] = true
	}

	for _, mode := range claim.Spec.AccessModes {
		_, ok := pvModesMap[mode]
		if !ok {
			return false
		}
	}
	return true
}

func claimToClaimKey(claim *v1.PersistentVolumeClaim) string {
	return fmt.Sprintf("%s/%s", claim.Namespace, claim.Name)
}

// GetVolumeNodeAffinity returns a VolumeNodeAffinity for given key and value.
func GetVolumeNodeAffinity(key string, value string) *v1.VolumeNodeAffinity {
	return &v1.VolumeNodeAffinity{
		Required: &v1.NodeSelector{
			NodeSelectorTerms: []v1.NodeSelectorTerm{
				{
					MatchExpressions: []v1.NodeSelectorRequirement{
						{
							Key:      key,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{value},
						},
					},
				},
			},
		},
	}
}
