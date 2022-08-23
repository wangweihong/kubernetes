/*
Copyright 2016 The Kubernetes Authors.

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

package bootstrap

import (
	"context"
	"crypto"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog"

	certificates "k8s.io/api/certificates/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	certificatesv1beta1 "k8s.io/client-go/kubernetes/typed/certificates/v1beta1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/certificate/csr"
	"k8s.io/client-go/util/keyutil"
)

const tmpPrivateKeyFile = "kubelet-client.key.tmp"

// LoadClientConfig tries to load the appropriate client config for retrieving certs and for use by users.
// If bootstrapPath is empty, only kubeconfigPath is checked. If bootstrap path is set and the contents
// of kubeconfigPath are valid, both certConfig and userConfig will point to that file. Otherwise the
// kubeconfigPath on disk is populated based on bootstrapPath but pointing to the location of the client cert
// in certDir. This preserves the historical behavior of bootstrapping where on subsequent restarts the
// most recent client cert is used to request new client certs instead of the initial token.
// 先加载kubeconfig配置作为客户端配置，如果kubeconfig不存在或者里面证书过期；如果指定了bootstrap config则尝试使用bootstrap config结合本地文件系统证书生成新的kubeconfig
// 配置
// 注意: 这里和LoadClientCert区别在于，当kubeconfig配置无法加载时,如会利用bootstapconfig结合本地文件系统上的证书/var/lib/kubelet/pki/kubelet-client-current.pem
// 生成新的kubeconfig, 不管kubelet-client-current.pem证书是否有效。
func LoadClientConfig(kubeconfigPath, bootstrapPath, certDir string) (certConfig, userConfig *restclient.Config, err error) {
	//没有指定自举配置路径
	if len(bootstrapPath) == 0 {
		//加载kubeconfig
		clientConfig, err := loadRESTClientConfig(kubeconfigPath)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to load kubeconfig: %v", err)
		}
		klog.V(2).Infof("No bootstrapping requested, will use kubeconfig")
		return clientConfig, restclient.CopyConfig(clientConfig), nil
	}

	store, err := certificate.NewFileStore("kubelet-client", certDir, certDir, "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build bootstrap cert store")
	}
	//kubeconfig配置文件是否有效，且其中证书没有过期
	ok, err := isClientConfigStillValid(kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}

	// use the current client config
	if ok {
		//加载kubecconfig配置
		clientConfig, err := loadRESTClientConfig(kubeconfigPath)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to load kubeconfig: %v", err)
		}
		klog.V(2).Infof("Current kubeconfig file contents are still valid, no bootstrap necessary")
		return clientConfig, restclient.CopyConfig(clientConfig), nil
	}
	//kubeconfig配置无效时, 回退到自举配置
	bootstrapClientConfig, err := loadRESTClientConfig(bootstrapPath)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to load bootstrap kubeconfig: %v", err)
	}

	//创建匿名客户端?
	clientConfig := restclient.AnonymousClientConfig(bootstrapClientConfig)
	pemPath := store.CurrentPath()
	clientConfig.KeyFile = pemPath // 私钥和证书在同一个文件中。
	clientConfig.CertFile = pemPath
	// 生成KubeConfig配置文件
	if err := writeKubeconfigFromBootstrapping(clientConfig, kubeconfigPath, pemPath); err != nil {
		return nil, nil, err
	}
	klog.V(2).Infof("Use the bootstrap credentials to request a cert, and set kubeconfig to point to the certificate dir")
	return bootstrapClientConfig, clientConfig, nil
}

// LoadClientCert requests a client cert for kubelet if the kubeconfigPath file does not exist.
// The kubeconfig at bootstrapPath is used to request a client certificate from the API server.
// On success, a kubeconfig file referencing the generated key and obtained certificate is written to kubeconfigPath.
// The certificate and key file are stored in certDir.
// 检测kubeconfigPath(/var/lib/kubelet/kubelet.conf)的配置是否有效且没有过期。如果存在有效且没有过期，则什么都不做。
// 否则加载bootstrapPath的自举客户端配置向apiserver创建证书签名申请，通过后更新本地文件系统证书(/var/lib/kubelet/pki/kubelet-client-current.pem)，并且生成kubeconfig配置文件！
func LoadClientCert(kubeconfigPath, bootstrapPath, certDir string, nodeName types.NodeName) error {
	// Short-circuit if the kubeconfig file exists and is valid.
	//加载配置文件，确认配置文件中证书是否存在且仍然有效(没有过期)
	ok, err := isClientConfigStillValid(kubeconfigPath)
	// kubeconfigPath配置存在, 但获取配置信息失败
	if err != nil {
		return err
	}

	// 证书有效，且没有过期
	if ok {
		klog.V(2).Infof("Kubeconfig %s exists and is valid, skipping bootstrap", kubeconfigPath)
		return nil
	}

	klog.V(2).Info("Using bootstrap kubeconfig to generate TLS client cert, key and kubeconfig file")
	// 加载bootstrap配置
	bootstrapClientConfig, err := loadRESTClientConfig(bootstrapPath)
	if err != nil {
		return fmt.Errorf("unable to load bootstrap kubeconfig: %v", err)
	}

	// 用来连接apiserver的客户端
	bootstrapClient, err := certificatesv1beta1.NewForConfig(bootstrapClientConfig)
	if err != nil {
		return fmt.Errorf("unable to create certificates signing request client: %v", err)
	}

	// 创建证书/私钥对文件存储
	store, err := certificate.NewFileStore("kubelet-client", certDir, certDir, "", "")
	if err != nil {
		return fmt.Errorf("unable to build bootstrap cert store")
	}

	var keyData []byte
	// 从指定文件获取私钥和证书对
	if cert, err := store.Current(); err == nil {
		// 如果私钥不为空, 转换私钥数据为PEM格式
		if cert.PrivateKey != nil {
			keyData, err = keyutil.MarshalPrivateKeyToPEM(cert.PrivateKey)
			if err != nil {
				keyData = nil
			}
		}
	}
	// Cache the private key in a separate file until CSR succeeds. This has to
	// be a separate file because store.CurrentPath() points to a symlink
	// managed by the store.
	// /var/lib/kubelet/pki/kubelet-client.key.tmp
	privKeyPath := filepath.Join(certDir, tmpPrivateKeyFile)
	// 确认私钥是否存在, 且合法
	if !verifyKeyData(keyData) {
		klog.V(2).Infof("No valid private key and/or certificate found, reusing existing private key or creating a new one")
		// Note: always call LoadOrGenerateKeyFile so that private key is
		// reused on next startup if CSR request fails.
		// 加载私钥文件，如果存在且合法则什么都不做，否则生成新的ecdsa私钥，并写到私钥文件中
		keyData, _, err = keyutil.LoadOrGenerateKeyFile(privKeyPath)
		if err != nil {
			return err
		}
	}

	// 周期访问apiserver, 直到超时或者有回应
	if err := waitForServer(*bootstrapClientConfig, 1*time.Minute); err != nil {
		klog.Warningf("Error waiting for apiserver to come up: %v", err)
	}

	// 向apiserver创建证书申请,并等待证书申请通过或拒绝或者1小时超时。证书申请成功，则返回证书内容。
	certData, err := requestNodeCertificate(bootstrapClient.CertificateSigningRequests(), keyData, nodeName)
	if err != nil {
		return err
	}

	// 创建/var/lib/kubelet/pki/kubelet-<client|server>-<时间戳>.pem文件，如果存在则清除原内容,写入新的证书和私钥内容。并且更新current符号链接
	if _, err := store.Update(certData, keyData); err != nil {
		return err
	}
	// 删除/var/lib/kubelet/pki/kubelet-client.key.tmp
	if err := os.Remove(privKeyPath); err != nil && !os.IsNotExist(err) {
		klog.V(2).Infof("failed cleaning up private key file %q: %v", privKeyPath, err)
	}

	// 生成kubeconfig配置
	return writeKubeconfigFromBootstrapping(bootstrapClientConfig, kubeconfigPath, store.CurrentPath())
}

// 生成KubeConfig配置文件
func writeKubeconfigFromBootstrapping(bootstrapClientConfig *restclient.Config, kubeconfigPath, pemPath string) error {
	// Get the CA data from the bootstrap client config.
	// bootstrap客户端配置找到ca证书
	caFile, caData := bootstrapClientConfig.CAFile, []byte{}
	if len(caFile) == 0 {
		caData = bootstrapClientConfig.CAData
	}

	// Build resulting kubeconfig.
	//生成kubeconfig配置
	kubeconfigData := clientcmdapi.Config{
		// Define a cluster stanza based on the bootstrap kubeconfig.
		Clusters: map[string]*clientcmdapi.Cluster{"default-cluster": {
			Server:                   bootstrapClientConfig.Host,
			InsecureSkipTLSVerify:    bootstrapClientConfig.Insecure,
			CertificateAuthority:     caFile,
			CertificateAuthorityData: caData,
		}},
		// Define auth based on the obtained client cert.
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"default-auth": {
			ClientCertificate: pemPath, // 指定证书路径
			ClientKey:         pemPath,
		}},
		// Define a context that connects the auth info and cluster, and set it as the default
		Contexts: map[string]*clientcmdapi.Context{"default-context": {
			Cluster:   "default-cluster",
			AuthInfo:  "default-auth",
			Namespace: "default",
		}},
		CurrentContext: "default-context",
	}

	// Marshal to disk
	return clientcmd.WriteToFile(kubeconfigData, kubeconfigPath)
}

//加载配置
func loadRESTClientConfig(kubeconfig string) (*restclient.Config, error) {
	// Load structured kubeconfig data from the given path.
	loader := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	loadedConfig, err := loader.Load()
	if err != nil {
		return nil, err
	}
	// Flatten the loaded data to a particular restclient.Config based on the current context.
	return clientcmd.NewNonInteractiveClientConfig(
		*loadedConfig,
		loadedConfig.CurrentContext,
		&clientcmd.ConfigOverrides{},
		loader,
	).ClientConfig()
}

// isClientConfigStillValid checks the provided kubeconfig to see if it has a valid
// client certificate. It returns true if the kubeconfig is valid, or an error if bootstrapping
// should stop immediately.
// 检测kubeconfig配置文件是否有效，且其中证书没有过期
func isClientConfigStillValid(kubeconfigPath string) (bool, error) {
	// 如果kubelet与apiserver通信的配置存在
	_, err := os.Stat(kubeconfigPath)
	// 不存在, 直接返回
	if os.IsNotExist(err) {
		return false, nil
	}

	// 读取配置失败
	if err != nil {
		return false, fmt.Errorf("error reading existing bootstrap kubeconfig %s: %v", kubeconfigPath, err)
	}
	//加载配置
	bootstrapClientConfig, err := loadRESTClientConfig(kubeconfigPath)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to read existing bootstrap client config: %v", err))
		return false, nil
	}
	//配置转换
	transportConfig, err := bootstrapClientConfig.TransportConfig()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to load transport configuration from existing bootstrap client config: %v", err))
		return false, nil
	}
	// has side effect of populating transport config data fields
	if _, err := transport.TLSConfigFor(transportConfig); err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to load TLS configuration from existing bootstrap client config: %v", err))
		return false, nil
	}

	//解析证书数据
	certs, err := certutil.ParseCertsPEM(transportConfig.TLS.CertData)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to load TLS certificates from existing bootstrap client config: %v", err))
		return false, nil
	}
	if len(certs) == 0 {
		utilruntime.HandleError(fmt.Errorf("unable to read TLS certificates from existing bootstrap client config: %v", err))
		return false, nil
	}

	//检测证书是否过期
	now := time.Now()
	for _, cert := range certs {
		if now.After(cert.NotAfter) {
			utilruntime.HandleError(fmt.Errorf("part of the existing bootstrap client certificate is expired: %s", cert.NotAfter))
			return false, nil
		}
	}
	return true, nil
}

// verifyKeyData returns true if the provided data appears to be a valid private key.
func verifyKeyData(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	_, err := keyutil.ParsePrivateKeyPEM(data)
	return err == nil
}

// 每隔2秒访问apiserver端点，知道apiserver回应或者超时
func waitForServer(cfg restclient.Config, deadline time.Duration) error {
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	cfg.Timeout = 1 * time.Second
	cli, err := restclient.UnversionedRESTClientFor(&cfg)
	if err != nil {
		return fmt.Errorf("couldn't create client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), deadline)
	defer cancel()

	var connected bool
	wait.JitterUntil(func() {
		if _, err := cli.Get().AbsPath("/healthz").Do(context.TODO()).Raw(); err != nil {
			klog.Infof("Failed to connect to apiserver: %v", err)
			return
		}
		cancel()
		connected = true
	}, 2*time.Second, 0.2, true, ctx.Done())

	if !connected {
		return errors.New("timed out waiting to connect to apiserver")
	}
	return nil
}

// requestNodeCertificate will create a certificate signing request for a node
// (Organization and CommonName for the CSR will be set as expected for node
// certificates) and send it to API server, then it will watch the object's
// status, once approved by API server, it will return the API server's issued
// certificate (pem-encoded). If there is any errors, or the watch timeouts, it
// will return an error. This is intended for use on nodes (kubelet and
// kubeadm).
// 向apiserver创建证书申请,并等待证书申请通过或拒绝或者1小时超时.返回申请到的证书内容
func requestNodeCertificate(client certificatesv1beta1.CertificateSigningRequestInterface, privateKeyData []byte, nodeName types.NodeName) (certData []byte, err error) {
	subject := &pkix.Name{
		Organization: []string{"system:nodes"},
		CommonName:   "system:node:" + string(nodeName),
	}
	// 从pem数据中解析出私钥对象
	privateKey, err := keyutil.ParsePrivateKeyPEM(privateKeyData)
	if err != nil {
		return nil, fmt.Errorf("invalid private key for certificate request: %v", err)
	}
	// 基于subject,创建一个用私钥privateKey签名过的证书签名申请(CSR)
	csrData, err := certutil.MakeCSR(privateKey, subject, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to generate certificate request: %v", err)
	}
	//私钥使用目的？ 这会什么作用？
	usages := []certificates.KeyUsage{
		certificates.UsageDigitalSignature, // 数字签名
		certificates.UsageKeyEncipherment,  // 私钥加密
		certificates.UsageClientAuth,       // 客户端授权
	}

	// The Signer interface contains the Public() method to get the public key.
	// 通过私钥对象获取公钥
	signer, ok := privateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key does not implement crypto.Signer")
	}

	// 计算出用于向apiserver创建的csr资源的名称， 以"node-csr-”为前缀
	name, err := digestedName(signer.Public(), subject, usages)
	if err != nil {
		return nil, err
	}

	// 向apiserver创建证书申请，成功创建则返回。如果之前存在且内容兼容没有过期则返回，否则报错
	req, err := csr.RequestCertificate(client, csrData, name, certificates.KubeAPIServerClientKubeletSignerName, usages, privateKey)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3600*time.Second)
	defer cancel()

	klog.V(2).Infof("Waiting for client certificate to be issued")
	// 等待证书申请结果，如果通过返回证书内容
	return csr.WaitForCertificate(ctx, client, req)
}

// This digest should include all the relevant pieces of the CSR we care about.
// We can't directly hash the serialized CSR because of random padding that we
// regenerate every loop and we include usages which are not contained in the
// CSR. This needs to be kept up to date as we add new fields to the node
// certificates and with ensureCompatible.
// 根据主体信息,公钥数据,签名目的计算出以node-csr-<hash>的名字(作为csr资源对象名)
func digestedName(publicKey interface{}, subject *pkix.Name, usages []certificates.KeyUsage) (string, error) {
	hash := sha512.New512_256()

	// Here we make sure two different inputs can't write the same stream
	// to the hash. This delimiter is not in the base64.URLEncoding
	// alphabet so there is no way to have spill over collisions. Without
	// it 'CN:foo,ORG:bar' hashes to the same value as 'CN:foob,ORG:ar'
	const delimiter = '|'
	encode := base64.RawURLEncoding.EncodeToString

	write := func(data []byte) {
		hash.Write([]byte(encode(data)))
		hash.Write([]byte{delimiter})
	}

	publicKeyData, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	write(publicKeyData)

	write([]byte(subject.CommonName))
	for _, v := range subject.Organization {
		write([]byte(v))
	}
	for _, v := range usages {
		write([]byte(v))
	}

	return fmt.Sprintf("node-csr-%s", encode(hash.Sum(nil))), nil
}
