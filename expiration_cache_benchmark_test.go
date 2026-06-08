package expirationcache_test

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	expirationcache "github.com/0xERR0R/expiration-cache"
)

// BenchmarkGetParallel measures concurrent cache-hit throughput as the shard
// count grows. Run with increasing -cpu to watch the single-lock ceiling lift:
//
//	go test -run=^$ -bench=BenchmarkGetParallel -benchmem -cpu=1,2,4,8 ./...
//
// To quantify lock contention directly, capture mutex profiles for two configs
// at the same -cpu and compare cumulative contention time:
//
//	go test -run=^$ -bench='BenchmarkGetParallel/shards=1' -cpu=8 -mutexprofile=mutex1.out ./...
//	go test -run=^$ -bench='BenchmarkGetParallel/shards=8' -cpu=8 -mutexprofile=mutex8.out ./...
//	go tool pprof -top mutex1.out
//	go tool pprof -top mutex8.out
func BenchmarkGetParallel(b *testing.B) {
	const numKeys = 10_000

	keys := make([]string, numKeys)
	for i := range keys {
		keys[i] = "key" + strconv.Itoa(i)
	}

	for _, shards := range []uint{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("shards=%d", shards), func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cache := expirationcache.NewCache[int](ctx, expirationcache.Options{
				// give every shard enough slack that uneven hashing never evicts
				// during warm-up, so all shard counts measure a 100% hit ratio
				MaxSize:         numKeys * 2,
				CleanupInterval: time.Hour, // keep the janitor out of the measurement
				Shards:          shards,
			})

			for i := range keys {
				v := i
				cache.Put(keys[i], &v, time.Hour)
			}

			if got := cache.TotalCount(); got != numKeys {
				b.Fatalf("warm-up evicted entries: have %d of %d keys; raise MaxSize", got, numKeys)
			}

			b.ReportAllocs()
			b.ResetTimer()

			var next atomic.Uint64

			b.RunParallel(func(pb *testing.PB) {
				// start each goroutine at a distinct key so they spread across shards
				// instead of contending on the same shard in lockstep
				i := int(next.Add(1)) * 97
				for pb.Next() {
					cache.Get(keys[i%numKeys])
					i++
				}
			})
		})
	}
}
