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

package scheduler

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"k8s.io/klog"
)

// WorkArgs keeps arguments that will be passed to the function executed by the worker.
type WorkArgs struct {
	NamespacedName types.NamespacedName
}

// KeyFromWorkArgs creates a key for the given `WorkArgs`
func (w *WorkArgs) KeyFromWorkArgs() string {
	return w.NamespacedName.String()
}

// NewWorkArgs is a helper function to create new `WorkArgs`
func NewWorkArgs(name, namespace string) *WorkArgs {
	return &WorkArgs{types.NamespacedName{Namespace: namespace, Name: name}}
}

// TimedWorker is a responsible for executing a function no earlier than at FireAt time.
// 定时工作器. TimedWorker创建时会启动一个异步线程在FireAt执行某项任务. 使用Timer.Stop可以停止该定时任务
type TimedWorker struct {
	WorkItem  *WorkArgs   //对应的资源
	CreatedAt time.Time   //创建时间
	FireAt    time.Time   //执行时间
	Timer     *time.Timer //TimedWorker创建时会启动一个异步线程在FireAt执行某项任务. 使用这个Timer.Stop可以停止该定时任务
}

// CreateWorker creates a TimedWorker that will execute `f` not earlier than `fireAt`.
// 如果fireAt与creatAt之间没有延时，立即对args执行f操作后返回，不需要创建定时工作器
// 否则创建一个定时工作器，并开始倒计时。倒计时结束执行f
func CreateWorker(args *WorkArgs, createdAt time.Time, fireAt time.Time, f func(args *WorkArgs) error) *TimedWorker {
	delay := fireAt.Sub(createdAt)
	//没有延时
	if delay <= 0 {
		//启动协程处理指定的资源后，返空
		go f(args)
		return nil
	}
	//在delay后执行f函数,返回的timer用于停止这个定时器。不能用于timer.C
	//执行time.AfterFunc后就开始倒计时
	timer := time.AfterFunc(delay, func() { f(args) })
	return &TimedWorker{
		WorkItem:  args,
		CreatedAt: createdAt,
		FireAt:    fireAt,
		Timer:     timer,
	}
}

// Cancel cancels the execution of function by the `TimedWorker`
// 取消定时任务， 这回
func (w *TimedWorker) Cancel() {
	if w != nil {
		w.Timer.Stop()
	}
}

// TimedWorkerQueue keeps a set of TimedWorkers that are still wait for execution.
// 用来保存等待特定时间执行的工作者表
type TimedWorkerQueue struct {
	sync.Mutex
	// map of workers keyed by string returned by 'KeyFromWorkArgs' from the given worker.
	workers  map[string]*TimedWorker    //定时工作器表， key是namespace/name。 注意Value可以为nil.因为不需要定时，所以已经执行
	workFunc func(args *WorkArgs) error //工作函数
}

// CreateWorkerQueue creates a new TimedWorkerQueue for workers that will execute
// given function `f`.
// 创建工作队列并指定处理函数
func CreateWorkerQueue(f func(args *WorkArgs) error) *TimedWorkerQueue {
	return &TimedWorkerQueue{
		workers:  make(map[string]*TimedWorker),
		workFunc: f,
	}
}

func (q *TimedWorkerQueue) getWrappedWorkerFunc(key string) func(args *WorkArgs) error {
	return func(args *WorkArgs) error {
		err := q.workFunc(args)
		q.Lock()
		defer q.Unlock()
		if err == nil {
			// To avoid duplicated calls we keep the key in the queue, to prevent
			// subsequent additions.
			q.workers[key] = nil
		} else {
			delete(q.workers, key)
		}
		return err
	}
}

// AddWork adds a work to the WorkerQueue which will be executed not earlier than `fireAt`.
// 当createAt与fireAt同一时间时，不启动定时处理，而是直接处理（也是异步)
func (q *TimedWorkerQueue) AddWork(args *WorkArgs, createdAt time.Time, fireAt time.Time) {
	key := args.KeyFromWorkArgs() //获取工作对象
	klog.V(4).Infof("Adding TimedWorkerQueue item %v at %v to be fired at %v", key, createdAt, fireAt)

	q.Lock()
	defer q.Unlock()
	if _, exists := q.workers[key]; exists {
		klog.Warningf("Trying to add already existing work for %+v. Skipping.", args)
		return
	}
	// 如果fireAt与creatAt之间没有延时，立即对args异步执行f操作后返回，不需要创建定时工作器
	// 否则创建一个定时工作器，并开始倒计时
	worker := CreateWorker(args, createdAt, fireAt, q.getWrappedWorkerFunc(key))
	q.workers[key] = worker
}

// CancelWork removes scheduled function execution from the queue. Returns true if work was cancelled.
//取消指定定时工作的执行，并从队列中移除
func (q *TimedWorkerQueue) CancelWork(key string) bool {
	q.Lock()
	defer q.Unlock()
	worker, found := q.workers[key]
	result := false
	if found {
		klog.V(4).Infof("Cancelling TimedWorkerQueue item %v at %v", key, time.Now())
		if worker != nil {
			result = true
			//取消指定worker的定时删除动作
			worker.Cancel()
		}
		delete(q.workers, key)
	}
	return result
}

// GetWorkerUnsafe returns a TimedWorker corresponding to the given key.
// Unsafe method - workers have attached goroutines which can fire after this function is called.
func (q *TimedWorkerQueue) GetWorkerUnsafe(key string) *TimedWorker {
	q.Lock()
	defer q.Unlock()
	return q.workers[key]
}
