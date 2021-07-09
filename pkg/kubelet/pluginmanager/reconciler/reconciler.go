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

// Package reconciler implements interfaces that attempt to reconcile the
// desired state of the world with the actual state of the world by triggering
// relevant actions (register/deregister plugins).
package reconciler

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/operationexecutor"
	"k8s.io/kubernetes/pkg/util/goroutinemap"
	"k8s.io/kubernetes/pkg/util/goroutinemap/exponentialbackoff"
)

// Reconciler runs a periodic loop to reconcile the desired state of the world
// with the actual state of the world by triggering register and unregister
// operations.
type Reconciler interface {
	// Starts running the reconciliation loop which executes periodically, checks
	// if plugins that should be registered are register and plugins that should be
	// unregistered are unregistered. If not, it will trigger register/unregister
	// operations to rectify.
	//每隔1秒时间进行比对期待注册表和实际注册表, 取消不存在期待注册表中的已注册插件的注册，注册仍未注册的插件。
	//当已注册插件更新时，重新注册该插件。
	Run(stopCh <-chan struct{})

	// AddHandler adds the given plugin handler for a specific plugin type
	//添加特定类型的插件的注册处理函数
	//当前支持两种类型：CSIPlugin以及DevicePlugin
	AddHandler(pluginType string, pluginHandler cache.PluginHandler)
}

// NewReconciler returns a new instance of Reconciler.
//
// loopSleepDuration - the amount of time the reconciler loop sleeps between
//   successive executions
//   syncDuration - the amount of time the syncStates sleeps between
//   successive executions
// operationExecutor - used to trigger register/unregister operations safely
//   (prevents more than one operation from being triggered on the same
//   socket path)
// desiredStateOfWorld - cache containing the desired state of the world
// actualStateOfWorld - cache containing the actual state of the world
func NewReconciler(
	operationExecutor operationexecutor.OperationExecutor,
	loopSleepDuration time.Duration,
	desiredStateOfWorld cache.DesiredStateOfWorld,
	actualStateOfWorld cache.ActualStateOfWorld) Reconciler {
	return &reconciler{
		operationExecutor:   operationExecutor,
		loopSleepDuration:   loopSleepDuration,
		desiredStateOfWorld: desiredStateOfWorld,
		actualStateOfWorld:  actualStateOfWorld,
		handlers:            make(map[string]cache.PluginHandler),
	}
}

type reconciler struct {
	operationExecutor operationexecutor.OperationExecutor //插件注册
	loopSleepDuration time.Duration                       //调和时间。每隔该时间进行比对期待注册表和实际注册表, 取消不存在期待注册表中的已注册插件的注册
	//注册仍未注册的插件。
	desiredStateOfWorld cache.DesiredStateOfWorld      //期待注册插件表
	actualStateOfWorld  cache.ActualStateOfWorld       //实际已注册插件表
	handlers            map[string]cache.PluginHandler //不同类型插件的注册处理函数
	sync.RWMutex
}

var _ Reconciler = &reconciler{}

func (rc *reconciler) Run(stopCh <-chan struct{}) {
	//周期执行（当前间隔为1秒），但只有上一次调和结束，才会执行下一次调和
	wait.Until(func() {
		rc.reconcile() //每隔该时间进行比对期待注册表和实际注册表, 取消不存在期待注册表中的已注册插件的注册，注册仍未注册的插件。
		//当已注册插件更新时，重新注册该插件。
	},
		rc.loopSleepDuration, // 间隔时间1秒
		stopCh)
}

//添加不同类型插件的注册/取消注册处理函数
//目前只支持CSIPlugin以及DevicePlugin
func (rc *reconciler) AddHandler(pluginType string, pluginHandler cache.PluginHandler) {
	rc.Lock()
	defer rc.Unlock()

	rc.handlers[pluginType] = pluginHandler
}

func (rc *reconciler) getHandlers() map[string]cache.PluginHandler {
	rc.RLock()
	defer rc.RUnlock()

	return rc.handlers
}

func (rc *reconciler) reconcile() {
	// Unregisterations are triggered before registrations

	// Ensure plugins that should be unregistered are unregistered.
	//1. 先移除实际注册表中存在，但在期待注册表中已经没有的插件
	//2. 移除实际/期待注册表中都存在，但期待注册的时间比实际注册的时间晚（很可能插件已经更新）
	for _, registeredPlugin := range rc.actualStateOfWorld.GetRegisteredPlugins() {
		//表明指定插件是否需要取消注册
		unregisterPlugin := false
		// 实际已注册的插件，并没有在期待插件注册表中，则应该取消当前的插件的注册
		if !rc.desiredStateOfWorld.PluginExists(registeredPlugin.SocketPath) {
			unregisterPlugin = true
		} else {
			// We also need to unregister the plugins that exist in both actual state of world
			// and desired state of world cache, but the timestamps don't match.
			// Iterate through desired state of world plugins and see if there's any plugin
			// with the same socket path but different timestamp.
			//同名插件在实际已注册表的注册时间比期望注册表中时间更早, 说明插件已经更新。则应取消该插件的注册。
			for _, dswPlugin := range rc.desiredStateOfWorld.GetPluginsToRegister() {
				if dswPlugin.SocketPath == registeredPlugin.SocketPath && dswPlugin.Timestamp != registeredPlugin.Timestamp {
					klog.V(5).Infof(registeredPlugin.GenerateMsgDetailed("An updated version of plugin has been found, unregistering the plugin first before reregistering", ""))
					unregisterPlugin = true
					break
				}
			}
		}

		if unregisterPlugin {
			klog.V(5).Infof(registeredPlugin.GenerateMsgDetailed("Starting operationExecutor.UnregisterPlugin", ""))
			err := rc.operationExecutor.UnregisterPlugin(registeredPlugin, rc.actualStateOfWorld)
			if err != nil &&
				!goroutinemap.IsAlreadyExists(err) &&
				!exponentialbackoff.IsExponentialBackoff(err) {
				// Ignore goroutinemap.IsAlreadyExists and exponentialbackoff.IsExponentialBackoff errors, they are expected.
				// Log all other errors.
				klog.Errorf(registeredPlugin.GenerateErrorDetailed("operationExecutor.UnregisterPlugin failed", err).Error())
			}
			if err == nil {
				klog.V(1).Infof(registeredPlugin.GenerateMsgDetailed("operationExecutor.UnregisterPlugin started", ""))
			}
		}
	}

	// Ensure plugins that should be registered are registered
	// 注册期待注册表中的仍未注册到实际注册表中的插件
	for _, pluginToRegister := range rc.desiredStateOfWorld.GetPluginsToRegister() {
		//如果期待注册的插件不存在实际注册表，同时
		if !rc.actualStateOfWorld.PluginExistsWithCorrectTimestamp(pluginToRegister) {
			klog.V(5).Infof(pluginToRegister.GenerateMsgDetailed("Starting operationExecutor.RegisterPlugin", ""))
			//如果注册成功，插件信息会添加到实际注册表中。**注册失败后，将会通知插件注册失败**
			err := rc.operationExecutor.RegisterPlugin(pluginToRegister.SocketPath, pluginToRegister.Timestamp, rc.getHandlers(), rc.actualStateOfWorld)
			if err != nil &&
				!goroutinemap.IsAlreadyExists(err) &&
				!exponentialbackoff.IsExponentialBackoff(err) {
				// Ignore goroutinemap.IsAlreadyExists and exponentialbackoff.IsExponentialBackoff errors, they are expected.
				klog.Errorf(pluginToRegister.GenerateErrorDetailed("operationExecutor.RegisterPlugin failed", err).Error())
			}
			if err == nil {
				klog.V(1).Infof(pluginToRegister.GenerateMsgDetailed("operationExecutor.RegisterPlugin started", ""))
			}
		}
	}
}
