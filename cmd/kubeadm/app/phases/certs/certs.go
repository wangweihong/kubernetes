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

package certs

import (
	"crypto"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/klog"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	pkiutil "k8s.io/kubernetes/cmd/kubeadm/app/util/pkiutil"

	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
)

// CreatePKIAssets will create and write to disk all PKI assets necessary to establish the control plane.
// If the PKI assets already exists in the target folder, they are used only if evaluated equal; otherwise an error is returned.
func CreatePKIAssets(cfg *kubeadmapi.InitConfiguration) error {
	klog.V(1).Infoln("creating PKI assets")

	// This structure cannot handle multilevel CA hierarchies.
	// This isn't a problem right now, but may become one in the future.

	var certList Certificates

	if cfg.Etcd.Local == nil {
		certList = GetCertsWithoutEtcd()
	} else {
		certList = GetDefaultCertList()
	}

	certTree, err := certList.AsMap().CertTree()
	if err != nil {
		return err
	}

	if err := certTree.CreateTree(cfg); err != nil {
		return errors.Wrap(err, "error creating PKI assets")
	}

	fmt.Printf("[certs] Valid certificates and keys now exist in %q\n", cfg.CertificatesDir)

	// Service accounts are not x509 certs, so handled separately
	return CreateServiceAccountKeyAndPublicKeyFiles(cfg.CertificatesDir, cfg.ClusterConfiguration.PublicKeyAlgorithm())
}

// CreateServiceAccountKeyAndPublicKeyFiles creates new public/private key files for signing service account users.
// If the sa public/private key files already exist in the target folder, they are used only if evaluated equals; otherwise an error is returned.
// 如果/etc/kubernetes/pki/sa.key存在,则直接返回。否则创建sa.key, 同时生成公钥文件 sa.pub
func CreateServiceAccountKeyAndPublicKeyFiles(certsDir string, keyType x509.PublicKeyAlgorithm) error {
	klog.V(1).Infoln("creating new public/private key files for signing service account users")
	// 从/etc/kubernetes/pki/sa.key文件读取私钥
	_, err := keyutil.PrivateKeyFromFile(filepath.Join(certsDir, kubeadmconstants.ServiceAccountPrivateKeyName))
	if err == nil {
		// kubeadm doesn't validate the existing certificate key more than this;
		// Basically, if we find a key file with the same path kubeadm thinks those files
		// are equal and doesn't bother writing a new file
		fmt.Printf("[certs] Using the existing %q key\n", kubeadmconstants.ServiceAccountKeyBaseName)
		return nil
	} else if !os.IsNotExist(err) {
		return errors.Wrapf(err, "file %s existed but it could not be loaded properly", kubeadmconstants.ServiceAccountPrivateKeyName)
	}
	// /etc/kubernetes/pki/sa.key不存在,生成
	// The key does NOT exist, let's generate it now
	// 生成指定算法的密钥对
	key, err := pkiutil.NewPrivateKey(keyType)
	if err != nil {
		return err
	}

	// Write .key and .pub files to disk
	fmt.Printf("[certs] Generating %q key and public key\n", kubeadmconstants.ServiceAccountKeyBaseName)

	// 将密钥对数据写到/etc/kubernetes/pki/sa.key
	if err := pkiutil.WriteKey(certsDir, kubeadmconstants.ServiceAccountKeyBaseName, key); err != nil {
		return err
	}

	// 读取密钥对中的公钥写到/etc/kubernetes/pki/sa.pub
	return pkiutil.WritePublicKey(certsDir, kubeadmconstants.ServiceAccountKeyBaseName, key.Public())
}

// CreateCACertAndKeyFiles generates and writes out a given certificate authority.
// The certSpec should be one of the variables from this package.
//  创建私钥，生成证书有效期为10年的X509自签名CA证书。如果指定路径下证书或者私钥文件已经存在, 则加载并检测是否为CA证书。如果不存在, 则写入CA证书和私钥
func CreateCACertAndKeyFiles(certSpec *KubeadmCert, cfg *kubeadmapi.InitConfiguration) error {
	// 创建的是CA证书, 不能指定CA项
	if certSpec.CAName != "" {
		return errors.Errorf("this function should only be used for CAs, but cert %s has CA %s", certSpec.Name, certSpec.CAName)
	}
	klog.V(1).Infof("creating a new certificate authority for %s", certSpec.Name)

	certConfig, err := certSpec.GetConfig(cfg)
	if err != nil {
		return err
	}

	// 创建私钥，生成证书有效期为10年的X509自签名CA证书
	caCert, caKey, err := pkiutil.NewCertificateAuthority(certConfig)
	if err != nil {
		return err
	}

	// 如果指定路径下证书或者私钥文件已经存在, 则加载并检测是否为CA证书。如果不存在, 则写入CA证书和私钥
	return writeCertificateAuthorityFilesIfNotExist(
		cfg.CertificatesDir,
		certSpec.BaseName,
		caCert,
		caKey,
	)
}

// NewCSR will generate a new CSR and accompanying key
func NewCSR(certSpec *KubeadmCert, cfg *kubeadmapi.InitConfiguration) (*x509.CertificateRequest, crypto.Signer, error) {
	certConfig, err := certSpec.GetConfig(cfg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to retrieve cert configuration")
	}

	return pkiutil.NewCSRAndKey(certConfig)
}

// CreateCSR creates a certificate signing request
func CreateCSR(certSpec *KubeadmCert, cfg *kubeadmapi.InitConfiguration, path string) error {
	csr, key, err := NewCSR(certSpec, cfg)
	if err != nil {
		return err
	}
	return writeCSRFilesIfNotExist(path, certSpec.BaseName, csr, key)
}

// CreateCertAndKeyFilesWithCA loads the given certificate authority from disk, then generates and writes out the given certificate and key.
// The certSpec and caCertSpec should both be one of the variables from this package.
// 创建指定的ca证书caCert签名的证书，如果证书私钥已经存在, 则检验证书是否由caCert签名
func CreateCertAndKeyFilesWithCA(certSpec *KubeadmCert, caCertSpec *KubeadmCert, cfg *kubeadmapi.InitConfiguration) error {
	// 检测证书要求的颁发者是否为指定的ca
	if certSpec.CAName != caCertSpec.Name {
		return errors.Errorf("expected CAname for %s to be %q, but was %s", certSpec.Name, certSpec.CAName, caCertSpec.Name)
	}

	//加载ca证书和私钥
	caCert, caKey, err := LoadCertificateAuthority(cfg.CertificatesDir, caCertSpec.BaseName)
	if err != nil {
		return errors.Wrapf(err, "couldn't load CA certificate %s", caCertSpec.Name)
	}

	// 如果证书、私钥存在, 则校验证书是不是由caCert签名。否则创建由caCert签名的证书
	return certSpec.CreateFromCA(cfg, caCert, caKey)
}

// LoadCertificateAuthority tries to load a CA in the given directory with the given name.
// 加载指定目录下的CA证书和私钥
func LoadCertificateAuthority(pkiDir string, baseName string) (*x509.Certificate, crypto.Signer, error) {
	// Checks if certificate authority exists in the PKI directory
	//  检测是否指定证书或私钥至少有一个存在， 如果都不存在直接报错
	if !pkiutil.CertOrKeyExist(pkiDir, baseName) {
		return nil, nil, errors.Errorf("couldn't load %s certificate authority from %s", baseName, pkiDir)
	}

	// Try to load certificate authority .crt and .key from the PKI directory
	// 加载/etc/kubernetes/pki下指定的证书和私钥
	caCert, caKey, err := pkiutil.TryLoadCertAndKeyFromDisk(pkiDir, baseName)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failure loading %s certificate authority", baseName)
	}

	// Make sure the loaded CA cert actually is a CA
	if !caCert.IsCA {
		return nil, nil, errors.Errorf("%s certificate is not a certificate authority", baseName)
	}

	return caCert, caKey, nil
}

// writeCertificateAuthorityFilesIfNotExist write a new certificate Authority to the given path.
// If there already is a certificate file at the given path; kubeadm tries to load it and check if the values in the
// existing and the expected certificate equals. If they do; kubeadm will just skip writing the file as it's up-to-date,
// otherwise this function returns an error.
// 如果指定路径下证书或者私钥文件已经存在, 则加载并检测是否为CA证书。如果不存在, 则写入CA证书和私钥
func writeCertificateAuthorityFilesIfNotExist(pkiDir string, baseName string, caCert *x509.Certificate, caKey crypto.Signer) error {

	// If cert or key exists, we should try to load them
	if pkiutil.CertOrKeyExist(pkiDir, baseName) {

		// Try to load .crt and .key from the PKI directory
		caCert, _, err := pkiutil.TryLoadCertAndKeyFromDisk(pkiDir, baseName)
		if err != nil {
			return errors.Wrapf(err, "failure loading %s certificate", baseName)
		}

		// Check if the existing cert is a CA
		if !caCert.IsCA {
			return errors.Errorf("certificate %s is not a CA", baseName)
		}

		// kubeadm doesn't validate the existing certificate Authority more than this;
		// Basically, if we find a certificate file with the same path; and it is a CA
		// kubeadm thinks those files are equal and doesn't bother writing a new file
		fmt.Printf("[certs] Using the existing %q certificate and key\n", baseName)
	} else {
		// Write .crt and .key files to disk
		fmt.Printf("[certs] Generating %q certificate and key\n", baseName)

		if err := pkiutil.WriteCertAndKey(pkiDir, baseName, caCert, caKey); err != nil {
			return errors.Wrapf(err, "failure while saving %s certificate and key", baseName)
		}
	}
	return nil
}

// writeCertificateFilesIfNotExist write a new certificate to the given path.
// If there already is a certificate file at the given path; kubeadm tries to load it and check if the values in the
// existing and the expected certificate equals. If they do; kubeadm will just skip writing the file as it's up-to-date,
// otherwise this function returns an error.
// 如果证书、私钥存在, 则校验证书是不是有signingCert签名。
func writeCertificateFilesIfNotExist(pkiDir string, baseName string, signingCert *x509.Certificate, cert *x509.Certificate, key crypto.Signer, cfg *pkiutil.CertConfig) error {

	// Checks if the signed certificate exists in the PKI directory
	//  检测是否指定证书或私钥至少有一个存在
	if pkiutil.CertOrKeyExist(pkiDir, baseName) {
		// Try to load signed certificate .crt and .key from the PKI directory
		// 加载指定路径下证书和私钥
		signedCert, _, err := pkiutil.TryLoadCertAndKeyFromDisk(pkiDir, baseName)
		if err != nil {
			return errors.Wrapf(err, "failure loading %s certificate", baseName)
		}

		// Check if the existing cert is signed by the given CA
		// 检测signedCert是否由signingCert自签名
		if err := signedCert.CheckSignatureFrom(signingCert); err != nil {
			return errors.Errorf("certificate %s is not signed by corresponding CA", baseName)
		}

		// Check if the certificate has the correct attributes
		// 检测证书配置的IP和域名是否合法
		if err := validateCertificateWithConfig(signedCert, baseName, cfg); err != nil {
			return err
		}

		fmt.Printf("[certs] Using the existing %q certificate and key\n", baseName)
	} else {
		// Write .crt and .key files to disk
		fmt.Printf("[certs] Generating %q certificate and key\n", baseName)
		// 生成证书和私钥文件
		if err := pkiutil.WriteCertAndKey(pkiDir, baseName, cert, key); err != nil {
			return errors.Wrapf(err, "failure while saving %s certificate and key", baseName)
		}
		if pkiutil.HasServerAuth(cert) {
			fmt.Printf("[certs] %s serving cert is signed for DNS names %v and IPs %v\n", baseName, cert.DNSNames, cert.IPAddresses)
		}
	}

	return nil
}

// writeCSRFilesIfNotExist writes a new CSR to the given path.
// If there already is a CSR file at the given path; kubeadm tries to load it and check if it's a valid certificate.
// otherwise this function returns an error.
func writeCSRFilesIfNotExist(csrDir string, baseName string, csr *x509.CertificateRequest, key crypto.Signer) error {
	if pkiutil.CSROrKeyExist(csrDir, baseName) {
		_, _, err := pkiutil.TryLoadCSRAndKeyFromDisk(csrDir, baseName)
		if err != nil {
			return errors.Wrapf(err, "%s CSR existed but it could not be loaded properly", baseName)
		}

		fmt.Printf("[certs] Using the existing %q CSR\n", baseName)
	} else {
		// Write .key and .csr files to disk
		fmt.Printf("[certs] Generating %q key and CSR\n", baseName)

		if err := pkiutil.WriteKey(csrDir, baseName, key); err != nil {
			return errors.Wrapf(err, "failure while saving %s key", baseName)
		}

		if err := pkiutil.WriteCSR(csrDir, baseName, csr); err != nil {
			return errors.Wrapf(err, "failure while saving %s CSR", baseName)
		}
	}

	return nil
}

type certKeyLocation struct {
	pkiDir     string // 证书目录
	caBaseName string // ca名
	baseName   string // 证书基本名称
	uxName     string
}

// SharedCertificateExists verifies if the shared certificates - the certificates that must be
// equal across control-plane nodes: ca.key, ca.crt, sa.key, sa.pub + etcd/ca.key, etcd/ca.crt if local/stacked etcd
// Missing keys are non-fatal and produce warnings.
func SharedCertificateExists(cfg *kubeadmapi.ClusterConfiguration) (bool, error) {

	if err := validateCACertAndKey(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.CACertAndKeyBaseName, "", "CA"}); err != nil {
		return false, err
	}

	if err := validatePrivatePublicKey(certKeyLocation{cfg.CertificatesDir, "", kubeadmconstants.ServiceAccountKeyBaseName, "service account"}); err != nil {
		return false, err
	}

	if err := validateCACertAndKey(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.FrontProxyCACertAndKeyBaseName, "", "front-proxy CA"}); err != nil {
		return false, err
	}

	// in case of local/stacked etcd
	if cfg.Etcd.External == nil {
		if err := validateCACertAndKey(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.EtcdCACertAndKeyBaseName, "", "etcd CA"}); err != nil {
			return false, err
		}
	}

	return true, nil
}

// UsingExternalCA determines whether the user is relying on an external CA.  We currently implicitly determine this is the case
// when the CA Cert is present but the CA Key is not.
// This allows us to, e.g., skip generating certs or not start the csr signing controller.
// In case we are using an external front-proxy CA, the function validates the certificates signed by front-proxy CA that should be provided by the user.
// 判断是否使用了外部ca证书,就需要验证apiserver和apiserver-kubelet-client两个证书签名是否有效。
// 使用外部ca条件： 1. ca.crt存在且为ca，且同时ca.key不存在
func UsingExternalCA(cfg *kubeadmapi.ClusterConfiguration) (bool, error) {
	// 加载指定路径(/etc/kubernetes/pki/ca.crt)下的证书, 确认该证书是否可用的未过期的CA证书
	if err := validateCACert(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.CACertAndKeyBaseName, "", "CA"}); err != nil {
		return false, err
	}

	// 确认私钥文件是否存在(/etc/kubernetes/pki/ca.key). 私钥存在，则直接返回
	caKeyPath := filepath.Join(cfg.CertificatesDir, kubeadmconstants.CAKeyName)
	if _, err := os.Stat(caKeyPath); !os.IsNotExist(err) {
		return false, nil
	}

	// 加载ca证书, 并认证apiserver证书签名是否有效
	if err := validateSignedCert(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.CACertAndKeyBaseName, kubeadmconstants.APIServerCertAndKeyBaseName, "API server"}); err != nil {
		return true, err
	}

	// 加载ca证书, 并认证apiserver证书签名是否有效
	if err := validateSignedCert(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.CACertAndKeyBaseName, kubeadmconstants.APIServerKubeletClientCertAndKeyBaseName, "API server kubelet client"}); err != nil {
		return true, err
	}

	return true, nil
}

// UsingExternalFrontProxyCA determines whether the user is relying on an external front-proxy CA.  We currently implicitly determine this is the case
// when the front proxy CA Cert is present but the front proxy CA Key is not.
// In case we are using an external front-proxy CA, the function validates the certificates signed by front-proxy CA that should be provided by the user.
// 如果/etc/kubernetes/pki/front-proxy-ca.crt存在但front-proxy-ca.key不存在，则认为是使用了外部front-proxy-ca
func UsingExternalFrontProxyCA(cfg *kubeadmapi.ClusterConfiguration) (bool, error) {
	// 加载指定路径(/etc/kubernetes/pki/front-proxy-ca.crt)下的证书, 确认该证书是否可用的未过期的CA证书
	if err := validateCACert(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.FrontProxyCACertAndKeyBaseName, "", "front-proxy CA"}); err != nil {
		return false, err
	}

	// 检测前端代理ca私钥是否存在
	frontProxyCAKeyPath := filepath.Join(cfg.CertificatesDir, kubeadmconstants.FrontProxyCAKeyName)
	if _, err := os.Stat(frontProxyCAKeyPath); !os.IsNotExist(err) {
		return false, nil
	}
	// 校验/etc/kubernetes/pki/front-proxy-client.crt是否由front-proxy-ca.crt签名的
	if err := validateSignedCert(certKeyLocation{cfg.CertificatesDir, kubeadmconstants.FrontProxyCACertAndKeyBaseName, kubeadmconstants.FrontProxyClientCertAndKeyBaseName, "front-proxy client"}); err != nil {
		return true, err
	}

	return true, nil
}

// validateCACert tries to load a x509 certificate from pkiDir and validates that it is a CA
// 加载指定路径(/etc/kubernetes/pki/ca.crt)下的证书, 确认该证书是否可用的未过期的CA证书
func validateCACert(l certKeyLocation) error {
	// Check CA Cert
	// 从指定路径加载证书文件，证书未激活或者已过期即报错。
	caCert, err := pkiutil.TryLoadCertFromDisk(l.pkiDir, l.caBaseName)
	if err != nil {
		return errors.Wrapf(err, "failure loading certificate for %s", l.uxName)
	}

	// Check if cert is a CA
	// 确认该证书是否CA证书
	if !caCert.IsCA {
		return errors.Errorf("certificate %s is not a CA", l.uxName)
	}
	return nil
}

// validateCACertAndKey tries to load a x509 certificate and private key from pkiDir,
// and validates that the cert is a CA. Failure to load the key produces a warning.
func validateCACertAndKey(l certKeyLocation) error {
	if err := validateCACert(l); err != nil {
		return err
	}

	_, err := pkiutil.TryLoadKeyFromDisk(l.pkiDir, l.caBaseName)
	if err != nil {
		klog.Warningf("assuming external key for %s: %v", l.uxName, err)
	}
	return nil
}

// validateSignedCert tries to load a x509 certificate and private key from pkiDir and validates
// that the cert is signed by a given CA
// 加载ca证书, 并认证指定证书签名是否有效
func validateSignedCert(l certKeyLocation) error {
	// Try to load CA
	// 加载ca证书, 确认ca证书有效且没有过期
	caCert, err := pkiutil.TryLoadCertFromDisk(l.pkiDir, l.caBaseName)
	if err != nil {
		return errors.Wrapf(err, "failure loading certificate authority for %s", l.uxName)
	}
	// 通过ca认证指定的证书签名是否有效
	return validateSignedCertWithCA(l, caCert)
}

// validateSignedCertWithCA tries to load a certificate and validate it with the given caCert
// 通过ca认证指定的证书签名是否有效
func validateSignedCertWithCA(l certKeyLocation, caCert *x509.Certificate) error {
	// Try to load key and signed certificate
	// 加载指定路径下证书和私钥
	signedCert, _, err := pkiutil.TryLoadCertAndKeyFromDisk(l.pkiDir, l.baseName)
	if err != nil {
		return errors.Wrapf(err, "failure loading certificate for %s", l.uxName)
	}

	// Check if the cert is signed by the CA
	// 通过ca检验, 确认证书是否有效
	if err := signedCert.CheckSignatureFrom(caCert); err != nil {
		return errors.Wrapf(err, "certificate %s is not signed by corresponding CA", l.uxName)
	}
	return nil
}

// validatePrivatePublicKey tries to load a private key from pkiDir
func validatePrivatePublicKey(l certKeyLocation) error {
	// Try to load key
	_, _, err := pkiutil.TryLoadPrivatePublicKeyFromDisk(l.pkiDir, l.baseName)
	if err != nil {
		return errors.Wrapf(err, "failure loading key for %s", l.uxName)
	}
	return nil
}

// validateCertificateWithConfig makes sure that a given certificate is valid at
// least for the SANs defined in the configuration.
// 检测证书配置的IP和域名是否合法
func validateCertificateWithConfig(cert *x509.Certificate, baseName string, cfg *pkiutil.CertConfig) error {
	for _, dnsName := range cfg.AltNames.DNSNames {
		if err := cert.VerifyHostname(dnsName); err != nil {
			return errors.Wrapf(err, "certificate %s is invalid", baseName)
		}
	}
	for _, ipAddress := range cfg.AltNames.IPs {
		if err := cert.VerifyHostname(ipAddress.String()); err != nil {
			return errors.Wrapf(err, "certificate %s is invalid", baseName)
		}
	}
	return nil
}
