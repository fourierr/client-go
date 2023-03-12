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

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/clock"
)

// DelayingInterface is an Interface that can Add an item at a later time. This makes it easier to
// requeue items after failures without ending up in a hot-loop.
type DelayingInterface interface {
	Interface
	// AddAfter adds an item to the workqueue after the indicated duration has passed
	// 在此刻过duration时间后再加入到queue中
	AddAfter(item interface{}, duration time.Duration)
}

// NewDelayingQueue constructs a new workqueue with delayed queuing ability.
// NewDelayingQueue does not emit metrics. For use with a MetricsProvider, please use
// NewNamedDelayingQueue instead.
func NewDelayingQueue() DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, "")
}

// NewDelayingQueueWithCustomQueue constructs a new workqueue with ability to
// inject custom queue Interface instead of the default one
func NewDelayingQueueWithCustomQueue(q Interface, name string) DelayingInterface {
	return newDelayingQueue(clock.RealClock{}, q, name)
}

// NewNamedDelayingQueue constructs a new named workqueue with delayed queuing ability
func NewNamedDelayingQueue(name string) DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, name)
}

// NewDelayingQueueWithCustomClock constructs a new named workqueue
// with ability to inject real or fake clock for testing purposes
func NewDelayingQueueWithCustomClock(clock clock.WithTicker, name string) DelayingInterface {
	return newDelayingQueue(clock, NewNamed(name), name)
}

func newDelayingQueue(clock clock.WithTicker, q Interface, name string) *delayingType {
	ret := &delayingType{
		Interface:       q,
		clock:           clock,
		heartbeat:       clock.NewTicker(maxWait),
		stopCh:          make(chan struct{}),
		waitingForAddCh: make(chan *waitFor, 1000),
		metrics:         newRetryMetrics(name),
	}

	go ret.waitingLoop()
	return ret
}

// delayingType wraps an Interface and provides delayed re-enquing
// 延迟队列的实现为delayingType，其继承了通用接口，在其基础上主要新增了waitingForAddCh字段，来实现延迟的功能
type delayingType struct {
	Interface

	// clock tracks time for delayed firing
	clock clock.Clock

	// stopCh lets us signal a shutdown to the waiting loop
	stopCh chan struct{}
	// stopOnce guarantees we only signal shutdown a single time
	//保证仅仅发送一次关闭信号
	stopOnce sync.Once

	// heartbeat ensures we wait no more than maxWait before firing
	// 在触发之前确保等待的时间不超过maxWait
	heartbeat clock.Ticker

	// waitingForAddCh is a buffered channel that feeds waitingForAdd
	// waitingForAddCh是一个缓冲channel
	waitingForAddCh chan *waitFor

	// metrics counts the number of retries
	// 记录重试次数
	metrics retryMetrics
}

// waitFor holds the data to add and the time it should be added
// 保存了包括数据`data`和该`item`什么时间起(`readyAt`)就可以进入队列了.
type waitFor struct {
	data    t
	readyAt time.Time
	// index in the priority queue (heap)
	index int
}

// waitForPriorityQueue implements a priority queue for waitFor items.
//
// waitForPriorityQueue implements heap.Interface. The item occurring next in
// time (i.e., the item with the smallest readyAt) is at the root (index 0).
// Peek returns this minimum item at index 0. Pop returns the minimum item after
// it has been removed from the queue and placed at index Len()-1 by
// container/heap. Push adds an item at index Len(), and container/heap
// percolates it into the correct location.
// waitForPriorityQueue 为 waitFor 项目实现优先级队列
// 是用于保存`waitFor`的优先队列, 按`readyAt`时间从早到晚排序. 先`ready`的`item`先出队列.
type waitForPriorityQueue []*waitFor

func (pq waitForPriorityQueue) Len() int {
	return len(pq)
}
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
func (q *delayingType) AddAfter(item interface{}, duration time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	q.metrics.retry()

	// immediately add things with no delay
	// 如果传入的时间小于等于0则立即添加
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
		// 构造一个waitFor对象传入到waitingForAddCh通道中
	case q.waitingForAddCh <- &waitFor{data: item, readyAt: q.clock.Now().Add(duration)}:
	}
}

// maxWait keeps a max bound on the wait time. It's just insurance against weird things happening.
// Checking the queue every 10 seconds isn't expensive and we know that we'll never end up with an
// expired item sitting for more than 10 seconds.
const maxWait = 10 * time.Second

// waitingLoop runs until the workqueue is shutdown and keeps a check on the list of items to be added.
// waitingLoop 方法一直运行到工作队列关闭，并检查要添加的项目列表，利用channel(q.waitingForAddCh)一直接受从AddAfter过来的waitFor，
// 将已经ready的item添加到队列中。
func (q *delayingType) waitingLoop() {
	defer utilruntime.HandleCrash()

	// Make a placeholder channel to use when there are no items in our list
	// // 创建一个占位channel，当list没有元素的时候，利用其实现等待
	never := make(<-chan time.Time)

	// Make a timer that expires when the item at the head of the waiting queue is ready
	// 构造一个定时器，当等待队列头部的元素准备好后，该定时器失效
	var nextReadyAtTimer clock.Timer

	waitingForQueue := &waitForPriorityQueue{}
	heap.Init(waitingForQueue)
	// 利用map实现去重，如果在此添加相同元素，仅实现更新时间
	waitingEntryByData := map[t]*waitFor{}

	for {
		if q.Interface.ShuttingDown() {
			return
		}

		now := q.clock.Now()

		// Add ready entries
		// 如果优先队列有元素
		for waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek().(*waitFor)
			// 如果队列头部的元素时间已经大于现在时间，则表明整个优先级队列中的元素都大于当前时间，还没到插入元素的时间，则直接break
			// 第一个元素时间是最小的
			if entry.readyAt.After(now) {
				break
			}
			// 否则说明以及改到了插入数据时间了，执行插入到通用队列中
			entry = heap.Pop(waitingForQueue).(*waitFor)
			q.Add(entry.data)
			// 并从优先级队列删除
			delete(waitingEntryByData, entry.data)
		}
		// 当优先级队列中没有元素，则在后面进行等待
		// Set up a wait for the first item's readyAt (if one exists)
		nextReadyAt := never
		// 如果优先级队列中有元素
		if waitingForQueue.Len() > 0 {
			if nextReadyAtTimer != nil {
				nextReadyAtTimer.Stop()
			}
			// 获取第一个元素
			entry := waitingForQueue.Peek().(*waitFor)
			// 构造一个nextReady时间等待器
			nextReadyAtTimer = q.clock.NewTimer(entry.readyAt.Sub(now))
			nextReadyAt = nextReadyAtTimer.C()
		}

		select {
		case <-q.stopCh:
			return
			// 定时器，如果过一段时间在没有任何数据，则进行一次大循环
		case <-q.heartbeat.C():
			// continue the loop, which will add ready items

		case <-nextReadyAt:
			// continue the loop, which will add ready items
			//addAfter放入的元素从该处获取
		case waitEntry := <-q.waitingForAddCh:
			// 如果元素时间大于当前时间，则添加到优先级队列中
			if waitEntry.readyAt.After(q.clock.Now()) {
				insert(waitingForQueue, waitingEntryByData, waitEntry)
			} else {
				// 如果元素时间大于当前时间，则添加到优先级队列中
				q.Add(waitEntry.data)
			}
			// 将channel中的元素全部取出来
			drained := false
			for !drained {
				select {
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
// 添加元素至优先级队列，如果元素相同则进行更新时间
func insert(q *waitForPriorityQueue, knownEntries map[t]*waitFor, entry *waitFor) {
	// if the entry already exists, update the time only if it would cause the item to be queued sooner
	// 判断队列中是否已经存在该元素
	existing, exists := knownEntries[entry.data]
	// 如果存则，则直接更新元素
	if exists {
		if existing.readyAt.After(entry.readyAt) {
			existing.readyAt = entry.readyAt
			heap.Fix(q, existing.index)
		}

		return
	}
	// 不存在则添加进优先级队列
	heap.Push(q, entry)
	knownEntries[entry.data] = entry
}
