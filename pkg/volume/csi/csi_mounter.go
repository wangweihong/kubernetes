/*
Copyright 2017 The Kubernetes Authors.

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

package csi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"k8s.io/klog"

	api "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/utils/mount"
	utilstrings "k8s.io/utils/strings"
)

//TODO (vladimirvivien) move this in a central loc later
var (
	//  将会在/var/lib/kubelet/pods/<podID>/volumes/kubernetes.io~csi/<volumeName>/vol_data.json 生成以下的内容
	// {"attachmentID":"csi-f68a9cdf83f572a3de797db507b8dd615595cf7054db3057b9e681bbbadab56a","driverName":"topsc.topke.io","nodeName":"haaaa-10-30-100-149","specVolID":"pvc-dca8527d-0939-4c6a-a2cc-e66a673f062c","volumeHandle":"pvc-dca8527d-0939-4c6a-a2cc-e66a673f062c","volumeLifecycleMode":"Persistent"}
	volDataKey = struct {
		specVolID,
		volHandle,
		driverName,
		nodeName,
		attachmentID,
		volumeLifecycleMode string
	}{
		"specVolID",
		"volumeHandle",
		"driverName", //真正的驱动名
		"nodeName",
		"attachmentID",
		"volumeLifecycleMode",
	}
)

type csiMountMgr struct {
	csiClientGetter
	k8s                 kubernetes.Interface
	plugin              *csiPlugin
	driverName          csiDriverName
	volumeLifecycleMode storage.VolumeLifecycleMode
	volumeID            string //CSI后端存储卷标识
	specVolumeID        string //pv名或者pod.spec.volume中指定的卷名
	readOnly            bool
	supportsSELinux     bool
	spec                *volume.Spec
	pod                 *api.Pod
	podUID              types.UID
	options             volume.VolumeOptions
	publishContext      map[string]string
	kubeVolHost         volume.KubeletVolumeHost
	volume.MetricsProvider
}

// volume.Volume methods
var _ volume.Volume = &csiMountMgr{}

// /var/lib/kubelet/pods/<podID>/volumes/kubernetes.io~csi/<volumeName>/mount  用来表示挂载在pod内部的卷
func (c *csiMountMgr) GetPath() string {
	dir := filepath.Join(getTargetPath(c.podUID, c.specVolumeID, c.plugin.host), "/mount")
	klog.V(4).Info(log("mounter.GetPath generated [%s]", dir))
	return dir
}

// /var/lib/kubelet/pods/<podID>/volumes/kubernetes.io~csi/<volumeName>  用来表示挂载在pod内部的卷
func getTargetPath(uid types.UID, specVolumeID string, host volume.VolumeHost) string {
	specVolID := utilstrings.EscapeQualifiedName(specVolumeID)
	return host.GetPodVolumeDir(uid, utilstrings.EscapeQualifiedName(CSIPluginName), specVolID)
}

// volume.Mounter methods
var _ volume.Mounter = &csiMountMgr{}

//默认能挂载
func (c *csiMountMgr) CanMount() error {
	return nil
}

func (c *csiMountMgr) SetUp(mounterArgs volume.MounterArgs) error {
	return c.SetUpAt(c.GetPath(), mounterArgs)
}

// dir:/var/lib/kubelet/pods/<podID>/volumes/kubernetes.io~csi/<volumeName>/mount  用来表示挂载在pod内部的卷
func (c *csiMountMgr) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	klog.V(4).Infof(log("Mounter.SetUpAt(%s)", dir))

	corruptedDir := false
	// 检测容器挂载路径是否已经被挂载
	mounted, err := isDirMounted(c.plugin, dir)
	if err != nil {
		//检测容器挂载目录路径是否存在有问题的挂载
		if isCorruptedDir(dir) {
			corruptedDir = true // leave to CSI driver to handle corrupted mount
			klog.Warning(log("mounter.SetUpAt detected corrupted mount for dir [%s]", dir))
		} else {
			return errors.New(log("mounter.SetUpAt failed while checking mount status for dir [%s]: %v", dir, err))
		}
	}
	// 已经被挂载，而且挂载没有问题，什么都不做
	if mounted && !corruptedDir {
		klog.V(4).Info(log("mounter.SetUpAt skipping mount, dir already mounted [%s]", dir))
		return nil
	}

	// 从csi驱动表中找到csi驱动端点，然后生成一个csi驱动客户端，该客户端中包含csi驱动通信端点以及一个与csidriver的NodeServer通信的客户端生成器
	csi, err := c.csiClientGetter.Get()
	if err != nil {
		return volumetypes.NewTransientOperationFailure(log("mounter.SetUpAt failed to get CSI client: %v", err))

	}
	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()

	//pvc中的csi或者pv.spec.csi获取csi的信息，二者只会获取一个
	volSrc, pvSrc, err := getSourceFromSpec(c.spec)
	if err != nil {
		return errors.New(log("mounter.SetupAt failed to get CSI persistent source: %v", err))
	}

	driverName := c.driverName      //csi驱动名
	volumeHandle := c.volumeID      //csi卷ID(远程存储唯一标志卷)
	readOnly := c.readOnly          //是否只读挂载
	accessMode := api.ReadWriteOnce //默认单点读写?

	var (
		fsType             string //
		volAttribs         map[string]string
		nodePublishSecrets map[string]string
		publishContext     map[string]string
		mountOptions       []string
		deviceMountPath    string
		secretRef          *api.SecretReference
	)

	switch {
	//从pvc中提取CSI信息
	case volSrc != nil:
		if !utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume) {
			return fmt.Errorf("CSIInlineVolume feature required")
		}
		if c.volumeLifecycleMode != storage.VolumeLifecycleEphemeral {
			return fmt.Errorf("unexpected volume mode: %s", c.volumeLifecycleMode)
		}
		if volSrc.FSType != nil {
			fsType = *volSrc.FSType
		}

		volAttribs = volSrc.VolumeAttributes

		if volSrc.NodePublishSecretRef != nil {
			secretName := volSrc.NodePublishSecretRef.Name
			ns := c.pod.Namespace
			secretRef = &api.SecretReference{Name: secretName, Namespace: ns}
		}
		//从pv中提取csi相关信息
	case pvSrc != nil:
		if c.volumeLifecycleMode != storage.VolumeLifecyclePersistent {
			return fmt.Errorf("unexpected driver mode: %s", c.volumeLifecycleMode)
		}

		fsType = pvSrc.FSType //要挂载文件系统类型

		volAttribs = pvSrc.VolumeAttributes
		//卷挂载到容器时存放的验证信息的密钥
		if pvSrc.NodePublishSecretRef != nil {
			secretRef = pvSrc.NodePublishSecretRef
		}

		//TODO (vladimirvivien) implement better AccessModes mapping between k8s and CSI
		//只使用第一个访问模式?
		if c.spec.PersistentVolume.Spec.AccessModes != nil {
			accessMode = c.spec.PersistentVolume.Spec.AccessModes[0]
		}
		//挂载卷文件系统时的挂载参数
		mountOptions = c.spec.PersistentVolume.Spec.MountOptions

		// Check for STAGE_UNSTAGE_VOLUME set and populate deviceMountPath if so
		//检测CSI driver是否支持挂载/卸载pv目录特性
		stageUnstageSet, err := csi.NodeSupportsStageUnstage(ctx)
		if err != nil {
			return errors.New(log("mounter.SetUpAt failed to check for STAGE_UNSTAGE_VOLUME capability: %v", err))
		}
		//检测CSI driver是否支持挂载卸载容器目录特性
		if stageUnstageSet {
			//算出外部CSI卷在节点的挂载目录：/var/lib/kubelet/plugins/kubernetes.io/pv/<PV>/globalmount
			deviceMountPath, err = makeDeviceMountPath(c.plugin, c.spec)
			if err != nil {
				return errors.New(log("mounter.SetUpAt failed to make device mount path: %v", err))
			}
		}

		// search for attachment by VolumeAttachment.Spec.Source.PersistentVolumeName
		//有什么用?
		if c.publishContext == nil {
			nodeName := string(c.plugin.host.GetNodeName())
			//通过driver对应的CSIDriver来决定要跳过挂载，如果跳过则什么都不干；否则根据handle/driver/nodename得到对应的VolumeAttachment，返回其中status.AttachmentMetadata信息
			c.publishContext, err = c.plugin.getPublishContext(c.k8s, volumeHandle, string(driverName), nodeName)
			if err != nil {
				// we could have a transient error associated with fetching publish context
				return volumetypes.NewTransientOperationFailure(log("mounter.SetUpAt failed to fetch publishContext: %v", err))
			}
			publishContext = c.publishContext
		}

	default:
		return fmt.Errorf("volume source not found in volume.Spec")
	}

	// create target_dir before call to NodePublish
	//创建容器挂载目录
	if err := os.MkdirAll(dir, 0750); err != nil && !corruptedDir {
		return errors.New(log("mounter.SetUpAt failed to create dir %#v:  %v", dir, err))
	}
	klog.V(4).Info(log("created target path successfully [%s]", dir))

	nodePublishSecrets = map[string]string{}
	if secretRef != nil {
		//从指定secret中提取验证信息
		nodePublishSecrets, err = getCredentialsFromSecret(c.k8s, secretRef)
		if err != nil {
			return volumetypes.NewTransientOperationFailure(fmt.Sprintf("fetching NodePublishSecretRef %s/%s failed: %v",
				secretRef.Namespace, secretRef.Name, err))
		}

	}

	// Inject pod information into volume_attributes
	podAttrs, err := c.podAttributes()
	if err != nil {
		return volumetypes.NewTransientOperationFailure(log("mounter.SetUpAt failed to assemble volume attributes: %v", err))
	}
	if podAttrs != nil {
		if volAttribs == nil {
			volAttribs = podAttrs
		} else {
			for k, v := range podAttrs {
				volAttribs[k] = v
			}
		}
	}

	err = csi.NodePublishVolume(
		ctx,
		volumeHandle,
		readOnly,
		deviceMountPath,
		dir,
		accessMode,
		publishContext,
		volAttribs,
		nodePublishSecrets,
		fsType,
		mountOptions,
	)

	if err != nil {
		// If operation finished with error then we can remove the mount directory.
		if volumetypes.IsOperationFinishedError(err) {
			if removeMountDirErr := removeMountDir(c.plugin, dir); removeMountDirErr != nil {
				klog.Error(log("mounter.SetupAt failed to remove mount dir after a NodePublish() error [%s]: %v", dir, removeMountDirErr))
			}
		}
		return err
	}

	c.supportsSELinux, err = c.kubeVolHost.GetHostUtil().GetSELinuxSupport(dir)
	if err != nil {
		klog.V(2).Info(log("error checking for SELinux support: %s", err))
	}

	// apply volume ownership
	// The following logic is derived from https://github.com/kubernetes/kubernetes/issues/66323
	// if fstype is "", then skip fsgroup (could be indication of non-block filesystem)
	// if fstype is provided and pv.AccessMode == ReadWriteOnly, then apply fsgroup
	err = c.applyFSGroup(fsType, mounterArgs.FsGroup, mounterArgs.FSGroupChangePolicy)
	if err != nil {
		// At this point mount operation is successful:
		//   1. Since volume can not be used by the pod because of invalid permissions, we must return error
		//   2. Since mount is successful, we must record volume as mounted in uncertain state, so it can be
		//      cleaned up.
		return volumetypes.NewUncertainProgressError(fmt.Sprintf("applyFSGroup failed for vol %s: %v", c.volumeID, err))
	}

	klog.V(4).Infof(log("mounter.SetUp successfully requested NodePublish [%s]", dir))
	return nil
}

//? 通过CSIdriver获取卷的信息?
func (c *csiMountMgr) podAttributes() (map[string]string, error) {
	kletHost, ok := c.plugin.host.(volume.KubeletVolumeHost)
	if ok {
		kletHost.WaitForCacheSync()
	}

	if c.plugin.csiDriverLister == nil {
		return nil, fmt.Errorf("CSIDriverLister not found")
	}

	csiDriver, err := c.plugin.csiDriverLister.Get(string(c.driverName))
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof(log("CSIDriver %q not found, not adding pod information", c.driverName))
			return nil, nil
		}
		return nil, err
	}

	// if PodInfoOnMount is not set or false we do not set pod attributes
	if csiDriver.Spec.PodInfoOnMount == nil || *csiDriver.Spec.PodInfoOnMount == false {
		klog.V(4).Infof(log("CSIDriver %q does not require pod information", c.driverName))
		return nil, nil
	}
	//c.Pod信息从哪里来的？
	attrs := map[string]string{
		"csi.storage.k8s.io/pod.name":            c.pod.Name,
		"csi.storage.k8s.io/pod.namespace":       c.pod.Namespace,
		"csi.storage.k8s.io/pod.uid":             string(c.pod.UID),
		"csi.storage.k8s.io/serviceAccount.name": c.pod.Spec.ServiceAccountName,
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume) {
		attrs["csi.storage.k8s.io/ephemeral"] = strconv.FormatBool(c.volumeLifecycleMode == storage.VolumeLifecycleEphemeral)
	}

	klog.V(4).Infof(log("CSIDriver %q requires pod information", c.driverName))
	return attrs, nil
}

func (c *csiMountMgr) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:        c.readOnly,
		Managed:         !c.readOnly,
		SupportsSELinux: c.supportsSELinux,
	}
}

// volume.Unmounter methods
var _ volume.Unmounter = &csiMountMgr{}

func (c *csiMountMgr) TearDown() error {
	return c.TearDownAt(c.GetPath())
}
func (c *csiMountMgr) TearDownAt(dir string) error {
	klog.V(4).Infof(log("Unmounter.TearDown(%s)", dir))

	volID := c.volumeID
	csi, err := c.csiClientGetter.Get()
	if err != nil {
		return errors.New(log("mounter.SetUpAt failed to get CSI client: %v", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()

	if err := csi.NodeUnpublishVolume(ctx, volID, dir); err != nil {
		return errors.New(log("mounter.TearDownAt failed: %v", err))
	}

	// clean mount point dir
	if err := removeMountDir(c.plugin, dir); err != nil {
		return errors.New(log("mounter.TearDownAt failed to clean mount dir [%s]: %v", dir, err))
	}
	klog.V(4).Infof(log("mounter.TearDownAt successfully unmounted dir [%s]", dir))

	return nil
}

// applyFSGroup applies the volume ownership it derives its logic
// from https://github.com/kubernetes/kubernetes/issues/66323
// 1) if fstype is "", then skip fsgroup (could be indication of non-block filesystem)
// 2) if fstype is provided and pv.AccessMode == ReadWriteOnly and !c.spec.ReadOnly then apply fsgroup
func (c *csiMountMgr) applyFSGroup(fsType string, fsGroup *int64, fsGroupChangePolicy *v1.PodFSGroupChangePolicy) error {
	if fsGroup != nil {
		if fsType == "" {
			klog.V(4).Info(log("mounter.SetupAt WARNING: skipping fsGroup, fsType not provided"))
			return nil
		}

		accessModes := c.spec.PersistentVolume.Spec.AccessModes
		if c.spec.PersistentVolume.Spec.AccessModes == nil {
			klog.V(4).Info(log("mounter.SetupAt WARNING: skipping fsGroup, access modes not provided"))
			return nil
		}
		if !hasReadWriteOnce(accessModes) {
			klog.V(4).Info(log("mounter.SetupAt WARNING: skipping fsGroup, only support ReadWriteOnce access mode"))
			return nil
		}

		if c.readOnly {
			klog.V(4).Info(log("mounter.SetupAt WARNING: skipping fsGroup, volume is readOnly"))
			return nil
		}

		err := volume.SetVolumeOwnership(c, fsGroup, fsGroupChangePolicy)
		if err != nil {
			return err
		}

		klog.V(4).Info(log("mounter.SetupAt fsGroup [%d] applied successfully to %s", *fsGroup, c.volumeID))
	}

	return nil
}

// isDirMounted returns the !notMounted result from IsLikelyNotMountPoint check
// 检测路径是否挂载点
func isDirMounted(plug *csiPlugin, dir string) (bool, error) {
	mounter := plug.host.GetMounter(plug.GetPluginName())
	//检测节点上目录是否挂载点
	notMnt, err := mounter.IsLikelyNotMountPoint(dir)
	if err != nil && !os.IsNotExist(err) {
		klog.Error(log("isDirMounted IsLikelyNotMountPoint test failed for dir [%v]", dir))
		return false, err
	}
	return !notMnt, nil
}

//检测目录路径是否存在有问题的挂载
func isCorruptedDir(dir string) bool {
	_, pathErr := mount.PathExists(dir)
	return pathErr != nil && mount.IsCorruptedMnt(pathErr)
}

// removeMountDir cleans the mount dir when dir is not mounted and removed the volume data file in dir
//删除挂载目录，如果仍会有挂载什么都不做（也不去报错)
func removeMountDir(plug *csiPlugin, mountPath string) error {
	klog.V(4).Info(log("removing mount path [%s]", mountPath))

	mnt, err := isDirMounted(plug, mountPath)
	if err != nil {
		return err
	}
	if !mnt {
		klog.V(4).Info(log("dir not mounted, deleting it [%s]", mountPath))
		if err := os.Remove(mountPath); err != nil && !os.IsNotExist(err) {
			return errors.New(log("failed to remove dir [%s]: %v", mountPath, err))
		}
		// remove volume data file as well
		volPath := filepath.Dir(mountPath)
		dataFile := filepath.Join(volPath, volDataFileName)
		klog.V(4).Info(log("also deleting volume info data file [%s]", dataFile))
		if err := os.Remove(dataFile); err != nil && !os.IsNotExist(err) {
			return errors.New(log("failed to delete volume data file [%s]: %v", dataFile, err))
		}
		// remove volume path
		klog.V(4).Info(log("deleting volume path [%s]", volPath))
		if err := os.Remove(volPath); err != nil && !os.IsNotExist(err) {
			return errors.New(log("failed to delete volume path [%s]: %v", volPath, err))
		}
	}
	return nil
}

// makeVolumeHandle returns csi-<sha256(podUID,volSourceSpecName)>
func makeVolumeHandle(podUID, volSourceSpecName string) string {
	result := sha256.Sum256([]byte(fmt.Sprintf("%s%s", podUID, volSourceSpecName)))
	return fmt.Sprintf("csi-%x", result)
}
