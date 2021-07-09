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

package pluginmanager

import (
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/metrics"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/operationexecutor"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/reconciler"
)

// PluginManager runs a set of asynchronous loops that figure out which plugins
// need to be registered/deregistered and makes it so.
type PluginManager interface {
	// Starts the plugin manager and all the asynchronous loops that it controls
	Run(sourcesReady config.SourcesReady, stopCh <-chan struct{}) // 异步监听插件注册目录/var/lib/kubelet/plugins_registry, 注册未注册的插件或者重注册已更新的插件。

	// AddHandler adds the given plugin handler for a specific plugin type, which
	// will be added to the actual state of world cache so that it can be passed to
	// the desired state of world cache in order to be used during plugin
	// registration/deregistration
	AddHandler(pluginType string, pluginHandler cache.PluginHandler) //添加插件类型的注册处理函数，当前支持CSIPlugin以及DevicePlugin.
}

const (
	// loopSleepDuration is the amount of time the reconciler loop waits
	// between successive executions
	loopSleepDuration = 1 * time.Second //调试周期
)

// NewPluginManager returns a new concrete instance implementing the
// PluginManager interface.
func NewPluginManager(
	sockDir string,
	recorder record.EventRecorder) PluginManager {
	asw := cache.NewActualStateOfWorld()    // 实际已注册插件表
	dsw := cache.NewDesiredStateOfWorld()   // 期待注册插件表
	reconciler := reconciler.NewReconciler( //调和器。 比对实际已注册查检表以及期待注册查检表，移除不再期待注册的插件，注册期待的注册插件。
		operationexecutor.NewOperationExecutor(
			operationexecutor.NewOperationGenerator(
				recorder,
			),
		),
		loopSleepDuration, //调和器调和间隔时间，默认1秒。
		dsw,
		asw,
	)

	pm := &pluginManager{
		desiredStateOfWorldPopulator: pluginwatcher.NewWatcher(
			sockDir, // 路径/var/lib/kubelet/plugins_regsitry
			dsw,     //记录以上目录树unix domain socket文件信息
		),
		reconciler:          reconciler,
		desiredStateOfWorld: dsw, // 每当/var/lib/kubelet/plugins_regsitry目录树有非"."的unix domain socket文件，watcher会将期待注册插件添加到该表。当socket文件移除时，插件会移出该表
		actualStateOfWorld:  asw, // 当有期待注册的插件注册成功时，reconciler会将注册成功的插件添加到该表。
	}
	return pm
}

// pluginManager implements the PluginManager interface
type pluginManager struct {
	// desiredStateOfWorldPopulator (the plugin watcher) runs an asynchronous
	// periodic loop to populate the desiredStateOfWorld.
	desiredStateOfWorldPopulator *pluginwatcher.Watcher // 插件注册目录监视器/var/lib/kubelet/plugins_registry以及子目录中非
	// '.'前缀的unix domain socket文件，watcher会将期待注册的插件会添加到期待注册插件表中
	// 当期待注册的插件socket文件从注册目录移除时，watcher会将插件从期待注册表中移除。

	// reconciler runs an asynchronous periodic loop to reconcile the
	// desiredStateOfWorld with the actualStateOfWorld by triggering register
	// and unregister operations using the operationExecutor.
	reconciler reconciler.Reconciler // 调和器。 周期(默认1秒）比对实际已注册查检表以及期待注册查检表，移除不再期待注册的插件，注册期待的注册插件。
	// 调和器会将需要移除注册的插件移出实际注册表。并调用相应的插件处理函数（取决于插件的类型）
	// 期待注册的插件注册成功后会添加到实际注册表。

	// actualStateOfWorld is a data structure containing the actual state of
	// the world according to the manager: i.e. which plugins are registered.
	// The data structure is populated upon successful completion of register
	// and unregister actions triggered by the reconciler.
	actualStateOfWorld cache.ActualStateOfWorld // 实际已注册插件表. 当pluginManager的reconciler调和时，将新注册的插件添加到实际已注册表中。

	// desiredStateOfWorld is a data structure containing the desired state of
	// the world according to the plugin manager: i.e. what plugins are registered.
	// The data structure is populated by the desired state of the world
	// populator (plugin watcher).
	desiredStateOfWorld cache.DesiredStateOfWorld //期待注册插件表，当/var/lib/kubelet/plugins_regsitry目录树有非"."的unix domain socket文件创建时，pluginManager的Watcher将会添加到该表
	// 当socket文件移除时，Watcher将插件从该表中移除
}

var _ PluginManager = &pluginManager{}

func (pm *pluginManager) Run(sourcesReady config.SourcesReady, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	// 启动fsnotify监听目录/var/lib/kubelet/plugins_registry以及子目录树, 一旦有非'.'的unix domain socket存在或者刚创建的
	// socket信息将保存到pluginManager.desiredStateOfWorld表中
	pm.desiredStateOfWorldPopulator.Start(stopCh)
	klog.V(2).Infof("The desired_state_of_world populator (plugin watcher) starts")

	klog.Infof("Starting Kubelet Plugin Manager")
	go pm.reconciler.Run(stopCh)
	//这里记录期待注册表和实际注册表中的指标
	metrics.Register(pm.actualStateOfWorld, pm.desiredStateOfWorld)
	<-stopCh
	klog.Infof("Shutting down Kubelet Plugin Manager")
}

//添加不同类型的插件的注册处理函数。
//当前支持两种类型：CSIPlugin以及DevicePlugin。 处理函数实际上是由reconciler在注册和移除插件时使用。
func (pm *pluginManager) AddHandler(pluginType string, handler cache.PluginHandler) {
	pm.reconciler.AddHandler(pluginType, handler)
}
