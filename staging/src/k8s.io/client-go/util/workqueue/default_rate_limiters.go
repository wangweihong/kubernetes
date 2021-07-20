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
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type RateLimiter interface {
	// When gets an item and gets to decide how long that item should wait
	When(item interface{}) time.Duration //计算对象需要等待多长时间u，有些限速器会根据对象进入队列的次数而延迟等待时间
	// Forget indicates that an item is finished being retried.  Doesn't matter whether its for perm failing
	// or for success, we'll stop tracking it
	Forget(item interface{}) //不再关注指定对象。从表(如果有）移除，不再跟踪对象失败次数
	// NumRequeues returns back how many failures the item has had
	NumRequeues(item interface{}) int //对象失败了多少次(重新进入队列多少次)
}

// DefaultControllerRateLimiter is a no-arg constructor for a default rate limiter for a workqueue.  It has
// both overall and per-item rate limiting.  The overall is a token bucket and the per-item is exponential
func DefaultControllerRateLimiter() RateLimiter {
	//这个限速器由失败重试退避限速器和令牌桶限速器组成，等待延时使用两个限速其中最长延时
	return NewMaxOfRateLimiter(
		NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
		// 10 qps, 100 bucket size.  This is only for retry speed and its only the overall factor (not per item)
		&BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)}, //上限10个，每当消费1个，需要等待100
	)
}

// BucketRateLimiter adapts a standard bucket to the workqueue ratelimiter API
//令牌桶限速器
type BucketRateLimiter struct {
	*rate.Limiter
}

var _ RateLimiter = &BucketRateLimiter{}

//根据桶里的令牌数，以及令牌补充时间来计算等待时间
func (r *BucketRateLimiter) When(item interface{}) time.Duration {
	return r.Limiter.Reserve().Delay()
}

//令牌桶不关心失败次数
func (r *BucketRateLimiter) NumRequeues(item interface{}) int {
	return 0
}

//令牌通不记录对象
func (r *BucketRateLimiter) Forget(item interface{}) {
}

// ItemBucketRateLimiter implements a workqueue ratelimiter API using standard rate.Limiter.
// Each key is using a separate limiter.
type ItemBucketRateLimiter struct {
	r     rate.Limit
	burst int

	limitersLock sync.Mutex
	limiters     map[interface{}]*rate.Limiter
}

var _ RateLimiter = &ItemBucketRateLimiter{}

// NewItemBucketRateLimiter creates new ItemBucketRateLimiter instance.
func NewItemBucketRateLimiter(r rate.Limit, burst int) *ItemBucketRateLimiter {
	return &ItemBucketRateLimiter{
		r:        r,
		burst:    burst,
		limiters: make(map[interface{}]*rate.Limiter),
	}
}

// When returns a time.Duration which we need to wait before item is processed.
func (r *ItemBucketRateLimiter) When(item interface{}) time.Duration {
	r.limitersLock.Lock()
	defer r.limitersLock.Unlock()

	limiter, ok := r.limiters[item]
	if !ok {
		limiter = rate.NewLimiter(r.r, r.burst)
		r.limiters[item] = limiter
	}

	return limiter.Reserve().Delay()
}

// NumRequeues returns always 0 (doesn't apply to ItemBucketRateLimiter).
func (r *ItemBucketRateLimiter) NumRequeues(item interface{}) int {
	return 0
}

// Forget removes item from the internal state.
func (r *ItemBucketRateLimiter) Forget(item interface{}) {
	r.limitersLock.Lock()
	defer r.limitersLock.Unlock()

	delete(r.limiters, item)
}

// ItemExponentialFailureRateLimiter does a simple baseDelay*2^<num-failures> limit
// dealing with max failures and expiration are up to the caller
type ItemExponentialFailureRateLimiter struct {
	failuresLock sync.Mutex
	failures     map[interface{}]int //保存某个对象的失败次数

	baseDelay time.Duration //用来算新的退避时间 baseDelay*2^<num-failures>
	maxDelay  time.Duration //最大延时.(退避时间不能超过改时间)
}

var _ RateLimiter = &ItemExponentialFailureRateLimiter{}

//这个限速器会根据对象的失败次数从基本重试中计算出下一次重试时间（base*2的失败次数次方），直到达到最大重试
//失败重试退避限速器
func NewItemExponentialFailureRateLimiter(baseDelay time.Duration, maxDelay time.Duration) RateLimiter {
	return &ItemExponentialFailureRateLimiter{
		failures:  map[interface{}]int{},
		baseDelay: baseDelay,
		maxDelay:  maxDelay,
	}
}

func DefaultItemBasedRateLimiter() RateLimiter {
	return NewItemExponentialFailureRateLimiter(time.Millisecond, 1000*time.Second)
}

// 根据对象的失败次数来计算出来某对象的下一次执行时间
func (r *ItemExponentialFailureRateLimiter) When(item interface{}) time.Duration {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()
	//失败次数
	exp := r.failures[item]
	r.failures[item] = r.failures[item] + 1

	// The backoff is capped such that 'calculated' value never overflows.
	backoff := float64(r.baseDelay.Nanoseconds()) * math.Pow(2, float64(exp))
	if backoff > math.MaxInt64 {
		return r.maxDelay
	}

	calculated := time.Duration(backoff)
	if calculated > r.maxDelay {
		return r.maxDelay
	}

	return calculated
}

//对象的失败次数
func (r *ItemExponentialFailureRateLimiter) NumRequeues(item interface{}) int {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	return r.failures[item]
}

//直接删除对象的失败次数记录
func (r *ItemExponentialFailureRateLimiter) Forget(item interface{}) {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	delete(r.failures, item)
}

// ItemFastSlowRateLimiter does a quick retry for a certain number of attempts, then a slow retry after that
type ItemFastSlowRateLimiter struct {
	failuresLock sync.Mutex
	failures     map[interface{}]int

	maxFastAttempts int
	fastDelay       time.Duration
	slowDelay       time.Duration
}

var _ RateLimiter = &ItemFastSlowRateLimiter{}

func NewItemFastSlowRateLimiter(fastDelay, slowDelay time.Duration, maxFastAttempts int) RateLimiter {
	return &ItemFastSlowRateLimiter{
		failures:        map[interface{}]int{},
		fastDelay:       fastDelay,
		slowDelay:       slowDelay,
		maxFastAttempts: maxFastAttempts,
	}
}

func (r *ItemFastSlowRateLimiter) When(item interface{}) time.Duration {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	r.failures[item] = r.failures[item] + 1

	if r.failures[item] <= r.maxFastAttempts {
		return r.fastDelay
	}

	return r.slowDelay
}

func (r *ItemFastSlowRateLimiter) NumRequeues(item interface{}) int {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	return r.failures[item]
}

func (r *ItemFastSlowRateLimiter) Forget(item interface{}) {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	delete(r.failures, item)
}

// MaxOfRateLimiter calls every RateLimiter and returns the worst case response
// When used with a token bucket limiter, the burst could be apparently exceeded in cases where particular items
// were separately delayed a longer time.
type MaxOfRateLimiter struct {
	limiters []RateLimiter
}

// 在所有限速器中找到最大延时
func (r *MaxOfRateLimiter) When(item interface{}) time.Duration {
	ret := time.Duration(0)
	for _, limiter := range r.limiters {
		curr := limiter.When(item)
		if curr > ret {
			ret = curr
		}
	}

	return ret
}

func NewMaxOfRateLimiter(limiters ...RateLimiter) RateLimiter {
	return &MaxOfRateLimiter{limiters: limiters}
}

//在所有限速器中找到最高的失败次数
func (r *MaxOfRateLimiter) NumRequeues(item interface{}) int {
	ret := 0
	for _, limiter := range r.limiters {
		curr := limiter.NumRequeues(item)
		if curr > ret {
			ret = curr
		}
	}

	return ret
}

//从所有限速器中，将指定的对象删除
func (r *MaxOfRateLimiter) Forget(item interface{}) {
	for _, limiter := range r.limiters {
		limiter.Forget(item)
	}
}
