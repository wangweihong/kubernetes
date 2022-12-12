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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	api "k8s.io/kubernetes/pkg/apis/core"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	utilio "k8s.io/utils/io"
)

type podEventType int

const (
	podAdd podEventType = iota
	podModify
	podDelete

	eventBufferLen = 10
)

type watchEvent struct {
	fileName  string
	eventType podEventType
}

type sourceFile struct {
	path           string             // 监听的路径。可以是一个目录，也可以是文件。如果是目录，只处理目录子文件，不做子目录递归
	nodeName       types.NodeName     // 节点名，用于设置创建的Pod的信息
	period         time.Duration      // 默认间隔20秒
	store          cache.Store        // 缓存，记录从文件解析出来的Pod对象。当缓存中对象发生变更时，会主动发送缓存新Pod列表给updates通道
	fileKeyMapping map[string]string  // 记录文件名和对应的pod对象的关系。key为文件名,value为Pod的`命名空间/名字`
	updates        chan<- interface{} // 1. 当缓存中的Pod发生变化时, 通过该通道推送最新的pod列表。2. 当文件系统监听出现错误，也通过其发送
	watchEvents    chan *watchEvent   // 用于接收inotify
}

// NewSourceFile watches a config file for changes.
// 启动静态pod文件监听器
func NewSourceFile(path string, nodeName types.NodeName, period time.Duration, updates chan<- interface{}) {
	// "github.com/sigma/go-inotify" requires a path without trailing "/"
	path = strings.TrimRight(path, string(os.PathSeparator))

	config := newSourceFile(path, nodeName, period, updates)
	klog.V(1).Infof("Watching path %q", path)
	config.run()
}

func newSourceFile(path string, nodeName types.NodeName, period time.Duration, updates chan<- interface{}) *sourceFile {
	//发送pod对象到updates
	send := func(objs []interface{}) {
		var pods []*v1.Pod
		for _, o := range objs {
			pods = append(pods, o.(*v1.Pod))
		}
		updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.FileSource}
	}
	// 创建一个pod缓存。该缓存会在缓存pod列表发生增删改时，将缓存中pod列表发给updates
	store := cache.NewUndeltaStore(send, cache.MetaNamespaceKeyFunc)
	return &sourceFile{
		path:           path,
		nodeName:       nodeName,
		period:         period,
		store:          store,
		fileKeyMapping: map[string]string{},
		updates:        updates,
		watchEvents:    make(chan *watchEvent, eventBufferLen),
	}
}

// 启动pod文件来源管理器
// 1. 每间隔20秒，主动解析静态文件目录下的文件，更新本地缓存并发送kubetypes.SET类型事件以及最新的Pod列表到updates chan
// 2. 当接收到inotify文件系统通知文件的增删改,
func (s *sourceFile) run() {
	listTicker := time.NewTicker(s.period)

	go func() {
		// Read path immediately to speed up startup.
		// 解析监听路径下的文件(或列表）成Pod对象,替换掉本地缓存的Pod列表并且通知update最新的Pod列表
		if err := s.listConfig(); err != nil {
			klog.Errorf("Unable to read config path %q: %v", s.path, err)
		}
		for {
			select {
			case <-listTicker.C:
				//解析监听路径下的文件(或列表）成Pod对象,替换掉本地缓存的Pod列表并且通知update最新的Pod列表
				if err := s.listConfig(); err != nil {
					klog.Errorf("Unable to read config path %q: %v", s.path, err)
				}
				//收到外部事件时
			case e := <-s.watchEvents:
				if err := s.consumeWatchEvent(e); err != nil {
					klog.Errorf("Unable to process watch event: %v", err)
				}
			}
		}
	}()
	//
	s.startWatch()
}

//填充pod的默认信息
func (s *sourceFile) applyDefaults(pod *api.Pod, source string) error {
	return applyDefaults(pod, source, true, s.nodeName)
}

// 解析监听路径下的文件(或列表）成Pod对象,替换掉本地缓存的Pod列表并且通知update最新的Pod列表
func (s *sourceFile) listConfig() error {
	path := s.path
	//确认监听路径是否存在
	statInfo, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// Emit an update with an empty PodList to allow FileSource to be marked as seen
		s.updates <- kubetypes.PodUpdate{Pods: []*v1.Pod{}, Op: kubetypes.SET, Source: kubetypes.FileSource}
		return fmt.Errorf("path does not exist, ignoring")
	}

	switch {
	//确认监听路径是否为目录,如果是目录，则遍历解析
	case statInfo.Mode().IsDir():
		// 遍历目录中的子文件，依次尝试解析成Pod对象，忽略子目录
		pods, err := s.extractFromDir(path)
		if err != nil {
			return err
		}

		//解析不到Pod就不替换本地缓存?
		if len(pods) == 0 {
			// Emit an update with an empty PodList to allow FileSource to be marked as seen
			s.updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.FileSource}
			return nil
		}
		// 替换缓存中的Pod对象，并发送替换后的Pod列表到update chan
		return s.replaceStore(pods...)

	case statInfo.Mode().IsRegular():
		pod, err := s.extractFromFile(path)
		if err != nil {
			return err
		}
		// 替换缓存中的Pod对象，并发送替换后的Pod列表到update chan
		return s.replaceStore(pod)

	default:
		return fmt.Errorf("path is not a directory or file")
	}
}

// Get as many pod manifests as we can from a directory. Return an error if and only if something
// prevented us from reading anything at all. Do not return an error if only some files
// were problematic.
// 遍历目录中的子文件，依次尝试解析成Pod对象，忽略子目录(并没有针对后缀,所有文本文件均解析)
func (s *sourceFile) extractFromDir(name string) ([]*v1.Pod, error) {
	dirents, err := filepath.Glob(filepath.Join(name, "[^.]*"))
	if err != nil {
		return nil, fmt.Errorf("glob failed: %v", err)
	}

	pods := make([]*v1.Pod, 0)
	if len(dirents) == 0 {
		return pods, nil
	}
	//按名字排序
	sort.Strings(dirents)
	for _, path := range dirents {
		statInfo, err := os.Stat(path)
		if err != nil {
			klog.Errorf("Can't get metadata for %q: %v", path, err)
			continue
		}

		switch {
		//不再递归子目录
		case statInfo.Mode().IsDir():
			klog.Errorf("Not recursing into manifest path %q", path)
			//普通文件
		case statInfo.Mode().IsRegular():
			pod, err := s.extractFromFile(path)
			if err != nil {
				if !os.IsNotExist(err) {
					klog.Errorf("Can't process manifest file %q: %v", path, err)
				}
			} else {
				pods = append(pods, pod)
			}
		default:
			klog.Errorf("Manifest path %q is not a directory or file: %v", path, statInfo.Mode())
		}
	}
	return pods, nil
}

// extractFromFile parses a file for Pod configuration information.
// 从指定文件中解析出pod的信息
func (s *sourceFile) extractFromFile(filename string) (pod *v1.Pod, err error) {
	klog.V(3).Infof("Reading config file %q", filename)
	defer func() {
		if err == nil && pod != nil {
			//获取pod的`命名空间/Pod名`
			objKey, keyErr := cache.MetaNamespaceKeyFunc(pod)
			if keyErr != nil {
				err = keyErr
				return
			}
			//记录文件名和指定Pod之间的关系
			s.fileKeyMapping[filename] = objKey
		}
	}()

	file, err := os.Open(filename)
	if err != nil {
		return pod, err
	}
	defer file.Close()

	// 读取的文件最大为10MB,超过就报错
	data, err := utilio.ReadAtMost(file, maxConfigLength)
	if err != nil {
		return pod, err
	}

	//用于填充pod的默认值
	defaultFn := func(pod *api.Pod) error {
		return s.applyDefaults(pod, filename)
	}

	//解析文件，提取其中的Pod
	parsed, pod, podErr := tryDecodeSinglePod(data, defaultFn)
	if parsed {
		if podErr != nil {
			return pod, podErr
		}
		return pod, nil
	}

	return pod, fmt.Errorf("%v: couldn't parse as pod(%v), please check config file", filename, podErr)
}

func (s *sourceFile) replaceStore(pods ...*v1.Pod) (err error) {
	objs := []interface{}{}
	for _, pod := range pods {
		objs = append(objs, pod)
	}
	return s.store.Replace(objs, "")
}
