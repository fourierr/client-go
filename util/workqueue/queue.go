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

package workqueue

import (
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// 通用FIFO 队列
type Interface interface {
	// 为队列添添加一个元素
	Add(item interface{})
	// 获取队列长度
	Len() int
	// 从头部获取一个元素
	Get() (item interface{}, shutdown bool)
	// 标记一个元素处理完成
	Done(item interface{})
	// 关闭队列
	ShutDown()
	ShutDownWithDrain()
	// 获取队列是否正在关闭
	ShuttingDown() bool
}

// New constructs a new work queue (see the package comment).
func New() *Type {
	return NewNamed("")
}

func NewNamed(name string) *Type {
	rc := clock.RealClock{}
	return newQueue(
		rc,
		globalMetricsFactory.newQueueMetrics(name, rc),
		defaultUnfinishedWorkUpdatePeriod,
	)
}

func newQueue(c clock.WithTicker, metrics queueMetrics, updatePeriod time.Duration) *Type {
	t := &Type{
		clock:                      c,
		dirty:                      set{},
		processing:                 set{},
		cond:                       sync.NewCond(&sync.Mutex{}),
		metrics:                    metrics,
		unfinishedWorkUpdatePeriod: updatePeriod,
	}

	// Don't start the goroutine for a type of noMetrics so we don't consume
	// resources unnecessarily
	if _, ok := metrics.(noMetrics); !ok {
		go t.updateUnfinishedWorkLoop()
	}

	return t
}

const defaultUnfinishedWorkUpdatePeriod = 500 * time.Millisecond

// Type is a work queue (see the package comment).
// 具体queue的实现
type Type struct {
	// queue defines the order in which we will work on items. Every
	// element of queue should be in the dirty set and not in the
	// processing set.
	// 存储具体元素，类型为slice
	queue []t

	// dirty defines all of the items that need to be processed.
	// 定义所有的元素被处理，是以map类型，可以对元素去重
	dirty set

	// Things that are currently being processed are in the processing set.
	// These things may be simultaneously in the dirty set. When we finish
	// processing something and remove it from this set, we'll check if
	// it's in the dirty set, and if so, add it to the queue.
	// 标记元素是否正在处理。
	// 有些元素可能同时存在processing和dirty中，当处理完这个元素，从集合删除的时候，如果发现元素还在dirty集合中，在此将其添加至queue队尾
	processing set

	cond *sync.Cond

	shuttingDown bool
	drain        bool

	metrics queueMetrics

	unfinishedWorkUpdatePeriod time.Duration
	clock                      clock.WithTicker
}

type empty struct{}
type t interface{}

// set 集合，用 map 来模拟 set 集合（元素不重复），更快效率的查找元素
type set map[t]empty

func (s set) has(item t) bool {
	_, exists := s[item]
	return exists
}

func (s set) insert(item t) {
	s[item] = empty{}
}

func (s set) delete(item t) {
	delete(s, item)
}

func (s set) len() int {
	return len(s)
}

// Add marks item as needing processing.
func (q *Type) Add(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	// 如果队列已经关闭则直接返回
	if q.shuttingDown {
		return
	}
	// 如果dirty中已经有该元素，则直接返回，目的是实现去重
	if q.dirty.has(item) {
		return
	}

	q.metrics.add(item)
	// 添加元素到dirty中
	q.dirty.insert(item)
	// 如果元素正在处理，则直接返回
	if q.processing.has(item) {
		return
	}
	// 添加元素到queue中
	q.queue = append(q.queue, item)
	// 通知其他协程接触阻塞
	q.cond.Signal()
}

// Len returns the current queue length, for informational purposes only. You
// shouldn't e.g. gate a call to Add() or Get() on Len() being a particular
// value, that can't be synchronized properly.
func (q *Type) Len() int {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return len(q.queue)
}

// Get blocks until it can return an item to be processed. If shutdown = true,
// the caller should end their goroutine. You must call Done with item when you
// have finished processing it.
func (q *Type) Get() (item interface{}, shutdown bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	// 如果队列为关闭，且队列长度为0，则进行阻塞，待元素添加进队列发送解除阻塞通知执行
	for len(q.queue) == 0 && !q.shuttingDown {
		q.cond.Wait()
	}
	// 如果队列长度为0，则关闭队列
	if len(q.queue) == 0 {
		// We must be shutting down.
		return nil, true
	}

	item = q.queue[0]
	// The underlying array still exists and reference this object, so the object will not be garbage collected.
	// 从队列头部弹出第一个元素
	q.queue[0] = nil
	q.queue = q.queue[1:]

	q.metrics.get(item)
	// 将元素插入到processing中，标记为正在处理
	q.processing.insert(item)
	// 从dirty中删除队列
	q.dirty.delete(item)

	return item, false
}

// Done marks item as done processing, and if it has been marked as dirty again
// while it was being processed, it will be re-added to the queue for
// re-processing.
func (q *Type) Done(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	q.metrics.done(item)
	// 将processing中的元素移除
	q.processing.delete(item)
	// 判断元素是否还在dirty中，如果在则将其重新添加至queue队尾
	if q.dirty.has(item) {
		q.queue = append(q.queue, item)
		q.cond.Signal()
	} else if q.processing.len() == 0 {
		q.cond.Signal()
	}
}

// ShutDown will cause q to ignore all new items added to it and
// immediately instruct the worker goroutines to exit.
func (q *Type) ShutDown() {
	q.setDrain(false)
	q.shutdown()
}

// ShutDownWithDrain will cause q to ignore all new items added to it. As soon
// as the worker goroutines have "drained", i.e: finished processing and called
// Done on all existing items in the queue; they will be instructed to exit and
// ShutDownWithDrain will return. Hence: a strict requirement for using this is;
// your workers must ensure that Done is called on all items in the queue once
// the shut down has been initiated, if that is not the case: this will block
// indefinitely. It is, however, safe to call ShutDown after having called
// ShutDownWithDrain, as to force the queue shut down to terminate immediately
// without waiting for the drainage.
func (q *Type) ShutDownWithDrain() {
	q.setDrain(true)
	q.shutdown()
	for q.isProcessing() && q.shouldDrain() {
		q.waitForProcessing()
	}
}

// isProcessing indicates if there are still items on the work queue being
// processed. It's used to drain the work queue on an eventual shutdown.
func (q *Type) isProcessing() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return q.processing.len() != 0
}

// waitForProcessing waits for the worker goroutines to finish processing items
// and call Done on them.
func (q *Type) waitForProcessing() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	// Ensure that we do not wait on a queue which is already empty, as that
	// could result in waiting for Done to be called on items in an empty queue
	// which has already been shut down, which will result in waiting
	// indefinitely.
	if q.processing.len() == 0 {
		return
	}
	q.cond.Wait()
}

func (q *Type) setDrain(shouldDrain bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	q.drain = shouldDrain
}

func (q *Type) shouldDrain() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return q.drain
}

func (q *Type) shutdown() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	q.shuttingDown = true
	q.cond.Broadcast()
}

func (q *Type) ShuttingDown() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	return q.shuttingDown
}

func (q *Type) updateUnfinishedWorkLoop() {
	t := q.clock.NewTicker(q.unfinishedWorkUpdatePeriod)
	defer t.Stop()
	for range t.C() {
		if !func() bool {
			q.cond.L.Lock()
			defer q.cond.L.Unlock()
			if !q.shuttingDown {
				q.metrics.updateUnfinishedWork()
				return true
			}
			return false

		}() {
			return
		}
	}
}
