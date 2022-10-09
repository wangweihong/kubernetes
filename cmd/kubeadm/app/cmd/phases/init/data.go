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
	"io"

	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
)

// InitData is the interface to use for init phases.
// The "initData" type from "cmd/init.go" must satisfy this interface.
type InitData interface {
	UploadCerts() bool
	CertificateKey() string       // 获取用来对证书内容进行aes加密的key.
	SetCertificateKey(key string) // 设置aes加密key
	SkipCertificateKeyPrint() bool
	Cfg() *kubeadmapi.InitConfiguration //kubeadm 初始化集群配置。
	DryRun() bool
	SkipTokenPrint() bool
	IgnorePreflightErrors() sets.String
	CertificateWriteDir() string // 证书生成目录。默认/etc/kubernetes/pki
	CertificateDir() string
	KubeConfigDir() string  // /etc/kubernetes
	KubeConfigPath() string // /etc/kubernetes/admin.conf
	ManifestDir() string    // control-plane static pod
	KubeletDir() string     // kubelet运行目录, 默认/var/lib/kubelet
	ExternalCA() bool       // 是否指定了外部CA. (指定了外部CA, 则不会生成自签名CA证书以及sa.key,sa.pub, kubeconfig等配置)
	OutputWriter() io.Writer
	Client() (clientset.Interface, error) //基于/etc/kubernetes/admin.conf生成的客户端
	Tokens() []string
	KustomizeDir() string
}
