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

package workqueue

import (
	"container/heap"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/clock"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// DelayingInterface is an Interface that can Add an item at a later time. This makes it easier to
// requeue items after failures without ending up in a hot-loop.
type DelayingInterface interface {
	Interface
	// AddAfter adds an item to the workqueue after the indicated duration has passed
	// 1.如果没有指定等待时间，立即加入工作队列 2. 如果指定等待时间，加入等待队列（异步协程处理)；如果已经在等待队列中则更新等待时间
	AddAfter(item interface{}, duration time.Duration)
}

// NewDelayingQueue constructs a new workqueue with delayed queuing ability
func NewDelayingQueue() DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, "")
}

// NewNamedDelayingQueue constructs a new named workqueue with delayed queuing ability
func NewNamedDelayingQueue(name string) DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, name)
}

// NewDelayingQueueWithCustomClock constructs a new named workqueue
// with ability to inject real or fake clock for testing purposes
func NewDelayingQueueWithCustomClock(clock clock.Clock, name string) DelayingInterface {
	ret := &delayingType{
		Interface:       NewNamed(name),
		clock:           clock,
		heartbeat:       clock.NewTicker(maxWait), //每隔10秒的定时器
		stopCh:          make(chan struct{}),
		waitingForAddCh: make(chan *waitFor, 1000),
		metrics:         newRetryMetrics(name),
	}
	//启动一个异步协程用来处理等待队列中的对象
	go ret.waitingLoop()

	return ret
}

// delayingType wraps an Interface and provides delayed re-enquing
// 启动一个协程用于管理等待插入对象，等待时间结束后插入主工作队列
type delayingType struct {
	Interface //主工作队列

	// clock tracks time for delayed firing
	clock clock.Clock //时钟

	// stopCh lets us signal a shutdown to the waiting loop
	stopCh chan struct{} //通知等待协程停止工作
	// stopOnce guarantees we only signal shutdown a single time
	stopOnce sync.Once

	// heartbeat ensures we wait no more than maxWait before firing
	heartbeat clock.Ticker //每隔一定周期检测下等待队列是否有对象等待时间已经结束

	// waitingForAddCh is a buffered channel that feeds waitingForAdd
	waitingForAddCh chan *waitFor //将要等待插入的对象告知处理的协程

	// metrics counts the number of retries
	metrics retryMetrics
}

// waitFor holds the data to add and the time it should be added
type waitFor struct {
	data    t         //对象的时间
	readyAt time.Time //插入队列的时间
	// index in the priority queue (heap)
	index int // 堆中的索引
}

// waitForPriorityQueue implements a priority queue for waitFor items.
//
// waitForPriorityQueue implements heap.Interface. The item occurring next in
// time (i.e., the item with the smallest readyAt) is at the root (index 0).
// Peek returns this minimum item at index 0. Pop returns the minimum item after
// it has been removed from the queue and placed at index Len()-1 by
// container/heap. Push adds an item at index Len(), and container/heap
// percolates it into the correct location.
type waitForPriorityQueue []*waitFor //用最小堆实现，按照 准备插入主队列的时间来排序

func (pq waitForPriorityQueue) Len() int {
	return len(pq)
}

//按插入时间从小到大排序
func (pq waitForPriorityQueue) Less(i, j int) bool {
	return pq[i].readyAt.Before(pq[j].readyAt)
}
func (pq waitForPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push adds an item to the queue. Push should not be called directly; instead,
// use `heap.Push`.
func (pq *waitForPriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*waitFor)
	item.index = n
	*pq = append(*pq, item)
}

// Pop removes an item from the queue. Pop should not be called directly;
// instead, use `heap.Pop`.
func (pq *waitForPriorityQueue) Pop() interface{} {
	n := len(*pq)
	item := (*pq)[n-1]
	item.index = -1
	*pq = (*pq)[0:(n - 1)]
	return item
}

// Peek returns the item at the beginning of the queue, without removing the
// item or otherwise mutating the queue. It is safe to call directly.

func (pq waitForPriorityQueue) Peek() interface{} {
	return pq[0]
}

// ShutDown stops the queue. After the queue drains, the returned shutdown bool
// on Get() will be true. This method may be invoked more than once.
func (q *delayingType) ShutDown() {
	q.stopOnce.Do(func() {
		q.Interface.ShutDown()
		close(q.stopCh)
		q.heartbeat.Stop()
	})
}

// AddAfter adds the given item to the work queue after the given delay
// 1.如果没有指定等待时间，立即加入工作队列 2. 如果指定等待时间，加入等待队列；如果已经在等待队列中则更新等待时间
func (q *delayingType) AddAfter(item interface{}, duration time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	q.metrics.retry()

	// immediately add things with no delay
	// 如果没有指定等待时间，立刻加到等待队列中
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
		//加入到等待插入队列中，等待插入。
		//如果之前已经加入等待插入队列，则更新等待时间
	case q.waitingForAddCh <- &waitFor{data: item, readyAt: q.clock.Now().Add(duration)}:
	}
}

// maxWait keeps a max bound on the wait time. It's just insurance against weird things happening.
// Checking the queue every 10 seconds isn't expensive and we know that we'll never end up with an
// expired item sitting for more than 10 seconds.
const maxWait = 10 * time.Second //每隔10秒钟会主动检测等待队列是否有对象结束等待

// waitingLoop runs until the workqueue is shutdown and keeps a check on the list of items to be added.
func (q *delayingType) waitingLoop() {
	defer utilruntime.HandleCrash()

	// Make a placeholder channel to use when there are no items in our list
	never := make(<-chan time.Time)

	// Make a timer that expires when the item at the head of the waiting queue is ready
	var nextReadyAtTimer clock.Timer //最短等待插入对象定时器
	//正在等待插入主队列的对象
	waitingForQueue := &waitForPriorityQueue{}
	heap.Init(waitingForQueue) // 将队列初始化堆。 这么做的好处是：队列按插入时间进行排序，当队列中某个对象更新了插入时间，能够快速的排序
	//表用于查找正在等待插入主队列的对象
	waitingEntryByData := map[t]*waitFor{}

	for {
		//如果主工作队列关闭，退出
		if q.Interface.ShuttingDown() {
			return
		}

		now := q.clock.Now()

		// Add ready entries
		// 这种方式非常好，因为队列的对象很可能会更改时间，从而导致队列排序
		// 通过遍历的方式就可以不用关心哪个对象有更新等待时间，反正都处理。因为是通过chan的方式来插入，可以保证在处理过程中不会有新的对象插入
		// 有对象在等待队列中排队
		for waitingForQueue.Len() > 0 {
			//获得等待时间最短的对象
			entry := waitingForQueue.Peek().(*waitFor)
			//仍需要等待
			if entry.readyAt.After(now) {
				break
			}
			//提取出等待的对象
			entry = heap.Pop(waitingForQueue).(*waitFor)
			q.Add(entry.data)                      //插入主工作队列
			delete(waitingEntryByData, entry.data) //从等待表中删除
		}

		// Set up a wait for the first item's readyAt (if one exists)
		nextReadyAt := never //最短等待时间的对象的定时器
		if waitingForQueue.Len() > 0 {
			if nextReadyAtTimer != nil {
				nextReadyAtTimer.Stop()
			}
			//获取最短等待时间的对象，创建一个定时器
			entry := waitingForQueue.Peek().(*waitFor)
			nextReadyAtTimer = q.clock.NewTimer(entry.readyAt.Sub(now))
			nextReadyAt = nextReadyAtTimer.C()
		}

		select {
		case <-q.stopCh:
			return
		//心跳时间到，切换到下次循环, 会立刻处理等待时间结束的对象 (用于在心跳间隔添加的等待对象,还没建立定时器)
		case <-q.heartbeat.C():
			// continue the loop, which will add ready items
		// 最短等待时间的对象等待时间结束了。切换到下次循环, 会立刻处理等待时间结束的对象（遍历列表）
		// 如果该对象在定时器到期前改变了等待时间（延长了）也无所谓。下次循环也是通过遍历
		case <-nextReadyAt:
			// continue the loop, which will add ready items
		// 接收到有需要延时添加到工作队列的对象
		case waitEntry := <-q.waitingForAddCh:
			// 还没有到插入的时间
			if waitEntry.readyAt.After(q.clock.Now()) {
				insert(waitingForQueue, waitingEntryByData, waitEntry)
			} else {
				// 延时已经过期，直接插入到工作队列中
				q.Add(waitEntry.data)
			}

			drained := false
			for !drained {
				select {
				// 如果还有数据插入，继续处理，直到没有新数据
				case waitEntry := <-q.waitingForAddCh:
					if waitEntry.readyAt.After(q.clock.Now()) {
						insert(waitingForQueue, waitingEntryByData, waitEntry)
					} else {
						q.Add(waitEntry.data)
					}
				default:
					drained = true
				}
			}
		}
	}
}

// insert adds the entry to the priority queue, or updates the readyAt if it already exists in the queue
func insert(q *waitForPriorityQueue, knownEntries map[t]*waitFor, entry *waitFor) {
	// if the entry already exists, update the time only if it would cause the item to be queued sooner
	//同一个对象已经在等待插入队列中了
	existing, exists := knownEntries[entry.data]
	if exists {
		// 对象插入时间已经更新(等待时间提前)，更新对象，并且调整等待队列排序
		if existing.readyAt.After(entry.readyAt) {
			existing.readyAt = entry.readyAt
			heap.Fix(q, existing.index)
		}

		return
	}
	//插入对象到等待队列
	heap.Push(q, entry)
	//记录该对象在等待表中。
	knownEntries[entry.data] = entry
}
