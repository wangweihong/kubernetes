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

// Package operationexecutor implements interfaces that enable execution of
// register and unregister operations with a
// goroutinemap so that more than one operation is never triggered
// on the same plugin.
package operationexecutor

import (
	"context"
	"fmt"
	"net"
	"time"

	"k8s.io/klog"

	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"k8s.io/client-go/tools/record"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
)

const (
	dialTimeoutDuration   = 10 * time.Second
	notifyTimeoutDuration = 5 * time.Second
)

var _ OperationGenerator = &operationGenerator{}

type operationGenerator struct {

	// recorder is used to record events in the API server
	recorder record.EventRecorder
}

// NewOperationGenerator is returns instance of operationGenerator
func NewOperationGenerator(recorder record.EventRecorder) OperationGenerator {

	return &operationGenerator{
		recorder: recorder,
	}
}

// OperationGenerator interface that extracts out the functions from operation_executor to make it dependency injectable
type OperationGenerator interface {
	// Generates the RegisterPlugin function needed to perform the registration of a plugin
	GenerateRegisterPluginFunc(
		socketPath string,
		timestamp time.Time,
		pluginHandlers map[string]cache.PluginHandler,
		actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error

	// Generates the UnregisterPlugin function needed to perform the unregistration of a plugin
	GenerateUnregisterPluginFunc(
		pluginInfo cache.PluginInfo,
		actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error
}

func (og *operationGenerator) GenerateRegisterPluginFunc(
	socketPath string,
	timestamp time.Time,
	pluginHandlers map[string]cache.PluginHandler,
	actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error {

	registerPluginFunc := func() error {
		//通过注册socket与插件通信
		client, conn, err := dial(socketPath, dialTimeoutDuration)
		if err != nil {
			return fmt.Errorf("RegisterPlugin error -- dial failed at socket %s, err: %v", socketPath, err)
		}
		defer conn.Close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		//通过注册socket来获取插件基本信息，如插件类型（CSIPlugin还是DevicePlugin),名字，通信socket以及版本列表
		//注册的插件都需要实现k8s.io\kubelet\pkg\apis\pluginregistration\v1\api.pb.go的RegistrationClient里面的方法
		//注意：在开发CSI driver不需要实现，是因为CSI sidecar容器node-driver-registar已经帮助开发者实现了该接口。
		infoResp, err := client.GetInfo(ctx, &registerapi.InfoRequest{})
		if err != nil {
			return fmt.Errorf("RegisterPlugin error -- failed to get plugin info using RPC GetInfo at socket %s, err: %v", socketPath, err)
		}
		//获取插件类型对应的注册处理函数. CSIPlugin还是DevicePlugin.
		handler, ok := pluginHandlers[infoResp.Type]
		if !ok {
			//调用插件的注册socket, 告知插件注册失败。
			if err := og.notifyPlugin(client, false, fmt.Sprintf("RegisterPlugin error -- no handler registered for plugin type: %s at socket %s", infoResp.Type, socketPath)); err != nil {
				return fmt.Errorf("RegisterPlugin error -- failed to send error at socket %s, err: %v", socketPath, err)
			}
			return fmt.Errorf("RegisterPlugin error -- no handler registered for plugin type: %s at socket %s", infoResp.Type, socketPath)
		}
		//获得插件的通信端点, 插件的注册socket和通信socket可以不一样
		if infoResp.Endpoint == "" {
			infoResp.Endpoint = socketPath
		}

		//校验插件是否合法
		if err := handler.ValidatePlugin(infoResp.Name, infoResp.Endpoint, infoResp.SupportedVersions); err != nil {
			//调用插件的注册socket, 告知插件注册失败。
			if err = og.notifyPlugin(client, false, fmt.Sprintf("RegisterPlugin error -- plugin validation failed with err: %v", err)); err != nil {
				return fmt.Errorf("RegisterPlugin error -- failed to send error at socket %s, err: %v", socketPath, err)
			}
			return fmt.Errorf("RegisterPlugin error -- pluginHandler.ValidatePluginFunc failed")
		}
		// We add the plugin to the actual state of world cache before calling a plugin consumer's Register handle
		// so that if we receive a delete event during Register Plugin, we can process it as a DeRegister call.
		// 添加插件到plugin manager的实际已注册查检表中
		err = actualStateOfWorldUpdater.AddPlugin(cache.PluginInfo{
			SocketPath: socketPath,    // 注册socket
			Timestamp:  timestamp,     // 注册时间
			Handler:    handler,       // 插件注册/移除注册的回调函数
			Name:       infoResp.Name, // 插件名
		})
		if err != nil {
			klog.Errorf("RegisterPlugin error -- failed to add plugin at socket %s, err: %v", socketPath, err)
		}
		if err := handler.RegisterPlugin(infoResp.Name, infoResp.Endpoint, infoResp.SupportedVersions); err != nil {
			//调用插件的注册socket, 告知插件注册失败。
			//CSIPlugin：如果使用了node-driver-registrar, 通知注册失败的结果会导致node-driver-registrar退出, 程序退出时会移除注册socket。
			//    移除注册socket文件后，pluginManager的Watcher会将该插件移除出期待注册表。在plugin manager的Reconciler调和时，会将该插件从实际注册表中移除。
			return og.notifyPlugin(client, false, fmt.Sprintf("RegisterPlugin error -- plugin registration failed with err: %v", err))
		}

		// Notify is called after register to guarantee that even if notify throws an error Register will always be called after validate
		// 通知插件注册成功
		if err := og.notifyPlugin(client, true, ""); err != nil {
			return fmt.Errorf("RegisterPlugin error -- failed to send registration status at socket %s, err: %v", socketPath, err)
		}
		return nil
	}
	return registerPluginFunc
}

func (og *operationGenerator) GenerateUnregisterPluginFunc(
	pluginInfo cache.PluginInfo,
	actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error {

	unregisterPluginFunc := func() error {
		if pluginInfo.Handler == nil {
			return fmt.Errorf("UnregisterPlugin error -- failed to get plugin handler for %s", pluginInfo.SocketPath)
		}
		// We remove the plugin to the actual state of world cache before calling a plugin consumer's Unregister handle
		// so that if we receive a register event during Register Plugin, we can process it as a Register call.
		// 将插件从实际注册表中移除。
		actualStateOfWorldUpdater.RemovePlugin(pluginInfo.SocketPath)

		pluginInfo.Handler.DeRegisterPlugin(pluginInfo.Name)

		klog.V(4).Infof("DeRegisterPlugin called for %s on %v", pluginInfo.Name, pluginInfo.Handler)
		return nil
	}
	return unregisterPluginFunc
}

//告知插件其注册的结果。插件得到通知后可能会出现以下的结果：
//CSIPlugin：如果使用了node-driver-registrar, 通知注册失败的结果会导致node-driver-registrar退出, 程序退出时会移除注册socket。
//    移除注册socket文件后，pluginManager的Watcher会将该插件移除出期待注册表。在plugin manager的Reconciler调和时，会将该插件从实际注册表中移除。
func (og *operationGenerator) notifyPlugin(client registerapi.RegistrationClient, registered bool, errStr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeoutDuration)
	defer cancel()

	status := &registerapi.RegistrationStatus{
		PluginRegistered: registered,
		Error:            errStr,
	}
	//告知插件的注册状态
	if _, err := client.NotifyRegistrationStatus(ctx, status); err != nil {
		return errors.Wrap(err, errStr)
	}

	if errStr != "" {
		return errors.New(errStr)
	}

	return nil
}

// Dial establishes the gRPC communication with the picked up plugin socket. https://godoc.org/google.golang.org/grpc#Dial
func dial(unixSocketPath string, timeout time.Duration) (registerapi.RegistrationClient, *grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c, err := grpc.DialContext(ctx, unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}),
	)

	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial socket %s, err: %v", unixSocketPath, err)
	}

	return registerapi.NewRegistrationClient(c), c, nil
}
