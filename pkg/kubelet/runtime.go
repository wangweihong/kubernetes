/*
Copyright 2015 The Kubernetes Authors.

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

package kubelet

import (
	"errors"
	"fmt"
	"sync"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

//这些运行中的各种错误会反馈到 kubelet对应的Node Ready Condition中。
// 运行环境当前状态
type runtimeState struct {
	sync.RWMutex
	lastBaseRuntimeSync      time.Time     // 容器运行时正常运行时, 才会更新该时间。这里记录是容器运行时上一次正常时间(即runtimeError==nil)
	baseRuntimeSyncThreshold time.Duration // 运行时成功同步时间阈值。即lastBaseRuntimeSync+baseRuntimeSyncThreshold < now,即可认为运行时宕机
	networkError             error         // 用于表示最近一次容器运行时网络是否出错。nil表示网络就绪。如CNI插件没有安装就会报该错误。网络错误不影响运行时错误
	runtimeError             error         // 用于表示最近一次检测容器运行时是否出错。nil表示运行时就绪。出错将认为运行环境有问题
	storageError             error         //最近一次存储错误，nil表示存储就绪
	cidr                     string
	healthChecks             []*healthCheck //当前只有PLEG一个健康检测。如果健康检测失败，运行时环境认为出问题
}

// A health check function should be efficient and not rely on external
// components (e.g., container runtime).
type healthCheckFnType func() (bool, error)

type healthCheck struct {
	name string
	fn   healthCheckFnType
}

//添加健康检测
func (s *runtimeState) addHealthCheck(name string, f healthCheckFnType) {
	s.Lock()
	defer s.Unlock()
	s.healthChecks = append(s.healthChecks, &healthCheck{name: name, fn: f})
}

// 设置最近一次成功同步运行时状态时间
func (s *runtimeState) setRuntimeSync(t time.Time) {
	s.Lock()
	defer s.Unlock()
	s.lastBaseRuntimeSync = t
}

// 设置网络状态
func (s *runtimeState) setNetworkState(err error) {
	s.Lock()
	defer s.Unlock()
	s.networkError = err
}

//设置容器运行时状态
func (s *runtimeState) setRuntimeState(err error) {
	s.Lock()
	defer s.Unlock()
	s.runtimeError = err
}

//设置存储状态
func (s *runtimeState) setStorageState(err error) {
	s.Lock()
	defer s.Unlock()
	s.storageError = err
}

// 设置PodCIDR
func (s *runtimeState) setPodCIDR(cidr string) {
	s.Lock()
	defer s.Unlock()
	s.cidr = cidr
}

//获取PodCIDR
func (s *runtimeState) podCIDR() string {
	s.RLock()
	defer s.RUnlock()
	return s.cidr
}

// 运行时环境是否有问题
// 这里很重要。一旦运行时环境有错误, kubelet的syncLoop循环，就不会进行Pod相关的创建。
func (s *runtimeState) runtimeErrors() error {
	s.RLock()
	defer s.RUnlock()
	errs := []error{}
	// 最近一次运行时成功同步时间为0，意味着kubelet从一开始就没有成功同步container runtime.
	if s.lastBaseRuntimeSync.IsZero() {
		errs = append(errs, errors.New("container runtime status check may not have completed yet"))
		// 最近一次运行时成功同步已经超过了阈值, 认为容器运行时已经宕机
	} else if !s.lastBaseRuntimeSync.Add(s.baseRuntimeSyncThreshold).After(time.Now()) {
		errs = append(errs, errors.New("container runtime is down"))
	}
	//健康检测是否出错
	for _, hc := range s.healthChecks {
		if ok, err := hc.fn(); !ok {
			errs = append(errs, fmt.Errorf("%s is not healthy: %v", hc.name, err))
		}
	}
	//容器运行时是否出错
	if s.runtimeError != nil {
		errs = append(errs, s.runtimeError)
	}

	return utilerrors.NewAggregate(errs)
}

//运行时网络所有错误
func (s *runtimeState) networkErrors() error {
	s.RLock()
	defer s.RUnlock()
	errs := []error{}
	if s.networkError != nil {
		errs = append(errs, s.networkError)
	}
	return utilerrors.NewAggregate(errs)
}

//运行时存储所有错误
func (s *runtimeState) storageErrors() error {
	s.RLock()
	defer s.RUnlock()
	errs := []error{}
	if s.storageError != nil {
		errs = append(errs, s.storageError)
	}
	return utilerrors.NewAggregate(errs)
}

// 运行环境状态
func newRuntimeState(
	runtimeSyncThreshold time.Duration,
) *runtimeState {
	return &runtimeState{
		lastBaseRuntimeSync:      time.Time{},
		baseRuntimeSyncThreshold: runtimeSyncThreshold,
		networkError:             ErrNetworkUnknown,
	}
}
