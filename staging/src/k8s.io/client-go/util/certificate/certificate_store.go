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

package certificate

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog"
)

const (
	keyExtension  = ".key"    // 私钥后缀
	certExtension = ".crt"    // 证书后缀
	pemExtension  = ".pem"    // 证书PEM编码后缀
	currentPair   = "current" // 当前证书私钥对前缀
	updatedPair   = "updated" // 已更新证书私钥对前缀
)

// 存储证书/私钥文件路径
type fileStore struct {
	pairNamePrefix string // kubelet-client 或者 kubelet-server
	certDirectory  string // 证书存放目录, 当前默认是/var/lib/kubelet/pki
	keyDirectory   string // 私钥存放目录, 当前默认是/var/lib/kubelet/pki
	certFile       string // 证书文件名, 可能为空。如果为空，则会从certDirectory找到<pairNamePrefix>.crt文件作为证书
	keyFile        string // 私钥文件名, 可能为空。如果为空, 则会从keyDirecctory找到<pairnamePrefix>.key文件作为私钥
}

// FileStore is a store that provides certificate retrieval as well as
// the path on disk of the current PEM.
// 提供证书、私钥对在文件系统中的路径信息
type FileStore interface {
	Store
	// CurrentPath returns the path on disk of the current certificate/key
	// pair encoded as PEM files.
	CurrentPath() string
}

// NewFileStore returns a concrete implementation of a Store that is based on
// storing the cert/key pairs in a single file per pair on disk in the
// designated directory. When starting up it will look for the currently
// selected cert/key pair in:
//
// 1. ${certDirectory}/${pairNamePrefix}-current.pem - both cert and key are in the same file.
// 2. ${certFile}, ${keyFile}
// 3. ${certDirectory}/${pairNamePrefix}.crt, ${keyDirectory}/${pairNamePrefix}.key
//
// The first one found will be used. If rotation is enabled, future cert/key
// updates will be written to the ${certDirectory} directory and
// ${certDirectory}/${pairNamePrefix}-current.pem will be created as a soft
// link to the currently selected cert/key pair.
// 创建证书文件对
func NewFileStore(
	pairNamePrefix string,
	certDirectory string,
	keyDirectory string,
	certFile string,
	keyFile string) (FileStore, error) {

	s := fileStore{
		pairNamePrefix: pairNamePrefix, // kubelet-client, kubelet-server
		certDirectory:  certDirectory,  //  /var/lib/kubelet/pki
		keyDirectory:   keyDirectory,   //  /var/lib/kubelet/pki
		certFile:       certFile,
		keyFile:        keyFile,
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return &s, nil
}

// CurrentPath returns the path to the current version of these certificates.
// /var/lib/kubelet/pki/current
func (s *fileStore) CurrentPath() string {
	return filepath.Join(s.certDirectory, s.filename(currentPair))
}

// recover checks if there is a certificate rotation that was interrupted while
// progress, and if so, attempts to recover to a good state.
// 如果/var/lib/kubelet/pki目录下current文件不存在, 但updated文件存在且为符号链接文件,则重命名updated文件为current
func (s *fileStore) recover() error {
	// If the 'current' file doesn't exist, continue on with the recovery process.
	// /var/lib/kubelet/pki/current
	currentPath := filepath.Join(s.certDirectory, s.filename(currentPair))
	// 获取current文件信息
	if exists, err := fileExists(currentPath); err != nil {
		return err
		// 如果文件存在
	} else if exists {
		return nil
	}

	// If the 'updated' file exists, and it is a symbolic link, continue on
	// with the recovery process.
	// 如果/var/lib/kubelet/pki/updated文件存在,且为符号链接文件
	updatedPath := filepath.Join(s.certDirectory, s.filename(updatedPair))
	if fi, err := os.Lstat(updatedPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	} else if fi.Mode()&os.ModeSymlink != os.ModeSymlink {
		return fmt.Errorf("expected %q to be a symlink but it is a file", updatedPath)
	}

	// Move the 'updated' symlink to 'current'.
	if err := os.Rename(updatedPath, currentPath); err != nil {
		return fmt.Errorf("unable to rename %q to %q: %v", updatedPath, currentPath, err)
	}
	return nil
}

//依次从1.指定路径/var/lib/kubelet/pki/current（同时包含证书和私钥)
// 2. 指定证书路径,私钥路径，
// 3. 证书目录/var/lib/kubelet/pki/ 下 kubelet-<client|server>.key,kubelet-<client|server>.crt
//读取X509证书私钥对
func (s *fileStore) Current() (*tls.Certificate, error) {
	// /var/lib/kubelet/pki目录下文件kubelet-client-current.pem或者kubelet-server-current.pem
	pairFile := filepath.Join(s.certDirectory, s.filename(currentPair))
	// 检测正好是私钥/证书对是否存在
	if pairFileExists, err := fileExists(pairFile); err != nil {
		return nil, err
		//证书/私钥对存在
	} else if pairFileExists {
		klog.Infof("Loading cert/key pair from %q.", pairFile)
		return loadFile(pairFile)
	}

	// 确认证书文件是否存在, certFile为空也报不存在
	certFileExists, err := fileExists(s.certFile)
	if err != nil {
		return nil, err
	}

	// 确认私钥文件是否存在，keyFile为空也报不存在
	keyFileExists, err := fileExists(s.keyFile)
	if err != nil {
		return nil, err
	}

	// 证书和私钥都存在就开始加载X509证书、密钥对
	if certFileExists && keyFileExists {
		klog.Infof("Loading cert/key pair from (%q, %q).", s.certFile, s.keyFile)
		return loadX509KeyPair(s.certFile, s.keyFile)
	}
	// kubelet-client.crt | kubelet-server.crt
	c := filepath.Join(s.certDirectory, s.pairNamePrefix+certExtension)
	// kubelet-client.key | kubelet-server.key
	k := filepath.Join(s.keyDirectory, s.pairNamePrefix+keyExtension)
	certFileExists, err = fileExists(c)
	if err != nil {
		return nil, err
	}
	keyFileExists, err = fileExists(k)
	if err != nil {
		return nil, err
	}
	if certFileExists && keyFileExists {
		klog.Infof("Loading cert/key pair from (%q, %q).", c, k)
		return loadX509KeyPair(c, k)
	}

	noKeyErr := NoCertKeyError(
		fmt.Sprintf("no cert/key files read at %q, (%q, %q) or (%q, %q)",
			pairFile,
			s.certFile,
			s.keyFile,
			s.certDirectory,
			s.keyDirectory))
	return nil, &noKeyErr
}

//加载证书私钥对文件(同时包含私钥和密钥),并解析成tls证书对象
func loadFile(pairFile string) (*tls.Certificate, error) {
	// LoadX509KeyPair knows how to parse combined cert and private key from
	// the same file.
	// 读取证书和私钥文件,并解析
	cert, err := tls.LoadX509KeyPair(pairFile, pairFile)
	if err != nil {
		return nil, fmt.Errorf("could not convert data from %q into cert/key pair: %v", pairFile, err)
	}
	certs, err := x509.ParseCertificates(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("unable to parse certificate data: %v", err)
	}
	cert.Leaf = certs[0]
	return &cert, nil
}

// 创建/var/lib/kubelet/pki/kubelet-<client|server>-<时间戳>.pem文件，如果存在则清除原内容,写入新的证书和私钥内容。并且更新current符号链接
func (s *fileStore) Update(certData, keyData []byte) (*tls.Certificate, error) {
	ts := time.Now().Format("2006-01-02-15-04-05")
	//以当前时间命名的证书文件
	pemFilename := s.filename(ts)

	//创建证书目录
	if err := os.MkdirAll(s.certDirectory, 0755); err != nil {
		return nil, fmt.Errorf("could not create directory %q to store certificates: %v", s.certDirectory, err)
	}
	// kubelet-client-<2022050511234>.pem 或者 kubelet-server-<2022050511234>.pem
	certPath := filepath.Join(s.certDirectory, pemFilename)

	// 创建证书文件，存在则清空
	f, err := os.OpenFile(certPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("could not open %q: %v", certPath, err)
	}
	defer f.Close()

	// First cert is leaf, remainder are intermediates
	// 将PEM格式证书解析成x509证书对象
	certs, err := certutil.ParseCertsPEM(certData)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate data: %v", err)
	}

	// 证书内容写到到指定路径
	for _, c := range certs {
		pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}

	// 确认私钥数据格式正确
	keyBlock, _ := pem.Decode(keyData)
	if keyBlock == nil {
		return nil, fmt.Errorf("invalid key data")
	}
	// 私钥内容写到指定路径
	pem.Encode(f, keyBlock)

	//加载证书私钥对文件(同时包含私钥和密钥),并解析成tls证书对象
	cert, err := loadFile(certPath)
	if err != nil {
		return nil, err
	}
	// 如果证书路径下current符号链接存在, 则删除创建新的current文件指向证书私钥对文件
	if err := s.updateSymlink(certPath); err != nil {
		return nil, err
	}
	return cert, nil
}

// updateSymLink updates the current symlink to point to the file that is
// passed it. It will fail if there is a non-symlink file exists where the
// symlink is expected to be.
// filename:证书私钥对文件, kubelet-client-<2022050511234>.pem 或者 kubelet-server-<2022050511234>.pem
// 如果证书路径下current符号链接存在, 则删除创建新的current文件指向证书私钥对文件
func (s *fileStore) updateSymlink(filename string) error {
	// If the 'current' file either doesn't exist, or is already a symlink,
	// proceed. Otherwise, this is an unrecoverable error.
	// 确认证书目录/var/lib/kubelet/pki下current文件是否存在，是否为符号链接文件
	currentPath := filepath.Join(s.certDirectory, s.filename(currentPair))
	currentPathExists := false
	if fi, err := os.Lstat(currentPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if fi.Mode()&os.ModeSymlink != os.ModeSymlink {
		return fmt.Errorf("expected %q to be a symlink but it is a file", currentPath)
	} else {
		// 标记current文件存在
		currentPathExists = true
	}

	// If the 'updated' file doesn't exist, proceed. If it exists but it is a
	// symlink, delete it.  Otherwise, this is an unrecoverable error.
	// 确认证书目录/var/lib/kubelet/pki下updated文件是否存在，是否为符号链接文件
	updatedPath := filepath.Join(s.certDirectory, s.filename(updatedPair))
	if fi, err := os.Lstat(updatedPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if fi.Mode()&os.ModeSymlink != os.ModeSymlink {
		return fmt.Errorf("expected %q to be a symlink but it is a file", updatedPath)
	} else {
		//删除updated文件
		if err := os.Remove(updatedPath); err != nil {
			return fmt.Errorf("unable to remove %q: %v", updatedPath, err)
		}
	}

	// Check that the new cert/key pair file exists to avoid rotating to an
	// invalid cert/key.
	// 确认下 存放证书/私钥对的文件是否存在
	if filenameExists, err := fileExists(filename); err != nil {
		return err
	} else if !filenameExists {
		return fmt.Errorf("file %q does not exist so it can not be used as the currently selected cert/key", filename)
	}

	// Ensure the source path is absolute to ensure the symlink target is
	// correct when certDirectory is a relative path.
	// 获取绝对路径
	filename, err := filepath.Abs(filename)
	if err != nil {
		return err
	}

	// Create the 'updated' symlink pointing to the requested file name.
	// 创建符号链接为updated,连接到证书/私钥对文件
	if err := os.Symlink(filename, updatedPath); err != nil {
		return fmt.Errorf("unable to create a symlink from %q to %q: %v", updatedPath, filename, err)
	}

	// Replace the 'current' symlink.
	// 如果`current`符号链接文件存在, 则删除
	if currentPathExists {
		if err := os.Remove(currentPath); err != nil {
			return fmt.Errorf("unable to remove %q: %v", currentPath, err)
		}
	}
	//重命名updated文件为current
	if err := os.Rename(updatedPath, currentPath); err != nil {
		return fmt.Errorf("unable to rename %q to %q: %v", updatedPath, currentPath, err)
	}
	return nil
}

// kubelet-client-<current>.pem
// kubelet-server-<current>.pem
// kubelet-client-<2022050511234>.pem
func (s *fileStore) filename(qualifier string) string {
	return s.pairNamePrefix + "-" + qualifier + pemExtension
}

//加载证书、私钥文件，解析成X509证书对象
func loadX509KeyPair(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	certs, err := x509.ParseCertificates(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("unable to parse certificate data: %v", err)
	}
	cert.Leaf = certs[0]
	return &cert, nil
}

// FileExists checks if specified file exists.
// 检测文件是否存在,注意如果文件名为空，报错错误也是os.ErrNotExist错误
func fileExists(filename string) (bool, error) {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}
