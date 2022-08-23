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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog"

	api "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	csitranslationplugins "k8s.io/csi-translation-lib/plugins"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi/nodeinfomanager"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

const (
	// CSIPluginName is the name of the in-tree CSI Plugin
	CSIPluginName = "kubernetes.io/csi"

	csiTimeout      = 2 * time.Minute
	volNameSep      = "^"
	volDataFileName = "vol_data.json" // /var/lib/kubelet/pods/<podID>/volumes/kubernetes.io~csi/<volumeName>/vol_data.json
	fsTypeBlockName = "block"

	// CsiResyncPeriod is default resync period duration
	// TODO: increase to something useful
	CsiResyncPeriod = time.Minute
)

type csiPlugin struct {
	host                   volume.VolumeHost                     //提供访问kubelet的功能
	blockEnabled           bool                                  //kubelet是否启动了CSI driver的block特性
	csiDriverLister        storagelisters.CSIDriverLister        //用来获取Apiserver中CSIDriver资源的列表 ? CSIDriver是谁创建的?
	volumeAttachmentLister storagelisters.VolumeAttachmentLister //用来获取Apiserver中VolumeAttachment资源的列表信息。kubelet不需要。controllerManager才需要设置。
}

// ProbeVolumePlugins returns implemented plugins
// 返回csi插件对象
func ProbeVolumePlugins() []volume.VolumePlugin {
	p := &csiPlugin{
		host:         nil,
		blockEnabled: utilfeature.DefaultFeatureGate.Enabled(features.CSIBlockVolume),
	}
	return []volume.VolumePlugin{p}
}

// volume.VolumePlugin methods
var _ volume.VolumePlugin = &csiPlugin{}

// RegistrationHandler is the handler which is fed to the pluginwatcher API.
type RegistrationHandler struct {
}

// TODO (verult) consider using a struct instead of global variables
// csiDrivers map keep track of all registered CSI drivers on the node and their
// corresponding sockets
var csiDrivers = &DriversStore{} // 读写安全表，用于记录已安装的CSI插件。

var nim nodeinfomanager.Interface // 在csiPlugin初始化时创建该对象。负责管理CSINode和Node

// PluginHandler is the plugin registration handler interface passed to the
// pluginwatcher module in kubelet
var PluginHandler = &RegistrationHandler{} //csi插件注册器。用于处理新的csi driver注册。kubelet plugin manager注册新的CSIPlugin时
//会调用RegistrationHandler执行的注册流程
// ValidatePlugin is called by kubelet's plugin watcher upon detection
// of a new registration socket opened by CSI Driver registrar side car.
func (h *RegistrationHandler) ValidatePlugin(pluginName string, endpoint string, versions []string) error {
	klog.Infof(log("Trying to validate a new CSI Driver with name: %s endpoint: %s versions: %s",
		pluginName, endpoint, strings.Join(versions, ",")))
	//1. 检测插件是否已经注册。
	//2. 如果插件已经注册，那么期待注册的版本是否高于已注册的版本
	_, err := h.validateVersions("ValidatePlugin", pluginName, endpoint, versions)
	if err != nil {
		return fmt.Errorf("validation failed for CSI Driver %s at endpoint %s: %v", pluginName, endpoint, err)
	}

	return err
}

// RegisterPlugin is called when a plugin can be registered
// 注册CSI插件。由PluginManager检测到CSI插件注册时调用
// pluginName: 插件名
// endpoint: 插件通信端点（不一定和插件注册端点相同）
// versions: 插件支持版本列表
func (h *RegistrationHandler) RegisterPlugin(pluginName string, endpoint string, versions []string) error {
	klog.Infof(log("Register new plugin with name: %s at endpoint: %s", pluginName, endpoint))
	// 检测插件最高版本
	// 如果已经有高版本的同名插件注册，则注册失败
	highestSupportedVersion, err := h.validateVersions("RegisterPlugin", pluginName, endpoint, versions)
	if err != nil {
		return err
	}

	// Storing endpoint of newly registered CSI driver into the map, where CSI driver name will be the key
	// all other CSI components will be able to get the actual socket of CSI drivers by its name.
	//保存注册的CSI插件到CSI驱动表中。
	//直接替换掉低版本的插件。
	csiDrivers.Set(pluginName, Driver{
		endpoint:                endpoint,
		highestSupportedVersion: highestSupportedVersion,
	})

	// Get node info from the driver.
	//建立与CSI驱动的客户端(node端）
	csi, err := newCsiDriverClient(csiDriverName(pluginName))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()

	//获取CSI驱动的节点信息
	//accessibleTopology：键值对。会更新到 kubelet对应的node.label上可用于调度。
	driverNodeID, maxVolumePerNode, accessibleTopology, err := csi.NodeGetInfo(ctx)
	if err != nil {
		if unregErr := unregisterDriver(pluginName); unregErr != nil {
			klog.Error(log("registrationHandler.RegisterPlugin failed to unregister plugin due to previous error: %v", unregErr))
		}
		return err
	}
	//1. 将CSIDriver的driverName以及driverNode记录到node的Annotation中，将 accessibleTopology更新到node的label中
	//2. 将CSIDriver的driverName以及driverNode，accessibleTopology更新到CSINode.spec(不存在CSINode则创建)中
	err = nim.InstallCSIDriver(pluginName, driverNodeID, maxVolumePerNode, accessibleTopology)
	if err != nil {
		if unregErr := unregisterDriver(pluginName); unregErr != nil {
			klog.Error(log("registrationHandler.RegisterPlugin failed to unregister plugin due to previous error: %v", unregErr))
		}
		return err
	}

	return nil
}

//检测同名的驱动是否已经存在更高版本的存在
func (h *RegistrationHandler) validateVersions(callerName, pluginName string, endpoint string, versions []string) (*utilversion.Version, error) {
	if len(versions) == 0 {
		return nil, errors.New(log("%s for CSI driver %q failed. Plugin returned an empty list for supported versions", callerName, pluginName))
	}

	// Validate version
	newDriverHighestVersion, err := highestSupportedVersion(versions)
	if err != nil {
		return nil, errors.New(log("%s for CSI driver %q failed. None of the versions specified %q are supported. err=%v", callerName, pluginName, versions, err))
	}
	//插件是否已经注册
	existingDriver, driverExists := csiDrivers.Get(pluginName)
	if driverExists {
		// 注册插件的版本低于已注册插件的版本
		if !existingDriver.highestSupportedVersion.LessThan(newDriverHighestVersion) {
			return nil, errors.New(log("%s for CSI driver %q failed. Another driver with the same name is already registered with a higher supported version: %q", callerName, pluginName, existingDriver.highestSupportedVersion))
		}
	}

	return newDriverHighestVersion, nil
}

// DeRegisterPlugin is called when a plugin removed its socket, signaling
// it is no longer available
// 移除插件的注册
func (h *RegistrationHandler) DeRegisterPlugin(pluginName string) {
	klog.Info(log("registrationHandler.DeRegisterPlugin request for plugin %s", pluginName))
	if err := unregisterDriver(pluginName); err != nil {
		klog.Error(log("registrationHandler.DeRegisterPlugin failed: %v", err))
	}
}

//初始化CSI pluign
func (p *csiPlugin) Init(host volume.VolumeHost) error {
	p.host = host

	//获得从apiserver通信的客户端
	csiClient := host.GetKubeClient()
	if csiClient == nil {
		klog.Warning(log("kubeclient not set, assuming standalone kubelet"))
	} else {
		// set CSIDriverLister and volumeAttachmentLister
		//如果初始化是由ControllerManager的AttachDetachController发起，而非kubelet
		adcHost, ok := host.(volume.AttachDetachVolumeHost)
		if ok {
			p.csiDriverLister = adcHost.CSIDriverLister()
			if p.csiDriverLister == nil {
				klog.Error(log("CSIDriverLister not found on AttachDetachVolumeHost"))
			}
			p.volumeAttachmentLister = adcHost.VolumeAttachmentLister()
			if p.volumeAttachmentLister == nil {
				klog.Error(log("VolumeAttachmentLister not found on AttachDetachVolumeHost"))
			}
		}
		//初始化由kubelet发起
		kletHost, ok := host.(volume.KubeletVolumeHost)
		if ok {
			p.csiDriverLister = kletHost.CSIDriverLister() //CSIDriver资源列表接口
			if p.csiDriverLister == nil {
				klog.Error(log("CSIDriverLister not found on KubeletVolumeHost"))
			}
			// We don't run the volumeAttachmentLister in the kubelet context
			p.volumeAttachmentLister = nil // kubelet不关心VolumeAttachment资源
		}
	}

	var migratedPlugins = map[string](func() bool){
		csitranslationplugins.GCEPDInTreePluginName: func() bool {
			return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigration) && utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationGCE)
		},
		csitranslationplugins.AWSEBSInTreePluginName: func() bool {
			return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigration) && utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationAWS)
		},
		csitranslationplugins.CinderInTreePluginName: func() bool {
			return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigration) && utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationOpenStack)
		},
		csitranslationplugins.AzureDiskInTreePluginName: func() bool {
			return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigration) && utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationAzureDisk)
		},
		csitranslationplugins.AzureFileInTreePluginName: func() bool {
			return utilfeature.DefaultFeatureGate.Enabled(features.CSIMigration) && utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationAzureFile)
		},
	}

	// Initializing the label management channels
	// nim是一个全局变量. 负责管理CSINode以及kubelet对应的Node
	nim = nodeinfomanager.NewNodeInfoManager(host.GetNodeName(), host, migratedPlugins)

	//CSINodeInfo在1.16版本后默认启动(1.19后已经废弃），CSIMigration在1.17版本后默认启动
	if utilfeature.DefaultFeatureGate.Enabled(features.CSINodeInfo) &&
		utilfeature.DefaultFeatureGate.Enabled(features.CSIMigration) {
		// This function prevents Kubelet from posting Ready status until CSINode
		// is both installed and initialized
		// 异步启动协程创建或更新与当前kubelet相关的CSINode
		if err := initializeCSINode(host); err != nil {
			return errors.New(log("failed to initialize CSINode: %v", err))
		}
	}

	return nil
}

// 设置kubelet运行时存储错误”CSI Node未初始化"(这使得kubelet对应的node处于Unready Condition),启动一个协程创建或者更新CSINode对象
// 当对象创建成功后, 移除运行时存储错误信息。
func initializeCSINode(host volume.VolumeHost) error {
	kvh, ok := host.(volume.KubeletVolumeHost)
	if !ok {
		klog.V(4).Info("Cast from VolumeHost to KubeletVolumeHost failed. Skipping CSINode initialization, not running on kubelet")
		return nil
	}
	kubeClient := host.GetKubeClient()
	if kubeClient == nil {
		// Kubelet running in standalone mode. Skip CSINode initialization
		klog.Warning("Skipping CSINode initialization, kubelet running in standalone mode")
		return nil
	}
	//设置kubelet运行时存储“CSINode未初始化"错误
	//设置这个存储会导致kubelet对应的Node处于NotReady Condition.
	kvh.SetKubeletError(errors.New("CSINode is not yet initialized"))
	// 启动协程创建和kubelet对应的Node同名的CSINode，当创建成功后， 移除kubelet运行时存储“CSINode未初始化"错误
	// 如果CSINode创建失败, kubelet会因为CSINode未初始化错误导致一直处于NotReady Condition.
	go func() {
		defer utilruntime.HandleCrash()

		// First wait indefinitely to talk to Kube APIServer
		// kubelet节点名
		nodeName := host.GetNodeName()
		// 一直向apiserver获取csi node的信息直到apiserver正常回应（此时csinode可能不存在，但无所谓）/
		// 目的是确保1. kubelet能够向apiserve获取csinode信息（有权限)，2. apiserver能够正常回应
		err := waitForAPIServerForever(kubeClient, nodeName)
		if err != nil {
			klog.Fatalf("Failed to initialize CSINode while waiting for API server to report ok: %v", err)
		}

		// Backoff parameters tuned to retry over 140 seconds. Will fail and restart the Kubelet
		// after max retry steps.
		initBackoff := wait.Backoff{
			Steps:    6,
			Duration: 15 * time.Millisecond, //初始回避值。
			Factor:   6.0,                   //因数
			Jitter:   0.1,                   //误差值
		}
		//执行创建/更新CSINode动作，当每次创建/更新失败后，睡眠一段时间（每次时间都会延长，第一次15毫秒左右，第二次90毫，第三次540，第四次3.24秒 ，第五次18秒）最多睡眠step-1次
		err = wait.ExponentialBackoff(initBackoff, func() (bool, error) {
			klog.V(4).Infof("Initializing migrated drivers on CSINode")
			// 如果csiNode不存在，则创建csiNode.
			// 如果csiNode已存在，则更新其annotation
			err := nim.InitializeCSINodeWithAnnotation()
			if err != nil {
				kvh.SetKubeletError(fmt.Errorf("Failed to initialize CSINode: %v", err))
				klog.Errorf("Failed to initialize CSINode: %v", err)
				return false, nil
			}

			// Successfully initialized drivers, allow Kubelet to post Ready
			// 移除runtime state存储错误，如果没有其他运行错误，kubelet对应的Node可以回到Ready Conidtion
			kvh.SetKubeletError(nil)
			return true, nil
		})
		if err != nil {
			// 2 releases after CSIMigration and all CSIMigrationX (where X is a volume plugin)
			// are permanently enabled the apiserver/controllers can assume that the kubelet is
			// using CSI for all Migrated volume plugins. Then all the CSINode initialization
			// code can be dropped from Kubelet.
			// Kill the Kubelet process and allow it to restart to retry initialization
			klog.Fatalf("Failed to initialize CSINode after retrying: %v", err)
		}
	}()
	return nil
}

//默认写死 kubernetes.io/csi
func (p *csiPlugin) GetPluginName() string {
	return CSIPluginName
}

// GetvolumeName returns a concatenated string of CSIVolumeSource.Driver<volNameSe>CSIVolumeSource.VolumeHandle
// That string value is used in Detach() to extract driver name and volumeName.
func (p *csiPlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	csi, err := getPVSourceFromSpec(spec)
	if err != nil {
		return "", errors.New(log("plugin.GetVolumeName failed to extract volume source from spec: %v", err))
	}

	// return driverName<separator>volumeHandle
	return fmt.Sprintf("%s%s%s", csi.Driver, volNameSep, csi.VolumeHandle), nil
}

//pv是否非空而且pv.Spe.CSI不为空
func (p *csiPlugin) CanSupport(spec *volume.Spec) bool {
	// TODO (vladimirvivien) CanSupport should also take into account
	// the availability/registration of specified Driver in the volume source
	if spec == nil {
		return false
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume) {
		return (spec.PersistentVolume != nil && spec.PersistentVolume.Spec.CSI != nil) ||
			(spec.Volume != nil && spec.Volume.CSI != nil)
	}

	return spec.PersistentVolume != nil && spec.PersistentVolume.Spec.CSI != nil
}

func (p *csiPlugin) RequiresRemount() bool {
	return false
}

//每个Pod挂载卷都会创建一个Mounter
func (p *csiPlugin) NewMounter(
	spec *volume.Spec,
	pod *api.Pod,
	_ volume.VolumeOptions) (volume.Mounter, error) {
	//csi的信息看是存在pod.spec.Volumes还是在pv.spec.CSI中
	volSrc, pvSrc, err := getSourceFromSpec(spec)
	if err != nil {
		return nil, err
	}

	var (
		driverName   string
		volumeHandle string
		readOnly     bool
	)

	switch {
	case volSrc != nil && utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume):
		volumeHandle = makeVolumeHandle(string(pod.UID), spec.Name())
		driverName = volSrc.Driver
		if volSrc.ReadOnly != nil {
			readOnly = *volSrc.ReadOnly
		}
	case pvSrc != nil:
		driverName = pvSrc.Driver         //CSIDriver驱动名
		volumeHandle = pvSrc.VolumeHandle //对应的后端存储卷的唯一标识符
		readOnly = spec.ReadOnly          //只读挂载
	default:
		return nil, errors.New(log("volume source not found in volume.Spec"))
	}

	volumeLifecycleMode, err := p.getVolumeLifecycleMode(spec)
	if err != nil {
		return nil, err
	}

	// Check CSIDriver.Spec.Mode to ensure that the CSI driver
	// supports the current volumeLifecycleMode.
	if err := p.supportsVolumeLifecycleMode(driverName, volumeLifecycleMode); err != nil {
		return nil, err
	}

	k8s := p.host.GetKubeClient()
	if k8s == nil {
		return nil, errors.New(log("failed to get a kubernetes client"))
	}

	kvh, ok := p.host.(volume.KubeletVolumeHost)
	if !ok {
		return nil, errors.New(log("cast from VolumeHost to KubeletVolumeHost failed"))
	}

	mounter := &csiMountMgr{
		plugin:              p,
		k8s:                 k8s,
		spec:                spec,
		pod:                 pod,
		podUID:              pod.UID,
		driverName:          csiDriverName(driverName),
		volumeLifecycleMode: volumeLifecycleMode,
		volumeID:            volumeHandle,
		specVolumeID:        spec.Name(),
		readOnly:            readOnly,
		kubeVolHost:         kvh,
	}
	mounter.csiClientGetter.driverName = csiDriverName(driverName)

	// Save volume info in pod dir
	dir := mounter.GetPath()     // /var/lib/kubelet/pods/<podID>/volumes/kubernetes.io~csi/<volumeName>/mount
	dataDir := filepath.Dir(dir) // dropoff /mount at end
	//创建挂载路径目录
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, errors.New(log("failed to create dir %#v:  %v", dataDir, err))
	}
	klog.V(4).Info(log("created path successfully [%s]", dataDir))

	mounter.MetricsProvider = NewMetricsCsi(volumeHandle, dir, csiDriverName(driverName))

	// persist volume info data for teardown
	node := string(p.host.GetNodeName())
	volData := map[string]string{
		volDataKey.specVolID:           spec.Name(),  // 要么是pod.spec.volume名，要么是pv名
		volDataKey.volHandle:           volumeHandle, // 后端存储唯一标志
		volDataKey.driverName:          driverName,   // 驱动名
		volDataKey.nodeName:            node,         // 主机名
		volDataKey.volumeLifecycleMode: string(volumeLifecycleMode),
	}
	// csi-<sha256(volName,csiDriverName,NodeName)>
	attachID := getAttachmentName(volumeHandle, driverName, node)
	volData[volDataKey.attachmentID] = attachID
	//生成/var/lib/kubelet/pods/<podID>/volumes/kubernetes.io~csi/<volumeName>/vol_data.json
	err = saveVolumeData(dataDir, volDataFileName, volData)
	defer func() {
		// Only if there was an error and volume operation was considered
		// finished, we should remove the directory.
		if err != nil && volumetypes.IsOperationFinishedError(err) {
			// attempt to cleanup volume mount dir.
			if err = removeMountDir(p, dir); err != nil {
				klog.Error(log("attacher.MountDevice failed to remove mount dir after error [%s]: %v", dir, err))
			}
		}
	}()

	if err != nil {
		errorMsg := log("csi.NewMounter failed to save volume info data: %v", err)
		klog.Error(errorMsg)

		return nil, errors.New(errorMsg)
	}

	klog.V(4).Info(log("mounter created successfully"))

	return mounter, nil
}

func (p *csiPlugin) NewUnmounter(specName string, podUID types.UID) (volume.Unmounter, error) {
	klog.V(4).Infof(log("setting up unmounter for [name=%v, podUID=%v]", specName, podUID))

	kvh, ok := p.host.(volume.KubeletVolumeHost)
	if !ok {
		return nil, errors.New(log("cast from VolumeHost to KubeletVolumeHost failed"))
	}

	unmounter := &csiMountMgr{
		plugin:       p,
		podUID:       podUID,
		specVolumeID: specName,
		kubeVolHost:  kvh,
	}

	// load volume info from file
	dir := unmounter.GetPath()
	dataDir := filepath.Dir(dir) // dropoff /mount at end
	data, err := loadVolumeData(dataDir, volDataFileName)
	if err != nil {
		return nil, errors.New(log("unmounter failed to load volume data file [%s]: %v", dir, err))
	}
	unmounter.driverName = csiDriverName(data[volDataKey.driverName])
	unmounter.volumeID = data[volDataKey.volHandle]
	unmounter.csiClientGetter.driverName = unmounter.driverName

	return unmounter, nil
}

func (p *csiPlugin) ConstructVolumeSpec(volumeName, mountPath string) (*volume.Spec, error) {
	klog.V(4).Info(log("plugin.ConstructVolumeSpec [pv.Name=%v, path=%v]", volumeName, mountPath))

	volData, err := loadVolumeData(mountPath, volDataFileName)
	if err != nil {
		return nil, errors.New(log("plugin.ConstructVolumeSpec failed loading volume data using [%s]: %v", mountPath, err))
	}

	klog.V(4).Info(log("plugin.ConstructVolumeSpec extracted [%#v]", volData))

	var spec *volume.Spec
	inlineEnabled := utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume)

	// If inlineEnabled is true and mode is VolumeLifecycleEphemeral,
	// use constructVolSourceSpec to construct volume source spec.
	// If inlineEnabled is false or mode is VolumeLifecyclePersistent,
	// use constructPVSourceSpec to construct volume construct pv source spec.
	if inlineEnabled && storage.VolumeLifecycleMode(volData[volDataKey.volumeLifecycleMode]) == storage.VolumeLifecycleEphemeral {
		spec = p.constructVolSourceSpec(volData[volDataKey.specVolID], volData[volDataKey.driverName])
		return spec, nil
	}
	spec = p.constructPVSourceSpec(volData[volDataKey.specVolID], volData[volDataKey.driverName], volData[volDataKey.volHandle])

	return spec, nil
}

// constructVolSourceSpec constructs volume.Spec with CSIVolumeSource
func (p *csiPlugin) constructVolSourceSpec(volSpecName, driverName string) *volume.Spec {
	vol := &api.Volume{
		Name: volSpecName,
		VolumeSource: api.VolumeSource{
			CSI: &api.CSIVolumeSource{
				Driver: driverName,
			},
		},
	}
	return volume.NewSpecFromVolume(vol)
}

//constructPVSourceSpec constructs volume.Spec with CSIPersistentVolumeSource
func (p *csiPlugin) constructPVSourceSpec(volSpecName, driverName, volumeHandle string) *volume.Spec {
	fsMode := api.PersistentVolumeFilesystem
	pv := &api.PersistentVolume{
		ObjectMeta: meta.ObjectMeta{
			Name: volSpecName,
		},
		Spec: api.PersistentVolumeSpec{
			PersistentVolumeSource: api.PersistentVolumeSource{
				CSI: &api.CSIPersistentVolumeSource{
					Driver:       driverName,
					VolumeHandle: volumeHandle,
				},
			},
			VolumeMode: &fsMode,
		},
	}
	return volume.NewSpecFromPersistentVolume(pv, false)
}

func (p *csiPlugin) SupportsMountOption() bool {
	// TODO (vladimirvivien) use CSI VolumeCapability.MountVolume.mount_flags
	// to probe for the result for this method
	// (bswartz) Until the CSI spec supports probing, our only option is to
	// make plugins register their support for mount options or lack thereof
	// directly with kubernetes.
	return true
}

func (p *csiPlugin) SupportsBulkVolumeVerification() bool {
	return false
}

// volume.AttachableVolumePlugin methods
var _ volume.AttachableVolumePlugin = &csiPlugin{}

var _ volume.DeviceMountableVolumePlugin = &csiPlugin{}

func (p *csiPlugin) NewAttacher() (volume.Attacher, error) {
	return p.newAttacherDetacher()
}

func (p *csiPlugin) NewDeviceMounter() (volume.DeviceMounter, error) {
	return p.NewAttacher()
}

func (p *csiPlugin) NewDetacher() (volume.Detacher, error) {
	return p.newAttacherDetacher()
}

//检测指定卷对应的驱动是否需要跳过Attach步骤(如果对应的CSIDriver存在，且Spec.AttachRequired为true,则认为是忽略Attach)
//如果CSIDriver列表中对应驱动不存在, 则不跳过Attach.
func (p *csiPlugin) CanAttach(spec *volume.Spec) (bool, error) {
	inlineEnabled := utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume)
	if inlineEnabled {
		volumeLifecycleMode, err := p.getVolumeLifecycleMode(spec)
		if err != nil {
			return false, err
		}

		if volumeLifecycleMode == storage.VolumeLifecycleEphemeral {
			klog.V(5).Info(log("plugin.CanAttach = false, ephemeral mode detected for spec %v", spec.Name()))
			return false, nil
		}
	}

	pvSrc, err := getCSISourceFromSpec(spec)
	if err != nil {
		return false, err
	}

	driverName := pvSrc.Driver
	//检测指定的驱动是否需要跳过Attach步骤(如果对应的CSIDriver存在，且Spec.AttachRequired为true,则认为是忽略Attach)
	//如果CSIDriver列表中对应驱动不存在, 则不跳过Attach.
	skipAttach, err := p.skipAttach(driverName)
	if err != nil {
		return false, err
	}

	return !skipAttach, nil
}

// CanDeviceMount returns true if the spec supports device mount
func (p *csiPlugin) CanDeviceMount(spec *volume.Spec) (bool, error) {
	inlineEnabled := utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume)
	if !inlineEnabled {
		// No need to check anything, we assume it is a persistent volume.
		return true, nil
	}

	volumeLifecycleMode, err := p.getVolumeLifecycleMode(spec)
	if err != nil {
		return false, err
	}

	if volumeLifecycleMode == storage.VolumeLifecycleEphemeral {
		klog.V(5).Info(log("plugin.CanDeviceMount skipped ephemeral mode detected for spec %v", spec.Name()))
		return false, nil
	}

	// Persistent volumes support device mount.
	return true, nil
}

func (p *csiPlugin) NewDeviceUnmounter() (volume.DeviceUnmounter, error) {
	return p.NewDetacher()
}

func (p *csiPlugin) GetDeviceMountRefs(deviceMountPath string) ([]string, error) {
	m := p.host.GetMounter(p.GetPluginName())
	return m.GetMountRefs(deviceMountPath)
}

// BlockVolumePlugin methods
var _ volume.BlockVolumePlugin = &csiPlugin{}

func (p *csiPlugin) NewBlockVolumeMapper(spec *volume.Spec, podRef *api.Pod, opts volume.VolumeOptions) (volume.BlockVolumeMapper, error) {
	if !p.blockEnabled {
		return nil, errors.New("CSIBlockVolume feature not enabled")
	}

	pvSource, err := getCSISourceFromSpec(spec)
	if err != nil {
		return nil, err
	}
	readOnly, err := getReadOnlyFromSpec(spec)
	if err != nil {
		return nil, err
	}

	klog.V(4).Info(log("setting up block mapper for [volume=%v,driver=%v]", pvSource.VolumeHandle, pvSource.Driver))

	k8s := p.host.GetKubeClient()
	if k8s == nil {
		return nil, errors.New(log("failed to get a kubernetes client"))
	}

	mapper := &csiBlockMapper{
		k8s:        k8s,
		plugin:     p,
		volumeID:   pvSource.VolumeHandle,
		driverName: csiDriverName(pvSource.Driver),
		readOnly:   readOnly,
		spec:       spec,
		specName:   spec.Name(),
		podUID:     podRef.UID,
	}
	mapper.csiClientGetter.driverName = csiDriverName(pvSource.Driver)

	// Save volume info in pod dir
	dataDir := getVolumeDeviceDataDir(spec.Name(), p.host)

	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, errors.New(log("failed to create data dir %s:  %v", dataDir, err))
	}
	klog.V(4).Info(log("created path successfully [%s]", dataDir))

	// persist volume info data for teardown
	node := string(p.host.GetNodeName())
	attachID := getAttachmentName(pvSource.VolumeHandle, pvSource.Driver, node)
	volData := map[string]string{
		volDataKey.specVolID:    spec.Name(),
		volDataKey.volHandle:    pvSource.VolumeHandle,
		volDataKey.driverName:   pvSource.Driver,
		volDataKey.nodeName:     node,
		volDataKey.attachmentID: attachID,
	}

	err = saveVolumeData(dataDir, volDataFileName, volData)
	defer func() {
		// Only if there was an error and volume operation was considered
		// finished, we should remove the directory.
		if err != nil && volumetypes.IsOperationFinishedError(err) {
			// attempt to cleanup volume mount dir.
			if err = removeMountDir(p, dataDir); err != nil {
				klog.Error(log("attacher.MountDevice failed to remove mount dir after error [%s]: %v", dataDir, err))
			}
		}
	}()
	if err != nil {
		errorMsg := log("csi.NewBlockVolumeMapper failed to save volume info data: %v", err)
		klog.Error(errorMsg)
		return nil, errors.New(errorMsg)
	}

	return mapper, nil
}

func (p *csiPlugin) NewBlockVolumeUnmapper(volName string, podUID types.UID) (volume.BlockVolumeUnmapper, error) {
	if !p.blockEnabled {
		return nil, errors.New("CSIBlockVolume feature not enabled")
	}

	klog.V(4).Infof(log("setting up block unmapper for [Spec=%v, podUID=%v]", volName, podUID))
	unmapper := &csiBlockMapper{
		plugin:   p,
		podUID:   podUID,
		specName: volName,
	}

	// load volume info from file
	dataDir := getVolumeDeviceDataDir(unmapper.specName, p.host)
	data, err := loadVolumeData(dataDir, volDataFileName)
	if err != nil {
		return nil, errors.New(log("unmapper failed to load volume data file [%s]: %v", dataDir, err))
	}
	unmapper.driverName = csiDriverName(data[volDataKey.driverName])
	unmapper.volumeID = data[volDataKey.volHandle]
	unmapper.csiClientGetter.driverName = unmapper.driverName

	return unmapper, nil
}

func (p *csiPlugin) ConstructBlockVolumeSpec(podUID types.UID, specVolName, mapPath string) (*volume.Spec, error) {
	if !p.blockEnabled {
		return nil, errors.New("CSIBlockVolume feature not enabled")
	}

	klog.V(4).Infof("plugin.ConstructBlockVolumeSpec [podUID=%s, specVolName=%s, path=%s]", string(podUID), specVolName, mapPath)

	dataDir := getVolumeDeviceDataDir(specVolName, p.host)
	volData, err := loadVolumeData(dataDir, volDataFileName)
	if err != nil {
		return nil, errors.New(log("plugin.ConstructBlockVolumeSpec failed loading volume data using [%s]: %v", mapPath, err))
	}

	klog.V(4).Info(log("plugin.ConstructBlockVolumeSpec extracted [%#v]", volData))

	blockMode := api.PersistentVolumeBlock
	pv := &api.PersistentVolume{
		ObjectMeta: meta.ObjectMeta{
			Name: volData[volDataKey.specVolID],
		},
		Spec: api.PersistentVolumeSpec{
			PersistentVolumeSource: api.PersistentVolumeSource{
				CSI: &api.CSIPersistentVolumeSource{
					Driver:       volData[volDataKey.driverName],
					VolumeHandle: volData[volDataKey.volHandle],
				},
			},
			VolumeMode: &blockMode,
		},
	}

	return volume.NewSpecFromPersistentVolume(pv, false), nil
}

// skipAttach looks up CSIDriver object associated with driver name
// to determine if driver requires attachment volume operation
//检测指定的驱动是否需要跳过Attach步骤(如果对应的CSIDriver存在，且Spec.AttachRequired为true,则认为是忽略Attach)
//如果CSIDriver对象列表中对应驱动不存在, 则不跳过Attach.
func (p *csiPlugin) skipAttach(driver string) (bool, error) {
	kletHost, ok := p.host.(volume.KubeletVolumeHost)
	if ok {
		if err := kletHost.WaitForCacheSync(); err != nil {
			return false, err
		}
	}

	if p.csiDriverLister == nil {
		return false, errors.New("CSIDriver lister does not exist")
	}
	//获取指定驱动的CSIDriver(?CSI driver是谁创建的?)
	csiDriver, err := p.csiDriverLister.Get(driver)
	if err != nil {
		//如果当前驱动不存在CSIDriver对象
		if apierrors.IsNotFound(err) {
			// Don't skip attach if CSIDriver does not exist
			return false, nil
		}
		return false, err
	}
	// csiDriver中指定是否卷必须要先附加到节点
	if csiDriver.Spec.AttachRequired != nil && *csiDriver.Spec.AttachRequired == false {
		return true, nil
	}
	return false, nil
}

// supportsVolumeMode checks whether the CSI driver supports a volume in the given mode.
// An error indicates that it isn't supported and explains why.
func (p *csiPlugin) supportsVolumeLifecycleMode(driver string, volumeMode storage.VolumeLifecycleMode) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume) {
		// Feature disabled, therefore only "persistent" volumes are supported.
		if volumeMode != storage.VolumeLifecyclePersistent {
			return fmt.Errorf("CSIInlineVolume feature not enabled, %q volumes not supported", volumeMode)
		}
		return nil
	}

	// Retrieve CSIDriver. It's not an error if that isn't
	// possible (we don't have the lister if CSIDriverRegistry is
	// disabled) or the driver isn't found (CSIDriver is
	// optional), but then only persistent volumes are supported.
	var csiDriver *storage.CSIDriver
	if p.csiDriverLister != nil {
		kletHost, ok := p.host.(volume.KubeletVolumeHost)
		if ok {
			if err := kletHost.WaitForCacheSync(); err != nil {
				return err
			}
		}

		c, err := p.csiDriverLister.Get(driver)
		if err != nil && !apierrors.IsNotFound(err) {
			// Some internal error.
			return err
		}
		csiDriver = c
	}

	// The right response depends on whether we have information
	// about the driver and the volume mode.
	switch {
	case csiDriver == nil && volumeMode == storage.VolumeLifecyclePersistent:
		// No information, but that's okay for persistent volumes (and only those).
		return nil
	case csiDriver == nil:
		return fmt.Errorf("volume mode %q not supported by driver %s (no CSIDriver object)", volumeMode, driver)
	case containsVolumeMode(csiDriver.Spec.VolumeLifecycleModes, volumeMode):
		// Explicitly listed.
		return nil
	default:
		return fmt.Errorf("volume mode %q not supported by driver %s (only supports %q)", volumeMode, driver, csiDriver.Spec.VolumeLifecycleModes)
	}
}

// containsVolumeMode checks whether the given volume mode is listed.
func containsVolumeMode(modes []storage.VolumeLifecycleMode, mode storage.VolumeLifecycleMode) bool {
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}

// getVolumeLifecycleMode returns the mode for the specified spec: {persistent|ephemeral}.
// 1) If mode cannot be determined, it will default to "persistent".
// 2) If Mode cannot be resolved to either {persistent | ephemeral}, an error is returned
// See https://github.com/kubernetes/enhancements/blob/master/keps/sig-storage/20190122-csi-inline-volumes.md
func (p *csiPlugin) getVolumeLifecycleMode(spec *volume.Spec) (storage.VolumeLifecycleMode, error) {
	// 1) if volume.Spec.Volume.CSI != nil -> mode is ephemeral
	// 2) if volume.Spec.PersistentVolume.Spec.CSI != nil -> persistent
	volSrc, _, err := getSourceFromSpec(spec)
	if err != nil {
		return "", err
	}

	if volSrc != nil && utilfeature.DefaultFeatureGate.Enabled(features.CSIInlineVolume) {
		return storage.VolumeLifecycleEphemeral, nil
	}
	return storage.VolumeLifecyclePersistent, nil
}

//通过driver对应的CSIDriver来决定要跳过挂载，如果跳过则什么都不干；否则根据handle/driver/nodename得到对应的VolumeAttachment，返回其中status.AttachmentMetadata信息
func (p *csiPlugin) getPublishContext(client clientset.Interface, handle, driver, nodeName string) (map[string]string, error) {
	//根据driver对应的CSIDriver对象是否存在，以及.Spec.AttachRequired的值来确认是否跳过挂载
	skip, err := p.skipAttach(driver)
	if err != nil {
		return nil, err
	}

	//跳过挂载，什么都不做
	if skip {
		return nil, nil
	}

	//通过卷名/驱动名/节点名算出一个VolumeAttachments ID
	attachID := getAttachmentName(handle, driver, nodeName)

	// search for attachment by VolumeAttachment.Spec.Source.PersistentVolumeName
	attachment, err := client.StorageV1().VolumeAttachments().Get(context.TODO(), attachID, meta.GetOptions{})
	if err != nil {
		return nil, err // This err already has enough context ("VolumeAttachment xyz not found")
	}

	if attachment == nil {
		err = errors.New("no existing VolumeAttachment found")
		return nil, err
	}
	return attachment.Status.AttachmentMetadata, nil
}

func (p *csiPlugin) newAttacherDetacher() (*csiAttacher, error) {
	k8s := p.host.GetKubeClient()
	if k8s == nil {
		return nil, errors.New(log("unable to get kubernetes client from host"))
	}

	return &csiAttacher{
		plugin:        p,
		k8s:           k8s,
		waitSleepTime: 1 * time.Second,
	}, nil
}

func unregisterDriver(driverName string) error {
	csiDrivers.Delete(driverName)
	//1.将特定的CSIDriver从CSINode.spec列表中移除
	//2.移除node.status.allocatable以及node.status.capacity中指定驱动最大卷挂载数量信息
	//3.将csi driver从node的annotation "csi.volume.kubernetes.io/nodeid"中移除
	if err := nim.UninstallCSIDriver(driverName); err != nil {
		return errors.New(log("Error uninstalling CSI driver: %v", err))
	}

	return nil
}

// Return the highest supported version
func highestSupportedVersion(versions []string) (*utilversion.Version, error) {
	if len(versions) == 0 {
		return nil, errors.New(log("CSI driver reporting empty array for supported versions"))
	}

	var highestSupportedVersion *utilversion.Version
	var theErr error
	for i := len(versions) - 1; i >= 0; i-- {
		currentHighestVer, err := utilversion.ParseGeneric(versions[i])
		if err != nil {
			theErr = err
			continue
		}
		if currentHighestVer.Major() > 1 {
			// CSI currently only has version 0.x and 1.x (see https://github.com/container-storage-interface/spec/releases).
			// Therefore any driver claiming version 2.x+ is ignored as an unsupported versions.
			// Future 1.x versions of CSI are supposed to be backwards compatible so this version of Kubernetes will work with any 1.x driver
			// (or 0.x), but it may not work with 2.x drivers (because 2.x does not have to be backwards compatible with 1.x).
			continue
		}
		if highestSupportedVersion == nil || highestSupportedVersion.LessThan(currentHighestVer) {
			highestSupportedVersion = currentHighestVer
		}
	}

	if highestSupportedVersion == nil {
		return nil, fmt.Errorf("could not find a highest supported version from versions (%v) reported by this driver: %v", versions, theErr)
	}

	if highestSupportedVersion.Major() != 1 {
		// CSI v0.x is no longer supported as of Kubernetes v1.17 in
		// accordance with deprecation policy set out in Kubernetes v1.13
		return nil, fmt.Errorf("highest supported version reported by driver is %v, must be v1.x", highestSupportedVersion)
	}
	return highestSupportedVersion, nil
}

// waitForAPIServerForever waits forever to get a CSINode instance as a proxy
// for a healthy APIServer
//
func waitForAPIServerForever(client clientset.Interface, nodeName types.NodeName) error {
	var lastErr error
	err := wait.PollImmediateInfinite(time.Second, func() (bool, error) {
		// Get a CSINode from API server to make sure 1) kubelet can reach API server
		// and 2) it has enough permissions. Kubelet may have restricted permissions
		// when it's bootstrapping TLS.
		// https://kubernetes.io/docs/reference/command-line-tools-reference/kubelet-tls-bootstrapping/
		_, lastErr = client.StorageV1().CSINodes().Get(context.TODO(), string(nodeName), meta.GetOptions{})
		if lastErr == nil || apierrors.IsNotFound(lastErr) {
			// API server contacted
			return true, nil
		}
		klog.V(2).Infof("Failed to contact API server when waiting for CSINode publishing: %s", lastErr)
		return false, nil
	})
	if err != nil {
		// In theory this is unreachable, but just in case:
		return fmt.Errorf("%v: %v", err, lastErr)
	}

	return nil
}
