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

/*
Package cache implements data structures used by the kubelet plugin manager to
keep track of registered plugins.
*/
package cache

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/klog"
)

// ActualStateOfWorld defines a set of thread-safe operations for the kubelet
// plugin manager's actual state of the world cache.
// This cache contains a map of socket file path to plugin information of
// all plugins attached to this node.
// 这里就是一个带读写锁的map, 记录实际上已注册的插件
type ActualStateOfWorld interface {

	// GetRegisteredPlugins generates and returns a list of plugins
	// that are successfully registered plugins in the current actual state of world.
	//获取所有已注册表
	GetRegisteredPlugins() []PluginInfo

	// AddPlugin add the given plugin in the cache.
	// An error will be returned if socketPath of the PluginInfo object is empty.
	// Note that this is different from desired world cache's AddOrUpdatePlugin
	// because for the actual state of world cache, there won't be a scenario where
	// we need to update an existing plugin if the timestamps don't match. This is
	// because the plugin should have been unregistered in the reconciller and therefore
	// removed from the actual state of world cache first before adding it back into
	// the actual state of world cache again with the new timestamp
	AddPlugin(pluginInfo PluginInfo) error

	// RemovePlugin deletes the plugin with the given socket path from the actual
	// state of world.
	// If a plugin does not exist with the given socket path, this is a no-op.
	//将插件从实际注册表中移除
	RemovePlugin(socketPath string)

	// PluginExists checks if the given plugin exists in the current actual
	// state of world cache with the correct timestamp
	PluginExistsWithCorrectTimestamp(pluginInfo PluginInfo) bool //检测指定插件是否已存在实际注册表而且注册时间一致。
}

// NewActualStateOfWorld returns a new instance of ActualStateOfWorld
func NewActualStateOfWorld() ActualStateOfWorld {
	return &actualStateOfWorld{
		socketFileToInfo: make(map[string]PluginInfo), //key是插件的注册socket
	}
}

type actualStateOfWorld struct {

	// socketFileToInfo is a map containing the set of successfully registered plugins
	// The keys are plugin socket file paths. The values are PluginInfo objects
	socketFileToInfo map[string]PluginInfo // 已注册插件信息， key是插件的注册socket路径
	sync.RWMutex
}

var _ ActualStateOfWorld = &actualStateOfWorld{}

// PluginInfo holds information of a plugin
//同时用来描述期待注册时和实际注册后的插件信息
// 在期待注册时：插件只知道期待注册时间以及注册socket的路径，其余信息未知
// 实际注册后： kubelet已经获取插件的类型/插件名。
type PluginInfo struct {
	SocketPath string        // 插件unix domain socket的路径
	Timestamp  time.Time     // 插件注册时间.当同名插件在期望注册表中的注册时间比实际已注册表的时间更新时，将会认为插件已更新，从而导致插件卸载后重新注册
	Handler    PluginHandler //插件注册/移除的处理回调函数（插件注册后才会设置，即desiredStateOfWorld表中的插件没有这个信息）
	Name       string        //插件名（注册后才知道，即desiredStateOfWorld表中的插件没有这个信息）
}

//添加插件到实际注册表中
func (asw *actualStateOfWorld) AddPlugin(pluginInfo PluginInfo) error {
	asw.Lock()
	defer asw.Unlock()

	if pluginInfo.SocketPath == "" {
		return fmt.Errorf("socket path is empty")
	}
	if _, ok := asw.socketFileToInfo[pluginInfo.SocketPath]; ok {
		klog.V(2).Infof("Plugin (Path %s) exists in actual state cache", pluginInfo.SocketPath)
	}
	asw.socketFileToInfo[pluginInfo.SocketPath] = pluginInfo
	return nil
}

func (asw *actualStateOfWorld) RemovePlugin(socketPath string) {
	asw.Lock()
	defer asw.Unlock()

	delete(asw.socketFileToInfo, socketPath)
}

func (asw *actualStateOfWorld) GetRegisteredPlugins() []PluginInfo {
	asw.RLock()
	defer asw.RUnlock()

	currentPlugins := []PluginInfo{}
	for _, pluginInfo := range asw.socketFileToInfo {
		currentPlugins = append(currentPlugins, pluginInfo)
	}
	return currentPlugins
}

func (asw *actualStateOfWorld) PluginExistsWithCorrectTimestamp(pluginInfo PluginInfo) bool {
	asw.RLock()
	defer asw.RUnlock()

	// We need to check both if the socket file path exists, and the timestamp
	// matches the given plugin (from the desired state cache) timestamp
	actualStatePlugin, exists := asw.socketFileToInfo[pluginInfo.SocketPath]
	return exists && (actualStatePlugin.Timestamp == pluginInfo.Timestamp)
}
