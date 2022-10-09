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

package phases

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmscheme "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/scheme"
	kubeadmapiv1beta2 "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/v1beta2"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	certsphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/certs"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/pkiutil"
)

var (
	saKeyLongDesc = fmt.Sprintf(cmdutil.LongDesc(`
		Generate the private key for signing service account tokens along with its public key, and save them into
		%s and %s files.
		If both files already exist, kubeadm skips the generation step and existing files will be used.
		`+cmdutil.AlphaDisclaimer), kubeadmconstants.ServiceAccountPrivateKeyName, kubeadmconstants.ServiceAccountPublicKeyName)

	genericLongDesc = cmdutil.LongDesc(`
		Generate the %[1]s, and save them into %[2]s.cert and %[2]s.key files.%[3]s

		If both files already exist, kubeadm skips the generation step and existing files will be used.
		` + cmdutil.AlphaDisclaimer)
)

var (
	csrOnly bool
	csrDir  string
)

// NewCertsPhase returns the phase for the certs
func NewCertsPhase() workflow.Phase {
	return workflow.Phase{
		Name:   "certs",
		Short:  "Certificate generation",
		Phases: newCertSubPhases(),
		Run:    runCerts,
		Long:   cmdutil.MacroCommandLongDescription,
	}
}

func localFlags() *pflag.FlagSet {
	set := pflag.NewFlagSet("csr", pflag.ExitOnError)
	options.AddCSRFlag(set, &csrOnly)
	options.AddCSRDirFlag(set, &csrDir)
	return set
}

// newCertSubPhases returns sub phases for certs phase
// 证书阶段的一系列子阶段
// 1. 生成一系列自签名CA证书(密钥对),以及CA签名的证书密钥. 如果已存在, 则直接使用已存在的 2.生成service account密钥文件sa.key,公钥文件sa.pub
func newCertSubPhases() []workflow.Phase {
	subPhases := []workflow.Phase{}

	// All subphase
	allPhase := workflow.Phase{
		Name:           "all",
		Short:          "Generate all certificates",
		InheritFlags:   getCertPhaseFlags("all"),
		RunAllSiblings: true,
	}

	subPhases = append(subPhases, allPhase)

	// This loop assumes that GetDefaultCertList() always returns a list of
	// certificate that is preceded by the CAs that sign them.
	var lastCACert *certsphase.KubeadmCert
	for _, cert := range certsphase.GetDefaultCertList() {
		var phase workflow.Phase
		// 当前证书为CA证书，执行创建CA证书流程
		if cert.CAName == "" {
			// 生成CA私钥和自签名证书, 如果路径已存在且确定为CA证书，则直接使用
			phase = newCertSubPhase(cert, runCAPhase(cert))
			lastCACert = cert
		} else {
			// 执行创建普通证书流程
			// 创建指定的ca证书caCert签名的证书，如果证书私钥已经存在, 则检验证书是否由caCert签名
			phase = newCertSubPhase(cert, runCertPhase(cert, lastCACert))
			phase.LocalFlags = localFlags()
		}
		subPhases = append(subPhases, phase)
	}

	// SA creates the private/public key pair, which doesn't use x509 at all
	// 如果/etc/kubernetes/pki/sa.key存在,则直接返回。否则创建sa.key, 同时生成公钥文件 sa.pub
	saPhase := workflow.Phase{
		Name:         "sa",
		Short:        "Generate a private key for signing service account tokens along with its public key",
		Long:         saKeyLongDesc,
		Run:          runCertsSa,
		InheritFlags: []string{options.CertificatesDir},
	}

	subPhases = append(subPhases, saPhase)

	return subPhases
}

func newCertSubPhase(certSpec *certsphase.KubeadmCert, run func(c workflow.RunData) error) workflow.Phase {
	phase := workflow.Phase{
		Name:  certSpec.Name,
		Short: fmt.Sprintf("Generate the %s", certSpec.LongName),
		Long: fmt.Sprintf(
			genericLongDesc,
			certSpec.LongName,
			certSpec.BaseName,
			getSANDescription(certSpec), //从证书配置中找到主体别名
		),
		Run:          run,
		InheritFlags: getCertPhaseFlags(certSpec.Name),
	}
	return phase
}

func getCertPhaseFlags(name string) []string {
	flags := []string{
		options.CertificatesDir,
		options.CfgPath,
		options.CSROnly,
		options.CSRDir,
		options.KubernetesVersion,
	}
	if name == "all" || name == "apiserver" {
		flags = append(flags,
			options.APIServerAdvertiseAddress,
			options.ControlPlaneEndpoint,
			options.APIServerCertSANs,
			options.NetworkingDNSDomain,
			options.NetworkingServiceSubnet,
		)
	}
	return flags
}

func getSANDescription(certSpec *certsphase.KubeadmCert) string {
	//Defaulted config we will use to get SAN certs
	defaultConfig := cmdutil.DefaultInitConfiguration()
	// GetAPIServerAltNames errors without an AdvertiseAddress; this is as good as any.
	defaultConfig.LocalAPIEndpoint = kubeadmapiv1beta2.APIEndpoint{
		AdvertiseAddress: "127.0.0.1",
	}

	defaultInternalConfig := &kubeadmapi.InitConfiguration{}

	kubeadmscheme.Scheme.Default(defaultConfig)
	if err := kubeadmscheme.Scheme.Convert(defaultConfig, defaultInternalConfig, nil); err != nil {
		return ""
	}

	certConfig, err := certSpec.GetConfig(defaultInternalConfig)
	if err != nil {
		return ""
	}

	if len(certConfig.AltNames.DNSNames) == 0 && len(certConfig.AltNames.IPs) == 0 {
		return ""
	}
	// This mutates the certConfig, but we're throwing it after we construct the command anyway
	// subject alternative name: 主体别名
	sans := []string{}

	for _, dnsName := range certConfig.AltNames.DNSNames {
		if dnsName != "" {
			sans = append(sans, dnsName)
		}
	}

	for _, ip := range certConfig.AltNames.IPs {
		sans = append(sans, ip.String())
	}
	return fmt.Sprintf("\n\nDefault SANs are %s", strings.Join(sans, ", "))
}

// 如果/etc/kubernetes/pki/sa.key存在,则直接返回。否则创建sa.key, 同时生成公钥文件 sa.pub
func runCertsSa(c workflow.RunData) error {
	data, ok := c.(InitData)
	if !ok {
		return errors.New("certs phase invoked with an invalid data struct")
	}

	// if external CA mode, skip service account key generation
	// 外部CA不执行当前步骤
	if data.ExternalCA() {
		fmt.Printf("[certs] Using existing sa keys\n")
		return nil
	}

	// create the new service account key (or use existing)
	// 如果/etc/kubernetes/pki/sa.key存在,则直接返回。否则创建sa.key, 同时生成公钥文件 sa.pub
	return certsphase.CreateServiceAccountKeyAndPublicKeyFiles(data.CertificateWriteDir(), data.Cfg().ClusterConfiguration.PublicKeyAlgorithm())
}

func runCerts(c workflow.RunData) error {
	data, ok := c.(InitData)
	if !ok {
		return errors.New("certs phase invoked with an invalid data struct")
	}

	fmt.Printf("[certs] Using certificateDir folder %q\n", data.CertificateWriteDir())
	return nil
}

// 生成CA私钥和自签名证书, 如果路径已存在且确定为CA证书，则直接使用
func runCAPhase(ca *certsphase.KubeadmCert) func(c workflow.RunData) error {
	return func(c workflow.RunData) error {
		data, ok := c.(InitData)
		if !ok {
			return errors.New("certs phase invoked with an invalid data struct")
		}

		// if using external etcd, skips etcd certificate authority generation
		// 外置etcd则忽略etcd-ca的创建
		if data.Cfg().Etcd.External != nil && ca.Name == "etcd-ca" {
			fmt.Printf("[certs] External etcd mode: Skipping %s certificate authority generation\n", ca.BaseName)
			return nil
		}

		// 如果ca证书已经存在，则ca证书生成
		if _, err := pkiutil.TryLoadCertFromDisk(data.CertificateDir(), ca.BaseName); err == nil {
			if _, err := pkiutil.TryLoadKeyFromDisk(data.CertificateDir(), ca.BaseName); err == nil {
				fmt.Printf("[certs] Using existing %s certificate authority\n", ca.BaseName)
				return nil
			}
			fmt.Printf("[certs] Using existing %s keyless certificate authority\n", ca.BaseName)
			return nil
		}

		// if dryrunning, write certificates authority to a temporary folder (and defer restore to the path originally specified by the user)
		cfg := data.Cfg()
		cfg.CertificatesDir = data.CertificateWriteDir()
		defer func() { cfg.CertificatesDir = data.CertificateDir() }()

		// create the new certificate authority (or use existing)
		// 创建私钥和自签名CA
		return certsphase.CreateCACertAndKeyFiles(ca, cfg)
	}
}

// 创建指定的ca证书caCert签名的证书，如果证书私钥已经存在, 则检验证书是否由caCert签名
func runCertPhase(cert *certsphase.KubeadmCert, caCert *certsphase.KubeadmCert) func(c workflow.RunData) error {
	return func(c workflow.RunData) error {
		data, ok := c.(InitData)
		if !ok {
			return errors.New("certs phase invoked with an invalid data struct")
		}

		// if using external etcd, skips etcd certificates generation
		if data.Cfg().Etcd.External != nil && cert.CAName == "etcd-ca" {
			fmt.Printf("[certs] External etcd mode: Skipping %s certificate generation\n", cert.BaseName)
			return nil
		}

		//  如果证书已经存在，则加载CA证书校验签名
		if certData, _, err := pkiutil.TryLoadCertAndKeyFromDisk(data.CertificateDir(), cert.BaseName); err == nil {
			caCertData, err := pkiutil.TryLoadCertFromDisk(data.CertificateDir(), caCert.BaseName)
			if err != nil {
				return errors.Wrapf(err, "couldn't load CA certificate %s", caCert.Name)
			}

			//  校验证书是否由指定ca来签名。
			if err := certData.CheckSignatureFrom(caCertData); err != nil {
				return errors.Wrapf(err, "[certs] certificate %s not signed by CA certificate %s", cert.BaseName, caCert.BaseName)
			}

			fmt.Printf("[certs] Using existing %s certificate and key on disk\n", cert.BaseName)
			return nil
		}
		// 只生成证书签名
		if csrOnly {
			fmt.Printf("[certs] Generating CSR for %s instead of certificate\n", cert.BaseName)
			if csrDir == "" {
				csrDir = data.CertificateWriteDir()
			}

			return certsphase.CreateCSR(cert, data.Cfg(), csrDir)
		}

		// if dryrunning, write certificates to a temporary folder (and defer restore to the path originally specified by the user)
		cfg := data.Cfg()
		cfg.CertificatesDir = data.CertificateWriteDir()
		defer func() { cfg.CertificatesDir = data.CertificateDir() }()

		// create the new certificate (or use existing)
		// 创建指定的ca证书caCert签名的证书，如果证书私钥已经存在, 则检验证书是否由caCert签名
		return certsphase.CreateCertAndKeyFilesWithCA(cert, caCert, cfg)
	}
}
