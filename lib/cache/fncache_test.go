/*
Copyright 2021 Gravitational, Inc.

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

package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

var _ = fmt.Printf

// TestFnCacheSanity runs basic fnCache test cases.
func TestFnCacheSanity(t *testing.T) {
	tts := []struct {
		ttl   time.Duration
		delay time.Duration
		desc  string
	}{
		{ttl: time.Millisecond * 40, delay: time.Millisecond * 20, desc: "long ttl, short delay"},
		{ttl: time.Millisecond * 20, delay: time.Millisecond * 40, desc: "short ttl, long delay"},
		{ttl: time.Millisecond * 40, delay: time.Millisecond * 40, desc: "long ttl, long delay"},
		{ttl: time.Millisecond * 40, delay: 0, desc: "non-blocking"},
	}

	for _, tt := range tts {
		testFnCacheSimple(t, tt.ttl, tt.delay, "ttl=%s, delay=%s, desc=%q", tt.ttl, tt.delay, tt.desc)
	}
}

// testFnCacheSimple runs a basic test case which spams concurrent request against a cache
// and verifies that the resulting hit/miss numbers roughly match our expectation.
func testFnCacheSimple(t *testing.T, ttl time.Duration, delay time.Duration, msg ...interface{}) {
	const rate = int64(20)     // get attempts per worker per ttl period
	const workers = int64(100) // number of concurrent workers
	const rounds = int64(10)   // number of full ttl cycles to go through

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cache := newFnCache(ttl)

	// readCounter is incremented upon each cache miss.
	readCounter := atomic.NewInt64(0)

	// getCounter is incremented upon each get made against the cache, hit or miss.
	getCounter := atomic.NewInt64(0)

	readTime := make(chan time.Time, 1)

	var wg sync.WaitGroup

	// spawn workers
	for w := int64(0); w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(ttl / time.Duration(rate))
			defer ticker.Stop()
			done := time.After(ttl * time.Duration(rounds))
			lastValue := int64(0)
			for {
				select {
				case <-ticker.C:
				case <-done:
					return
				}
				vi, err := cache.Get(ctx, "key", func() (interface{}, error) {
					if delay > 0 {
						<-time.After(delay)
					}

					select {
					case readTime <- time.Now():
					default:
					}

					val := readCounter.Inc()
					return val, nil
				})
				require.NoError(t, err, msg...)
				require.GreaterOrEqual(t, vi.(int64), lastValue, msg...)
				lastValue = vi.(int64)
				getCounter.Inc()
			}
		}()
	}

	startTime := <-readTime

	// wait for workers to finish
	wg.Wait()

	elapsed := time.Since(startTime)

	// approxReads is the approximate expected number of reads
	approxReads := float64(elapsed) / float64(ttl+delay)

	// verify that number of actual reads is within +/- 1 of the number of expected reads.
	require.InDelta(t, approxReads, readCounter.Load(), 1, msg...)

	// verify that we're performing approximately workers*rate*rounds get operations when unblocked. significant
	// deviation might indicate a performance issue, or a programming error in the test.
	//require.InDelta(t, float64(rounds)*portionUnblocked, float64(getCounter.Load())/float64(workers*rate), 1.5, msg...)
}

// TestFnCacheCancellation verifies expected cancellation behavior.  Specifically, we expect that
// in-progress loading continues, and the entry is correctly updated, even if the call to Get
// which happened to trigger the load needs to be unblocked early.
func TestFnCacheCancellation(t *testing.T) {
	const timeout = time.Millisecond * 10

	cache := newFnCache(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	blocker := make(chan struct{})

	v, err := cache.Get(ctx, "key", func() (interface{}, error) {
		<-blocker
		return "val", nil
	})

	require.Nil(t, v)
	require.Equal(t, context.DeadlineExceeded, trace.Unwrap(err))

	// unblock the loading operation which is still in progress
	close(blocker)

	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	defer cancel()

	v, err = cache.Get(ctx, "key", func() (interface{}, error) {
		panic("this should never run!")
	})

	require.NoError(t, err)
	require.Equal(t, "val", v.(string))
}
