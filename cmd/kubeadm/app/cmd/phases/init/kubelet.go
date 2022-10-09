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

package phases

import (
	"fmt"

	"github.com/pkg/errors"
	"k8s.io/klog"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
	"k8s.io/kubernetes/cmd/kubeadm/app/componentconfigs"
	kubeletphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/kubelet"
)

var (
	kubeletStartPhaseExample = cmdutil.Examples(`
		# Writes a dynamic environment file with kubelet flags from a InitConfiguration file.
		kubeadm init phase kubelet-start --config config.yaml
		`)
)

// NewKubeletStartPhase creates a kubeadm workflow phase that start kubelet on a node.
func NewKubeletStartPhase() workflow.Phase {
	return workflow.Phase{
		Name:    "kubelet-start",
		Short:   "Write kubelet settings and (re)start the kubelet",
		Long:    "Write a file with KubeletConfiguration and an environment file with node specific kubelet settings, and then (re)start kubelet.",
		Example: kubeletStartPhaseExample,
		//从kubeadm配置中提取kubelet相关配置,写到/var/lib/kubelet/config.yaml,并尝试启动 kubelet.service
		Run: runKubeletStart,
		InheritFlags: []string{
			options.CfgPath,
			options.NodeCRISocket,
			options.NodeName,
		},
	}
}

// runKubeletStart executes kubelet start logic.
// 从kubeadm配置中提取kubelet相关配置,写到/var/lib/kubelet/config.yaml,并尝试启动 kubelet.service
func runKubeletStart(c workflow.RunData) error {
	data, ok := c.(InitData)
	if !ok {
		return errors.New("kubelet-start phase invoked with an invalid data struct")
	}

	// First off, configure the kubelet. In this short timeframe, kubeadm is trying to stop/restart the kubelet
	// Try to stop the kubelet service so no race conditions occur when configuring it
	if !data.DryRun() {
		klog.V(1).Infoln("Stopping the kubelet")
		kubeletphase.TryStopKubelet()
	}

	// Write env file with flags for the kubelet to use. We do not need to write the --register-with-taints for the control-plane,
	// as we handle that ourselves in the mark-control-plane phase
	// TODO: Maybe we want to do that some time in the future, in order to remove some logic from the mark-control-plane phase?
	// 将kubelet运行时参数写到/var/lib/kubelet/kubeadm-flags.env
	if err := kubeletphase.WriteKubeletDynamicEnvFile(&data.Cfg().ClusterConfiguration, &data.Cfg().NodeRegistration, false, data.KubeletDir()); err != nil {
		return errors.Wrap(err, "error writing a dynamic environment file for the kubelet")
	}

	// 从kubeadm配置读取kubelet的配置
	kubeletCfg, ok := data.Cfg().ComponentConfigs[componentconfigs.KubeletGroup]
	if !ok {
		return errors.New("no kubelet component config found in the active component config set")
	}

	// Write the kubelet configuration file to disk.
	// 将kubeadm配置中kubelet部分写到/var/lib/kubelet/config.yaml
	// apt安装的kubelet,会配置kubelet运行时指定参数--config=/var/lib/kubelet/config.yaml
	if err := kubeletphase.WriteConfigToDisk(kubeletCfg, data.KubeletDir()); err != nil {
		return errors.Wrap(err, "error writing kubelet configuration to disk")
	}

	// Try to start the kubelet service in case it's inactive
	if !data.DryRun() {
		fmt.Println("[kubelet-start] Starting the kubelet")
		// 利用systemd重启kubelet, 重启失败不报错
		kubeletphase.TryStartKubelet()
	}

	return nil
}
