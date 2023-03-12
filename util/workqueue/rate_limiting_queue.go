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

// RateLimitingInterface is an interface that rate limits items being added to the queue.
type RateLimitingInterface interface {
	DelayingInterface

	// AddRateLimited adds an item to the workqueue after the rate limiter says it's ok
	// 增加一个元素大限速队列
	AddRateLimited(item interface{})

	// Forget indicates that an item is finished being retried.  Doesn't matter whether it's for perm failing
	// or for success, we'll stop the rate limiter from tracking it.  This only clears the `rateLimiter`, you
	// still have to call `Done` on the queue.
	// 表示完成该元素重试，丢弃掉该元素
	Forget(item interface{})

	// NumRequeues returns back how many times the item was requeued
	// 查询元素入队列的次数
	NumRequeues(item interface{}) int
}

// NewRateLimitingQueue constructs a new workqueue with rateLimited queuing ability
// Remember to call Forget!  If you don't, you may end up tracking failures forever.
// NewRateLimitingQueue does not emit metrics. For use with a MetricsProvider, please use
// NewNamedRateLimitingQueue instead.
func NewRateLimitingQueue(rateLimiter RateLimiter) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewDelayingQueue(),
		rateLimiter:       rateLimiter,
	}
}

func NewNamedRateLimitingQueue(rateLimiter RateLimiter, name string) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewNamedDelayingQueue(name),
		rateLimiter:       rateLimiter,
	}
}

func NewRateLimitingQueueWithDelayingInterface(di DelayingInterface, rateLimiter RateLimiter) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: di,
		rateLimiter:       rateLimiter,
	}
}

// rateLimitingType wraps an Interface and provides rateLimited re-enquing
// rateLimitingType 是限速队列的具体实现
type rateLimitingType struct {
	// 继承延迟队列
	DelayingInterface
	// 新增限速接口
	rateLimiter RateLimiter
}

// AddRateLimited AddAfter's the item based on the time when the rate limiter says it's ok
// 获取到限速器延迟时间，然后加入到延迟队列
func (q *rateLimitingType) AddRateLimited(item interface{}) {
	q.DelayingInterface.AddAfter(item, q.rateLimiter.When(item))
}

// NumRequeues 通过限速器获取元素加入队列的次数
func (q *rateLimitingType) NumRequeues(item interface{}) int {
	return q.rateLimiter.NumRequeues(item)
}

// Forget 通过限速器丢弃元素
func (q *rateLimitingType) Forget(item interface{}) {
	q.rateLimiter.Forget(item)
}
