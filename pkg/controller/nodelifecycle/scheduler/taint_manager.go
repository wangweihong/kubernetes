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

package scheduler

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/apis/core/helper"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	"k8s.io/klog"
)

const (
	// TODO (k82cn): Figure out a reasonable number of workers/channels and propagate
	// the number of workers up making it a parameter of Run() function.

	// NodeUpdateChannelSize defines the size of channel for node update events.
	NodeUpdateChannelSize = 10
	// UpdateWorkerSize defines the size of workers for node update or/and pod update.
	UpdateWorkerSize     = 8 //节点更新队列长度
	podUpdateChannelSize = 1
	retries              = 5
)

type nodeUpdateItem struct {
	nodeName string
}

type podUpdateItem struct {
	podName      string
	podNamespace string
	nodeName     string
}

// node列表有8个worker, hash计算出某个节点应该用哪个worker来处理
func hash(val string, max int) int {
	hasher := fnv.New32a()
	io.WriteString(hasher, val)
	return int(hasher.Sum32() % uint32(max))
}

// GetPodFunc returns the pod for the specified name/namespace, or a NotFound error if missing.
//
type GetPodFunc func(name, namespace string) (*v1.Pod, error)

// GetNodeFunc returns the node for the specified name, or a NotFound error if missing.
type GetNodeFunc func(name string) (*v1.Node, error)

// GetPodsByNodeNameFunc returns the list of pods assigned to the specified node.
type GetPodsByNodeNameFunc func(nodeName string) ([]*v1.Pod, error)

// NoExecuteTaintManager listens to Taint/Toleration changes and is responsible for removing Pods
// from Nodes tainted with NoExecute Taints.
// 当Pod/Node更新时，确认pod/node上所有Pod是否容忍node的污点。 1. 如果不能完全容忍，则立即删除pod。2. 如果能完全容忍且没有设置容忍时间，什么都不做
// 3. 如果能完全容忍单设置了容忍时间，启动定时删除（最小容忍时间）
type NoExecuteTaintManager struct {
	client                clientset.Interface
	recorder              record.EventRecorder
	getPod                GetPodFunc            //获取Pod信息的接口
	getNode               GetNodeFunc           //获取节点信息的接口
	getPodsAssignedToNode GetPodsByNodeNameFunc //用于获取某个节点上pod列表

	taintEvictionQueue *TimedWorkerQueue //污点定时删除Pod队列。加入队列的对象都会异步启动一个定时器倒计时(最小容忍时间）执行处理函数，如果定时为0或者没有设置最小容忍时间，则立即启动协程异步执行
	// 处理函数：告知Apiserver某个Pod即将删除,然后尝试最多5次去删除该Pod
	// 1.  当节点不存在Effect=NotExecute的污点时，将Pod从taintEvictionQueue移除，并取消定时删除任务
	// 2.  当Pod不能容忍节点所有Effect=NotExecute的污点时，将Pod先从taintEvictionQueue移除，并取消定时删除任务（如果有）。再利用taintEvictionQueue启动协程直接删除Pod
	// 3.  当Pod容忍节点所有Effect=NotExecute的污点时， 如果没有设置容忍时间。将Pod先从taintEvictionQueue移除，并取消定时删除任务（如果有）
	// 4.  当Pod容忍节点所有Effect=NotExecute的污点时， 如果设置容忍时间。算出最小容忍时间，移除原来的定时任务，采用新的定时删除任务（定时为最小容忍时间）
	// keeps a map from nodeName to all noExecute taints on that Node
	taintedNodesLock sync.Mutex
	taintedNodes     map[string][]v1.Taint //用来记录节点所有Effect=NoExecute的Taint. key是节点名

	nodeUpdateChannels []chan nodeUpdateItem // 长度为8.将会启动8个线程，分别处理对应的nodeUpdateChannels传递的数据.chan缓存长度为10
	podUpdateChannels  []chan podUpdateItem  // 长度为8，将会启动8个线程，分别处理对应podUpdateChannels传递的数据。chan缓存长度为1

	nodeUpdateQueue workqueue.Interface //当节点发生创建/更新/删除时，添加到该队列中。
	podUpdateQueue  workqueue.Interface //当Pod发生创建/更新/删除时，添加到该队列中
}

//返回函数：如果emitEventFunc不为空, 该函数处理对应的Pod,在尝试删除指定的Pod，最多尝试5次。
func deletePodHandler(c clientset.Interface, emitEventFunc func(types.NamespacedName)) func(args *WorkArgs) error {
	return func(args *WorkArgs) error {
		ns := args.NamespacedName.Namespace
		name := args.NamespacedName.Name
		klog.V(0).Infof("NoExecuteTaintManager is deleting Pod: %v", args.NamespacedName.String())
		if emitEventFunc != nil {
			emitEventFunc(args.NamespacedName)
		}
		var err error
		//尝试5次去删除该pod
		for i := 0; i < retries; i++ {
			err = c.CoreV1().Pods(ns).Delete(context.TODO(), name, metav1.DeleteOptions{})
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return err
	}
}

//提取Taint表中所有Effect=NoExecute
func getNoExecuteTaints(taints []v1.Taint) []v1.Taint {
	result := []v1.Taint{}
	for i := range taints {
		if taints[i].Effect == v1.TaintEffectNoExecute {
			result = append(result, taints[i])
		}
	}
	return result
}

// getMinTolerationTime returns minimal toleration time from the given slice, or -1 if it's infinite.
// 获取Toleration表中所有Toleration中最短容忍时间。 小于0，表示永远容忍
func getMinTolerationTime(tolerations []v1.Toleration) time.Duration {
	minTolerationTime := int64(math.MaxInt64)
	if len(tolerations) == 0 {
		return 0
	}

	for i := range tolerations {
		if tolerations[i].TolerationSeconds != nil {
			tolerationSeconds := *(tolerations[i].TolerationSeconds)
			if tolerationSeconds <= 0 {
				return 0
			} else if tolerationSeconds < minTolerationTime {
				minTolerationTime = tolerationSeconds
			}
		}
	}
	//时间无效
	if minTolerationTime == int64(math.MaxInt64) {
		return -1
	}
	return time.Duration(minTolerationTime) * time.Second
}

// NewNoExecuteTaintManager creates a new NoExecuteTaintManager that will use passed clientset to
// communicate with the API server.
func NewNoExecuteTaintManager(c clientset.Interface, getPod GetPodFunc, getNode GetNodeFunc, getPodsAssignedToNode GetPodsByNodeNameFunc) *NoExecuteTaintManager {
	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "taint-controller"})
	eventBroadcaster.StartLogging(klog.Infof)
	if c != nil {
		klog.V(0).Infof("Sending events to api server.")
		//创建事件对象
		eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.CoreV1().Events("")})
	} else {
		//没有kubeClient，程序退出
		klog.Fatalf("kubeClient is nil when starting NodeController")
	}

	tm := &NoExecuteTaintManager{
		client:                c,
		recorder:              recorder,
		getPod:                getPod,
		getNode:               getNode,
		getPodsAssignedToNode: getPodsAssignedToNode,
		taintedNodes:          make(map[string][]v1.Taint),

		nodeUpdateQueue: workqueue.NewNamed("noexec_taint_node"),
		podUpdateQueue:  workqueue.NewNamed("noexec_taint_pod"),
	}
	//创建一个工作队列，提供处理函数：1. 事件告知Apiserver某个Pod即将删除  2. 尝试5次去删除该Pod
	tm.taintEvictionQueue = CreateWorkerQueue(deletePodHandler(c, tm.emitPodDeletionEvent))

	return tm
}

// Run starts NoExecuteTaintManager which will run in loop until `stopCh` is closed.
func (tc *NoExecuteTaintManager) Run(stopCh <-chan struct{}) {
	klog.V(0).Infof("Starting NoExecuteTaintManager")
	//初始化tc.nodeUpdateChannels以及podUpdateChannels列表，默认长度均为8
	for i := 0; i < UpdateWorkerSize; i++ {
		tc.nodeUpdateChannels = append(tc.nodeUpdateChannels, make(chan nodeUpdateItem, NodeUpdateChannelSize))
		tc.podUpdateChannels = append(tc.podUpdateChannels, make(chan podUpdateItem, podUpdateChannelSize))
	}

	// Functions that are responsible for taking work items out of the workqueues and putting them
	// into channels.
	//启动一个协程处理，当工作队列中有节点更新时，通知相应的进程进行处理
	go func(stopCh <-chan struct{}) {
		for {
			//等待nodeUpdateQueue中的数据
			item, shutdown := tc.nodeUpdateQueue.Get()
			// 如果队列关闭，则退出
			if shutdown {
				break
			}
			//更新的节点信息
			nodeUpdate := item.(nodeUpdateItem)
			//根据节点名，计算出节点由哪个工作线程来处理（默认有8个线程）
			hash := hash(nodeUpdate.nodeName, UpdateWorkerSize)
			select {
			case <-stopCh:
				tc.nodeUpdateQueue.Done(item)
				return
			case tc.nodeUpdateChannels[hash] <- nodeUpdate:
				// tc.nodeUpdateQueue.Done is called by the nodeUpdateChannels worker
			}
		}
	}(stopCh)
	//启动一个协程处理，当工作队列中有pod更新时，通知相应的进程进行处理
	go func(stopCh <-chan struct{}) {
		for {
			item, shutdown := tc.podUpdateQueue.Get()
			if shutdown {
				break
			}
			// The fact that pods are processed by the same worker as nodes is used to avoid races
			// between node worker setting tc.taintedNodes and pod worker reading this to decide
			// whether to delete pod.
			// It's possible that even without this assumption this code is still correct.
			podUpdate := item.(podUpdateItem)
			hash := hash(podUpdate.nodeName, UpdateWorkerSize)
			select {
			case <-stopCh:
				tc.podUpdateQueue.Done(item)
				return
			case tc.podUpdateChannels[hash] <- podUpdate:
				// tc.podUpdateQueue.Done is called by the podUpdateChannels worker
			}
		}
	}(stopCh)

	wg := sync.WaitGroup{}
	wg.Add(UpdateWorkerSize)

	//启动8个工作线程，处理节点更新以及Pod更新
	for i := 0; i < UpdateWorkerSize; i++ {
		go tc.worker(i, wg.Done, stopCh)
	}
	wg.Wait()
}

func (tc *NoExecuteTaintManager) worker(worker int, done func(), stopCh <-chan struct{}) {
	defer done()

	// When processing events we want to prioritize Node updates over Pod updates,
	// as NodeUpdates that interest NoExecuteTaintManager should be handled as soon as possible -
	// we don't want user (or system) to wait until PodUpdate queue is drained before it can
	// start evicting Pods from tainted Nodes.
	for {
		select {
		case <-stopCh:
			return
			//节点更新
		case nodeUpdate := <-tc.nodeUpdateChannels[worker]:
			tc.handleNodeUpdate(nodeUpdate)
			tc.nodeUpdateQueue.Done(nodeUpdate)
			//pod更新，先等到所有节点更新事件结束再处理pod. 好方法
		case podUpdate := <-tc.podUpdateChannels[worker]:
			// If we found a Pod update we need to empty Node queue first.
		priority:
			for {
				select {
				case nodeUpdate := <-tc.nodeUpdateChannels[worker]:
					tc.handleNodeUpdate(nodeUpdate)
					tc.nodeUpdateQueue.Done(nodeUpdate)
				default:
					break priority
				}
			}
			// After Node queue is emptied we process podUpdate.
			tc.handlePodUpdate(podUpdate)
			tc.podUpdateQueue.Done(podUpdate)
		}
	}
}

// PodUpdated is used to notify NoExecuteTaintManager about Pod changes.
func (tc *NoExecuteTaintManager) PodUpdated(oldPod *v1.Pod, newPod *v1.Pod) {
	podName := ""
	podNamespace := ""
	nodeName := ""
	oldTolerations := []v1.Toleration{}
	if oldPod != nil {
		podName = oldPod.Name
		podNamespace = oldPod.Namespace
		nodeName = oldPod.Spec.NodeName
		oldTolerations = oldPod.Spec.Tolerations
	}
	newTolerations := []v1.Toleration{}
	if newPod != nil {
		podName = newPod.Name
		podNamespace = newPod.Namespace
		nodeName = newPod.Spec.NodeName
		newTolerations = newPod.Spec.Tolerations
	}

	if oldPod != nil && newPod != nil && helper.Semantic.DeepEqual(oldTolerations, newTolerations) && oldPod.Spec.NodeName == newPod.Spec.NodeName {
		return
	}
	updateItem := podUpdateItem{
		podName:      podName,
		podNamespace: podNamespace,
		nodeName:     nodeName,
	}

	tc.podUpdateQueue.Add(updateItem)
}

// NodeUpdated is used to notify NoExecuteTaintManager about Node changes.
func (tc *NoExecuteTaintManager) NodeUpdated(oldNode *v1.Node, newNode *v1.Node) {
	nodeName := ""
	oldTaints := []v1.Taint{}
	if oldNode != nil {
		nodeName = oldNode.Name
		oldTaints = getNoExecuteTaints(oldNode.Spec.Taints)
	}

	newTaints := []v1.Taint{}
	if newNode != nil {
		nodeName = newNode.Name
		newTaints = getNoExecuteTaints(newNode.Spec.Taints)
	}

	if oldNode != nil && newNode != nil && helper.Semantic.DeepEqual(oldTaints, newTaints) {
		return
	}
	updateItem := nodeUpdateItem{
		nodeName: nodeName,
	}

	tc.nodeUpdateQueue.Add(updateItem)
}

//取消对Pod nsName的定时删除任务，并通知apiserver关于该节点的删除取消事件
func (tc *NoExecuteTaintManager) cancelWorkWithEvent(nsName types.NamespacedName) {
	if tc.taintEvictionQueue.CancelWork(nsName.String()) {
		tc.emitCancelPodDeletionEvent(nsName)
	}
}

// 1.  当节点不存在Effect=NotExecute的污点时，将Pod从taintEvictionQueue移除，并取消定时删除任务
// 2.  当Pod不能容忍节点所有Effect=NotExecute的污点时，将Pod先从taintEvictionQueue移除，并取消定时删除任务（如果有）。再利用taintEvictionQueue启动协程直接删除Pod
// 3.  当Pod容忍节点所有Effect=NotExecute的污点时， 如果没有设置容忍时间。将Pod先从taintEvictionQueue移除，并取消定时删除任务（如果有）
// 4.  当Pod容忍节点所有Effect=NotExecute的污点时， 如果设置容忍时间。算出最小容忍时间，移除原来的定时任务，采用新的定时删除任务（定时为最小容忍时间）
func (tc *NoExecuteTaintManager) processPodOnNode(
	podNamespacedName types.NamespacedName,
	nodeName string,
	tolerations []v1.Toleration,
	taints []v1.Taint,
	now time.Time,
) {
	//如果节点的NotEffectTain为空
	if len(taints) == 0 {
		//取消该Pod的定时删除
		tc.cancelWorkWithEvent(podNamespacedName)
	}
	// 检测是否所有taint都能够容忍
	allTolerated, usedTolerations := v1helper.GetMatchingTolerations(taints, tolerations)
	//如果Pod不能容忍节点上所有污点，如果Pod已经在taintEvictionQueue中。则取消其定时删除任务，改成立即删除。
	if !allTolerated {
		klog.V(2).Infof("Not all taints are tolerated after update for Pod %v on %v", podNamespacedName.String(), nodeName)
		// We're canceling scheduled work (if any), as we're going to delete the Pod right away.
		tc.cancelWorkWithEvent(podNamespacedName)                                                                               //移除定时删除
		tc.taintEvictionQueue.AddWork(NewWorkArgs(podNamespacedName.Name, podNamespacedName.Namespace), time.Now(), time.Now()) // 这里是直接删除
		return
	}

	//获取最小容忍时间
	minTolerationTime := getMinTolerationTime(usedTolerations)
	// getMinTolerationTime returns negative value to denote infinite toleration.
	// Pod对节点污点的容忍没有设置期限
	if minTolerationTime < 0 {
		klog.V(4).Infof("Current tolerations for %v tolerate forever, cancelling any scheduled deletion.", podNamespacedName.String())
		//取消Pod的定时删除
		tc.cancelWorkWithEvent(podNamespacedName)
		return
	}

	startTime := now
	triggerTime := startTime.Add(minTolerationTime) //最小容忍时间
	//获取当前Pod
	scheduledEviction := tc.taintEvictionQueue.GetWorkerUnsafe(podNamespacedName.String())
	if scheduledEviction != nil {
		startTime = scheduledEviction.CreatedAt
		if startTime.Add(minTolerationTime).Before(triggerTime) {
			return
		}
		tc.cancelWorkWithEvent(podNamespacedName)
	}
	//添加定时任务，在最小容忍时间到达后删除对应的Podcast
	tc.taintEvictionQueue.AddWork(NewWorkArgs(podNamespacedName.Name, podNamespacedName.Namespace), startTime, triggerTime)
}

//处理Pod更新事件
func (tc *NoExecuteTaintManager) handlePodUpdate(podUpdate podUpdateItem) {
	pod, err := tc.getPod(podUpdate.podName, podUpdate.podNamespace)
	if err != nil {
		// Pod已经被删除
		if apierrors.IsNotFound(err) {
			// Delete
			podNamespacedName := types.NamespacedName{Namespace: podUpdate.podNamespace, Name: podUpdate.podName}
			klog.V(4).Infof("Noticed pod deletion: %#v", podNamespacedName)
			tc.cancelWorkWithEvent(podNamespacedName)
			return
		}
		utilruntime.HandleError(fmt.Errorf("could not get pod %s/%s: %v", podUpdate.podName, podUpdate.podNamespace, err))
		return
	}

	// We key the workqueue and shard workers by nodeName. If we don't match the current state we should not be the one processing the current object.
	if pod.Spec.NodeName != podUpdate.nodeName {
		return
	}

	// Create or Update
	podNamespacedName := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	klog.V(4).Infof("Noticed pod update: %#v", podNamespacedName)
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return
	}
	taints, ok := func() ([]v1.Taint, bool) {
		tc.taintedNodesLock.Lock()
		defer tc.taintedNodesLock.Unlock()
		taints, ok := tc.taintedNodes[nodeName]
		return taints, ok
	}()
	// It's possible that Node was deleted, or Taints were removed before, which triggered
	// eviction cancelling if it was needed.
	if !ok {
		return
	}
	tc.processPodOnNode(podNamespacedName, nodeName, pod.Spec.Tolerations, taints, time.Now())
}

//1.如果节点不存在，将节点在节点污点表的信息移除
//2.
func (tc *NoExecuteTaintManager) handleNodeUpdate(nodeUpdate nodeUpdateItem) {
	// 获取节点信息
	node, err := tc.getNode(nodeUpdate.nodeName)
	if err != nil {
		// 如果节点不存在，将节点相关的taint信息从节点污点表中移除
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("Noticed node deletion: %#v", nodeUpdate.nodeName)
			tc.taintedNodesLock.Lock()
			defer tc.taintedNodesLock.Unlock()
			delete(tc.taintedNodes, nodeUpdate.nodeName) //将节点相关的taint信息从节点表中移除
			return
		}
		utilruntime.HandleError(fmt.Errorf("cannot get node %s: %v", nodeUpdate.nodeName, err))
		return
	}

	// Create or Update
	klog.V(4).Infof("Noticed node update: %#v", nodeUpdate)
	//获取Node所有Effect=NoExecute的Taint
	taints := getNoExecuteTaints(node.Spec.Taints)
	func() {
		tc.taintedNodesLock.Lock()
		defer tc.taintedNodesLock.Unlock()
		klog.V(4).Infof("Updating known taints on node %v: %v", node.Name, taints)
		//如果节点没有了Effect=NoExecute的Taint,则从节点污点表中移除
		if len(taints) == 0 {
			delete(tc.taintedNodes, node.Name)
		} else {
			//更新节点NoExecuteTaint表
			tc.taintedNodes[node.Name] = taints
		}
	}()

	// This is critical that we update tc.taintedNodes before we call getPodsAssignedToNode:
	// getPodsAssignedToNode can be delayed as long as all future updates to pods will call
	// tc.PodUpdated which will use tc.taintedNodes to potentially delete delayed pods.
	//获取缓存中所有在节点nodeName上的Pod列表
	pods, err := tc.getPodsAssignedToNode(node.Name)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}
	//没有节点则不处理
	if len(pods) == 0 {
		return
	}
	// Short circuit, to make this controller a bit faster.
	//该节点没有Effect=NoExecute的Taint
	if len(taints) == 0 {
		klog.V(4).Infof("All taints were removed from the Node %v. Cancelling all evictions...", node.Name)
		for i := range pods {
			//取消所有该节点上Pod的定时删除
			tc.cancelWorkWithEvent(types.NamespacedName{Namespace: pods[i].Namespace, Name: pods[i].Name})
		}
		return
	}

	now := time.Now()
	//对节点上所有的Pod,对比taint和toleration来决定如何处理Pod(立即删除，定时删除，还是不删除)
	for _, pod := range pods {
		podNamespacedName := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		tc.processPodOnNode(podNamespacedName, node.Name, pod.Spec.Tolerations, taints, now)
	}
}

//生成事件，标记指定pod的删除（调用者标记后会尝试去删除指定的pod）
func (tc *NoExecuteTaintManager) emitPodDeletionEvent(nsName types.NamespacedName) {
	if tc.recorder == nil {
		return
	}
	ref := &v1.ObjectReference{
		Kind:      "Pod",
		Name:      nsName.Name,
		Namespace: nsName.Namespace,
	}
	tc.recorder.Eventf(ref, v1.EventTypeNormal, "TaintManagerEviction", "Marking for deletion Pod %s", nsName.String())
}

//生成事件，告知取消指定pod的删除
func (tc *NoExecuteTaintManager) emitCancelPodDeletionEvent(nsName types.NamespacedName) {
	if tc.recorder == nil {
		return
	}
	ref := &v1.ObjectReference{
		Kind:      "Pod",
		Name:      nsName.Name,
		Namespace: nsName.Namespace,
	}
	tc.recorder.Eventf(ref, v1.EventTypeNormal, "TaintManagerEviction", "Cancelling deletion of Pod %s", nsName.String())
}
