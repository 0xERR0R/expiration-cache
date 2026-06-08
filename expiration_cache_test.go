package expirationcache

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Expiration cache", func() {
	var (
		ctx      context.Context
		cancelFn context.CancelFunc
	)
	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)
	})
	Describe("Sharding", func() {
		It("rounds the shard count up to a power of two", func() {
			cache := NewCache[string](ctx, Options{Shards: 5})
			Expect(cache.shards).Should(HaveLen(8))
		})

		It("defaults to a single shard", func() {
			cache := NewCache[string](ctx, Options{})
			Expect(cache.shards).Should(HaveLen(1))
		})

		It("routes a key to the same shard every time", func() {
			cache := NewCache[string](ctx, Options{Shards: 8})
			Expect(cache.shard("some-key")).Should(BeIdenticalTo(cache.shard("some-key")))
		})

		It("spreads keys across more than one shard", func() {
			cache := NewCache[int](ctx, Options{Shards: 16, MaxSize: 10000})
			for i := range 1000 {
				v := i
				cache.Put(fmt.Sprintf("key%d", i), &v, time.Minute)
			}

			used := 0
			for _, s := range cache.shards {
				if s.Len() > 0 {
					used++
				}
			}

			// 1000 keys across 16 shards reach every shard with overwhelming probability
			Expect(used).Should(Equal(len(cache.shards)))
			Expect(cache.TotalCount()).Should(Equal(1000))
		})

		It("clamps an absurd shard count instead of hanging or panicking", func() {
			// a huge Shards value must not overflow the power-of-two rounding loop
			cache := NewCache[int](ctx, Options{Shards: math.MaxUint, MaxSize: 1000})
			Expect(len(cache.shards)).Should(And(
				BeNumerically(">", 0),
				BeNumerically("<=", 1000)))

			v := 1
			cache.Put("k", &v, time.Minute)
			got, _ := cache.Get("k")
			Expect(got).Should(HaveValue(Equal(1)))
		})

		It("does not collapse capacity when MaxSize overflows int", func() {
			// MaxSize > MaxInt must clamp to a large positive size, not a negative one
			cache := NewCache[int](ctx, Options{MaxSize: math.MaxUint})
			for i := range 1000 {
				v := i
				cache.Put(fmt.Sprintf("key%d", i), &v, time.Minute)
			}

			Expect(cache.TotalCount()).Should(Equal(1000))
		})

		It("never creates more shards than the capacity allows", func() {
			// 16 shards over a 4-item cache would leave shards with zero capacity
			cache := NewCache[int](ctx, Options{Shards: 16, MaxSize: 4})
			Expect(len(cache.shards)).Should(BeNumerically("<=", 4))

			for i := range 100 {
				v := i
				cache.Put(fmt.Sprintf("key%d", i), &v, time.Minute)
			}

			Expect(cache.TotalCount()).Should(BeNumerically("<=", 4))
		})

		It("keeps total entries within the global cap across shards", func() {
			// 8 does not divide 100, so capacity is distributed as 4 shards of 13 and
			// 4 of 12 (= 100). The total must never exceed MaxSize despite the rounding.
			cache := NewCache[int](ctx, Options{Shards: 8, MaxSize: 100})
			for i := range 1000 {
				v := i
				cache.Put(fmt.Sprintf("key%d", i), &v, time.Minute)
			}

			Expect(cache.TotalCount()).Should(BeNumerically("<=", 100))
		})

		It("revives expired entries across shards via preExpirationFn", func() {
			var reloaded atomic.Int32
			reload := func(_ context.Context, _ string) (*string, time.Duration) {
				reloaded.Add(1)
				v := "reloaded"

				return &v, time.Minute
			}

			cache := NewCacheWithOnExpired[string](ctx,
				Options{Shards: 8, CleanupInterval: 50 * time.Millisecond}, reload)
			for i := range 20 {
				v := "v"
				cache.Put(fmt.Sprintf("key%d", i), &v, 20*time.Millisecond)
			}

			Eventually(func() int32 {
				return reloaded.Load()
			}, "1s").Should(Equal(int32(20)))

			// the entries must actually be revived in their shards, not just the
			// callback fired: every key should now hold the reloaded value
			for i := range 20 {
				val, ttl := cache.Get(fmt.Sprintf("key%d", i))
				Expect(val).Should(HaveValue(Equal("reloaded")))
				Expect(ttl).Should(BeNumerically(">", time.Duration(0)))
			}
			Expect(cache.TotalCount()).Should(Equal(20))
		})

		It("never reports a negative count under concurrent puts, evictions and cleanup", func() {
			// A small capacity forces constant eviction while a tight cleanup
			// interval reconciles the counter concurrently. If the eviction
			// decrement were a plain Add(-1), a reconcile snapshot landing between a
			// Put's increment and its matching eviction could drive count below zero.
			cache := NewCache[int](ctx, Options{
				Shards:          8,
				MaxSize:         64,
				CleanupInterval: time.Millisecond,
			})

			var wg sync.WaitGroup
			var negatives atomic.Int32

			for g := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range 5000 {
						v := i
						cache.Put(fmt.Sprintf("g%d-key%d", g, i), &v, time.Minute)
						if cache.TotalCount() < 0 {
							negatives.Add(1)
						}
					}
				}()
			}

			wg.Wait()

			Expect(negatives.Load()).Should(BeZero())
			Expect(cache.TotalCount()).Should(And(
				BeNumerically(">=", 0),
				BeNumerically("<=", 64)))
		})
	})
	Describe("Basic operations", func() {
		When("string cache was created", func() {
			It("Initial cache should be empty", func() {
				cache := NewCache[string](ctx, Options{})
				Expect(cache.TotalCount()).Should(Equal(0))
			})
			It("Initial cache should not contain any elements", func() {
				cache := NewCache[string](ctx, Options{})
				val, expiration := cache.Get("key1")
				Expect(val).Should(BeNil())
				Expect(expiration).Should(Equal(time.Duration(0)))
			})
		})
		When("Put new value with positive TTL", func() {
			It("Should return the value before element expires", func() {
				cache := NewCache[string](ctx, Options{CleanupInterval: 100 * time.Millisecond})
				v := "v1"
				cache.Put("key1", &v, 50*time.Millisecond)
				val, expiration := cache.Get("key1")
				Expect(val).Should(HaveValue(Equal("v1")))
				Expect(expiration.Milliseconds()).Should(BeNumerically("<=", 50))

				Expect(cache.TotalCount()).Should(Equal(1))
			})
			It("Should return nil after expiration", func() {
				cache := NewCache[string](ctx, Options{CleanupInterval: 100 * time.Millisecond})
				v := "v1"
				cache.Put("key1", &v, 50*time.Millisecond)

				// wait for expiration
				Eventually(func(g Gomega) {
					val, ttl := cache.Get("key1")
					g.Expect(val).Should(HaveValue(Equal("v1")))
					g.Expect(ttl.Milliseconds()).Should(BeNumerically("==", 0))
				}, "100ms").Should(Succeed())

				// wait for cleanup run
				Eventually(func() int {
					return cache.TotalCount()
				}).Should(Equal(0))
			})
		})
		When("Put new value without expiration", func() {
			It("Should not cache the value", func() {
				cache := NewCache[string](ctx, Options{CleanupInterval: 50 * time.Millisecond})
				v := "x"
				cache.Put("key1", &v, 0)
				val, expiration := cache.Get("key1")
				Expect(val).Should(BeNil())
				Expect(expiration.Milliseconds()).Should(BeNumerically("==", 0))
				Expect(cache.TotalCount()).Should(Equal(0))
			})
		})
		When("Put updated value", func() {
			It("Should return updated value", func() {
				cache := NewCache[string](ctx, Options{})
				v1 := "v1"
				v2 := "v2"
				cache.Put("key1", &v1, 50*time.Millisecond)
				cache.Put("key1", &v2, 200*time.Millisecond)

				val, expiration := cache.Get("key1")

				Expect(val).Should(HaveValue(Equal("v2")))
				Expect(expiration.Milliseconds()).Should(BeNumerically(">", 100))
				Expect(expiration.Milliseconds()).Should(BeNumerically("<=", 200))
				Expect(cache.TotalCount()).Should(Equal(1))
			})
		})
		When("Purging after usage", func() {
			It("Should be empty after purge", func() {
				cache := NewCache[string](ctx, Options{})
				v1 := "y"
				cache.Put("key1", &v1, time.Second)

				Expect(cache.TotalCount()).Should(Equal(1))

				cache.Clear()

				Expect(cache.TotalCount()).Should(Equal(0))
			})
		})
		When("Removing a single key", func() {
			It("should delete the entry and decrement the count", func() {
				cache := NewCache[string](ctx, Options{})
				v1 := "v1"
				v2 := "v2"
				cache.Put("key1", &v1, time.Second)
				cache.Put("key2", &v2, time.Second)

				Expect(cache.TotalCount()).Should(Equal(2))

				cache.Remove("key1")

				val, _ := cache.Get("key1")
				Expect(val).Should(BeNil())
				// the other key is untouched
				val2, _ := cache.Get("key2")
				Expect(val2).Should(HaveValue(Equal("v2")))
				Expect(cache.TotalCount()).Should(Equal(1))
			})
			It("should be a no-op for a missing key", func() {
				cache := NewCache[string](ctx, Options{})
				v1 := "v1"
				cache.Put("key1", &v1, time.Second)

				cache.Remove("does-not-exist")

				val, _ := cache.Get("key1")
				Expect(val).Should(HaveValue(Equal("v1")))
				Expect(cache.TotalCount()).Should(Equal(1))
			})
			It("removes keys routed to different shards", func() {
				cache := NewCache[int](ctx, Options{Shards: 8, MaxSize: 1000})
				for i := range 100 {
					v := i
					cache.Put(fmt.Sprintf("key%d", i), &v, time.Minute)
				}

				Expect(cache.TotalCount()).Should(Equal(100))

				for i := range 100 {
					cache.Remove(fmt.Sprintf("key%d", i))
				}

				Expect(cache.TotalCount()).Should(Equal(0))
				val, _ := cache.Get("key42")
				Expect(val).Should(BeNil())
			})
		})
		When("Adding value with negative TTL", func() {
			It("should not store value with negative TTL", func() {
				v := "neg"
				cache := NewCache[string](ctx, Options{})
				cache.Put("neg", &v, -time.Second)
				val, _ := cache.Get("neg")
				Expect(val).Should(BeNil())
			})
		})
		When("Adding value with very large TTL", func() {
			It("should store value with very large TTL", func() {
				v := "large"
				cache := NewCache[string](ctx, Options{})
				cache.Put("large", &v, 100*365*24*time.Hour) // 100 years
				val, _ := cache.Get("large")
				Expect(val).Should(HaveValue(Equal("large")))
			})
		})
		When("Using empty string key", func() {
			It("should handle empty string key", func() {
				v := "empty"
				cache := NewCache[string](ctx, Options{})
				cache.Put("", &v, time.Second)
				val, _ := cache.Get("")
				Expect(val).Should(HaveValue(Equal("empty")))
			})
		})
		When("Adding a nil value", func() {
			It("should not panic on nil value", func() {
				cache := NewCache[string](ctx, Options{})
				cache.Put("nil", nil, time.Second)
				val, _ := cache.Get("nil")
				Expect(val).Should(BeNil())
			})
		})
	})
	Describe("Hook functions", func() {
		When("Hook functions are defined", func() {
			It("should call each hook function", func() {
				onCacheHitChannel := make(chan string, 10)
				onCacheMissChannel := make(chan string, 10)
				onAfterPutChannel := make(chan int, 10)
				cache := NewCache[string](ctx, Options{
					OnCacheHitFn: func(key string) {
						onCacheHitChannel <- key
					},
					OnCacheMissFn: func(key string) {
						onCacheMissChannel <- key
					},
					OnAfterPutFn: func(newSize int) {
						onAfterPutChannel <- newSize
					},
				})

				By("Get non existing value", func() {
					val, _ := cache.Get("notExists")
					Expect(val).Should(BeNil())

					Expect(onCacheMissChannel).Should(Receive(Equal("notExists")))
					Expect(onCacheHitChannel).ShouldNot(Receive())
					Expect(onAfterPutChannel).ShouldNot(Receive())
				})

				By("Put new cache entry", func() {
					v1 := "v1"
					cache.Put("key1", &v1, time.Second)
					Expect(onCacheMissChannel).ShouldNot(Receive())
					Expect(onCacheMissChannel).ShouldNot(Receive())
					Expect(onAfterPutChannel).Should(Receive(Equal(1)))
				})

				By("Get existing value", func() {
					val, _ := cache.Get("key1")
					Expect(val).Should(HaveValue(Equal("v1")))

					Expect(onCacheMissChannel).ShouldNot(Receive())
					Expect(onCacheHitChannel).Should(Receive(Equal("key1")))
					Expect(onAfterPutChannel).ShouldNot(Receive())
				})
			})
		})
	})
	Describe("preExpiration function", func() {
		When("function is defined", func() {
			It("should update the value and TTL if function returns values", func() {
				fn := func(ctx context.Context, key string) (val *string, ttl time.Duration) {
					v2 := "v2"

					return &v2, time.Second
				}

				cache := NewCacheWithOnExpired[string](ctx, Options{}, fn)
				v1 := "v1"
				cache.Put("key1", &v1, 50*time.Millisecond)

				// wait for expiration
				Eventually(func(g Gomega) {
					val, ttl := cache.Get("key1")
					g.Expect(val).Should(HaveValue(Equal("v1")))
					g.Expect(ttl.Milliseconds()).Should(
						BeNumerically("==", 0))
				}, "150ms").Should(Succeed())
			})

			It("should update the value and TTL if function returns values on cleanup if element is expired", func() {
				fn := func(ctx context.Context, key string) (val *string, ttl time.Duration) {
					v2 := "val2"

					return &v2, time.Second
				}
				cache := NewCacheWithOnExpired[string](ctx, Options{}, fn)
				v1 := "somval"
				cache.Put("key1", &v1, time.Millisecond)

				time.Sleep(2 * time.Millisecond)

				// trigger cleanUp manually -> onExpiredFn will be executed, because element is expired
				cache.cleanUp()

				// wait for expiration
				val, ttl := cache.Get("key1")
				Expect(val).Should(HaveValue(Equal("val2")))
				Expect(ttl.Milliseconds()).Should(And(
					BeNumerically(">", 900),
					BeNumerically("<=", 1000)))
			})

			It("should delete the key if function returns nil", func() {
				fn := func(ctx context.Context, key string) (val *string, ttl time.Duration) {
					return nil, 0
				}
				cache := NewCacheWithOnExpired[string](ctx, Options{CleanupInterval: 100 * time.Microsecond}, fn)
				v1 := "z"
				cache.Put("key1", &v1, 50*time.Millisecond)

				Eventually(func() (interface{}, time.Duration) {
					return cache.Get("key1")
				}, "200ms").Should(BeNil())
			})
		})
	})
	Describe("LRU behaviour", func() {
		When("Defined max size is reached", func() {
			It("should remove old elements", func() {
				cache := NewCache[string](ctx, Options{MaxSize: 3})

				v1 := "val1"
				v2 := "val2"
				v3 := "val3"
				v4 := "val4"
				v5 := "val5"

				cache.Put("key1", &v1, time.Second)
				cache.Put("key2", &v2, time.Second)
				cache.Put("key3", &v3, time.Second)
				cache.Put("key4", &v4, time.Second)

				Expect(cache.TotalCount()).Should(Equal(3))

				// key1 was removed
				Expect(cache.Get("key1")).Should(BeNil())
				// key2,3,4 still in the cache
				Expect(cache.shard("key2").Contains("key2")).Should(BeTrue())
				Expect(cache.shard("key3").Contains("key3")).Should(BeTrue())
				Expect(cache.shard("key4").Contains("key4")).Should(BeTrue())

				// now get key2 to increase usage count
				_, _ = cache.Get("key2")

				// put key5
				cache.Put("key5", &v5, time.Second)

				// now key3 should be removed
				Expect(cache.shard("key2").Contains("key2")).Should(BeTrue())
				Expect(cache.shard("key3").Contains("key3")).Should(BeFalse())
				Expect(cache.shard("key4").Contains("key4")).Should(BeTrue())
				Expect(cache.shard("key5").Contains("key5")).Should(BeTrue())
			})
		})
	})
	Describe("Concurrency", func() {
		It("should be safe for concurrent access", func() {
			cache := NewCache[int](ctx, Options{})
			done := make(chan struct{})
			go func() {
				for i := range 1000 {
					v := i
					cache.Put(fmt.Sprintf("k%d", i), &v, time.Second)
				}
				done <- struct{}{}
			}()
			go func() {
				for i := range 1000 {
					cache.Get(fmt.Sprintf("k%d", i))
				}
				done <- struct{}{}
			}()
			go func() {
				for _ = range 1000 {
					cache.Clear()
				}
				done <- struct{}{}
			}()
			<-done
			<-done
			<-done // wait for all goroutines
			// No assertion needed: test will fail on panic or race
		})
	})
})

func BenchmarkPutGet(b *testing.B) {
	cache := NewCache[int](context.Background(), Options{MaxSize: 100_000})
	for i := 0; i < b.N; i++ {
		v := i
		cache.Put(fmt.Sprintf("k%d", i), &v, time.Minute)
		cache.Get(fmt.Sprintf("k%d", i))
	}
}
