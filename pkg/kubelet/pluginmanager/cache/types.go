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

package cache

// PluginHandler is an interface a client of the pluginwatcher API needs to implement in
// order to consume plugins
// The PluginHandler follows the simple following state machine:
//
//                         +--------------------------------------+
//                         |            ReRegistration            |
//                         | Socket created with same plugin name |
//                         |                                      |
//                         |                                      |
//    Socket Created       v                                      +        Socket Deleted
// +------------------> Validate +---------------------------> Register +------------------> DeRegister
//                         +                                      +                              +
//                         |                                      |                              |
//                         | Error                                | Error                        |
//                         |                                      |                              |
//                         v                                      v                              v
//                        Out                                    Out                            Out
//
// The pluginwatcher module follows strictly and sequentially this state machine for each *plugin name*.
// e.g: If you are Registering a plugin foo, you cannot get a DeRegister call for plugin foo
//      until the Register("foo") call returns. Nor will you get a Validate("foo", "Different endpoint", ...)
//      call until the Register("foo") call returns.
//
// ReRegistration: Socket created with same plugin name, usually for a plugin update
// e.g: plugin with name foo registers at foo.com/foo-1.9.7 later a plugin with name foo
//      registers at foo.com/foo-1.9.9
//
// DeRegistration: When ReRegistration happens only the deletion of the new socket will trigger a DeRegister call
//这个接口有两个实现，一是DevicePlugin；二是CSIPlugin
type PluginHandler interface {
	// Validate returns an error if the information provided by
	// the potential plugin is erroneous (unsupported version, ...)
	//检测插件是否合法
	//plugin注册时必须实现k8s.io\kubelet\pkg\apis\pluginregistration\v1.RegistrationClient
	//通过RegistrationClient.GetInfo()已经获得插件的名字/支持版本/类型/访问端点等信息。
	//得知以上信息后才调用ValidatePlugin进行验证。
	//CSIPlugin: 检测是否由同名的插件已注册，如果已注册，比较当前期待注册的插件与已注册的插件的版本。如果校验失败，告知插件注册失败。
	ValidatePlugin(pluginName string, endpoint string, versions []string) error
	// RegisterPlugin is called so that the plugin can be register by any
	// plugin consumer
	// Error encountered here can still be Notified to the plugin.
	//真正注册插件
	RegisterPlugin(pluginName, endpoint string, versions []string) error
	// DeRegister is called once the pluginwatcher observes that the socket has
	// been deleted.
	//移除插件的注册
	DeRegisterPlugin(pluginName string)
}
