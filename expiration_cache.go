package expirationcache

import (
	"context"
	"hash/maphash"
	"math"
	"math/bits"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	defaultCleanUpInterval = 10 * time.Second
	defaultSize            = 10_000
	// maxShards bounds the shard count so an absurd Options.Shards value cannot
	// overflow the power-of-two rounding or allocate an unbounded number of LRUs.
	maxShards = 1 << 16
)

type element[T any] struct {
	val            *T
	expiresEpochMs int64
}

// ExpirationLRUCache is an LRU cache with per-item expiration and optional callbacks.
// It is safe for concurrent use by multiple goroutines.
type ExpirationLRUCache[T any] struct {
	cleanUpInterval time.Duration
	preExpirationFn OnExpirationCallback[T]
	onCacheHit      OnCacheHitCallback
	onCacheMiss     OnCacheMissCallback
	onAfterPut      OnAfterPutCallback
	shards          []*lru.Cache[string, *element[T]]
	seed            maphash.Seed
	mask            uint64
	count           atomic.Int64
}

// Options configures the behavior of ExpirationLRUCache.
//
// OnCacheHitFn: Optional callback invoked when a cache hit occurs.
// OnCacheMissFn: Optional callback invoked when a cache miss occurs.
// OnAfterPutFn: Optional callback invoked after a new item is put in the cache.
// CleanupInterval: How often expired items are cleaned up (default 10s).
// MaxSize: Maximum total number of items across all shards (default 10,000). The
// capacity is divided across shards so the total never exceeds MaxSize; with more
// than one shard, eviction is per-shard, so a hot shard may evict entries while
// others still have room and the effective capacity is approximate.
type Options struct {
	OnCacheHitFn    OnCacheHitCallback
	OnCacheMissFn   OnCacheMissCallback
	OnAfterPutFn    OnAfterPutCallback
	CleanupInterval time.Duration
	MaxSize         uint
	// Shards sets the number of independent LRU shards the cache is split into,
	// rounded up to a power of two (and clamped to a sane maximum). Keys are routed
	// to a shard by hash, so reads and writes on different shards never contend on
	// the same lock. 0 or 1 means a single shard (no sharding) and matches
	// pre-sharding behavior; this is the default.
	//
	// Sharding only improves throughput when multiple goroutines run in parallel on
	// multiple CPUs, because Get takes a write lock to update LRU recency. On a
	// single CPU it yields no benefit and slightly degrades eviction quality, so
	// leave it at the default unless GOMAXPROCS > 1; a value near GOMAXPROCS is a
	// reasonable starting point.
	Shards uint
}

// OnExpirationCallback will be called just before an element gets expired and will
// be removed from cache. This function can return new value and TTL to leave the
// element in the cache or nil to remove it
type OnExpirationCallback[T any] func(ctx context.Context, key string) (val *T, ttl time.Duration)

// OnCacheHitCallback will be called on cache get if entry was found
type OnCacheHitCallback func(key string)

// OnCacheMissCallback will be called on cache get and entry was not found
type OnCacheMissCallback func(key string)

// OnAfterPutCallback will be called after put, receives new element count as parameter
type OnAfterPutCallback func(newSize int)

// NewCache creates a new ExpirationLRUCache with the given options.
// The cache is safe for concurrent use by multiple goroutines.
//
// ctx: Context for controlling the lifetime of the background cleanup goroutine.
// options: Configuration for cache behavior.
func NewCache[T any](ctx context.Context, options Options) *ExpirationLRUCache[T] {
	return NewCacheWithOnExpired[T](ctx, options, nil)
}

// NewCacheWithOnExpired creates a new ExpirationLRUCache with the given options and a custom expiration callback.
// The cache is safe for concurrent use by multiple goroutines.
//
// ctx: Context for controlling the lifetime of the background cleanup goroutine.
// options: Configuration for cache behavior.
// onExpirationFn: Callback invoked before an item expires; can return a new value and TTL to keep the item alive.
func NewCacheWithOnExpired[T any](ctx context.Context, options Options,
	onExpirationFn OnExpirationCallback[T],
) *ExpirationLRUCache[T] {
	size := defaultSize
	if options.MaxSize > 0 {
		// guard against MaxSize > MaxInt overflowing to a negative size
		if options.MaxSize > uint(math.MaxInt) {
			size = math.MaxInt
		} else {
			size = int(options.MaxSize)
		}
	}

	// clamp the requested shard count before rounding so nextPow2 cannot overflow
	requested := options.Shards
	if requested > maxShards {
		requested = maxShards
	}

	shardCount := nextPow2(requested)
	// never create more shards than the cache can hold items, so every shard gets
	// at least one slot and the per-shard capacities still sum to exactly size.
	if shardCount > size {
		shardCount = floorPow2(size)
	}

	c := &ExpirationLRUCache[T]{
		cleanUpInterval: defaultCleanUpInterval,
		preExpirationFn: func(ctx context.Context, key string) (val *T, ttl time.Duration) {
			return nil, 0
		},
		onCacheHit:  func(key string) {},
		onCacheMiss: func(key string) {},
		onAfterPut:  func(newSize int) {},
		seed:        maphash.MakeSeed(),
		mask:        uint64(shardCount - 1),
	}

	// Distribute the global capacity across shards so the per-shard limits sum to
	// exactly size (no rounding overshoot): the first `remainder` shards get one
	// extra slot. The eviction callback keeps the live counter in sync.
	base := size / shardCount
	remainder := size % shardCount

	// Saturating decrement: the count is also reconciled by cleanUp via
	// count.Store(liveCount()). If such a Store snapshots the counter between a
	// Put's increment and the matching eviction here, a plain Add(-1) could cross
	// below zero. Flooring at zero keeps count >= 0 an invariant; the rare dropped
	// decrement self-corrects at the next reconcile.
	onEvict := func(_ string, _ *element[T]) {
		for {
			v := c.count.Load()
			if v <= 0 {
				return
			}

			if c.count.CompareAndSwap(v, v-1) {
				return
			}
		}
	}

	shards := make([]*lru.Cache[string, *element[T]], shardCount)
	for i := range shards {
		capacity := base
		if i < remainder {
			capacity++
		}

		l, err := lru.NewWithEvict[string, *element[T]](capacity, onEvict)
		if err != nil {
			// capacity is guaranteed >= 1, so this is unreachable; fall back to a
			// minimal shard rather than storing a nil cache that would panic later.
			l, _ = lru.NewWithEvict[string, *element[T]](1, onEvict)
		}

		shards[i] = l
	}

	c.shards = shards

	if options.CleanupInterval > 0 {
		c.cleanUpInterval = options.CleanupInterval
	}

	if options.OnAfterPutFn != nil {
		c.onAfterPut = options.OnAfterPutFn
	}

	if options.OnCacheHitFn != nil {
		c.onCacheHit = options.OnCacheHitFn
	}

	if options.OnCacheMissFn != nil {
		c.onCacheMiss = options.OnCacheMissFn
	}

	if onExpirationFn != nil {
		c.preExpirationFn = onExpirationFn
	}

	go periodicCleanup(ctx, c)

	return c
}

// nextPow2 returns the smallest power of two >= n, and at least 1. n must be
// bounded (see maxShards) so the shift cannot overflow.
func nextPow2(n uint) int {
	if n <= 1 {
		return 1
	}

	return 1 << bits.Len(n-1)
}

// floorPow2 returns the largest power of two <= n, and at least 1.
func floorPow2(n int) int {
	if n < 2 {
		return 1
	}

	return 1 << (bits.Len(uint(n)) - 1)
}

// shard returns the LRU shard that owns key.
func (e *ExpirationLRUCache[T]) shard(key string) *lru.Cache[string, *element[T]] {
	// single-shard caches route everything to shard 0; skip hashing the key.
	if e.mask == 0 {
		return e.shards[0]
	}

	return e.shards[maphash.String(e.seed, key)&e.mask]
}

// liveCount sums the live element count across all shards. It is O(shards) and is
// only used to reconcile the cached counter during cleanup.
func (e *ExpirationLRUCache[T]) liveCount() (count int) {
	for _, shard := range e.shards {
		count += shard.Len()
	}

	return count
}

func periodicCleanup[T any](ctx context.Context, c *ExpirationLRUCache[T]) {
	ticker := time.NewTicker(c.cleanUpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanUp()
		case <-ctx.Done():
			return
		}
	}
}

func (e *ExpirationLRUCache[T]) cleanUp() {
	type expiredEntry struct {
		shard *lru.Cache[string, *element[T]]
		key   string
	}

	var expired []expiredEntry

	// collect expired entries together with the shard that owns them, so the
	// delete pass below does not have to re-hash the key to find the shard again.
	for _, shard := range e.shards {
		for _, k := range shard.Keys() {
			v, ok := shard.Peek(k)
			if !ok {
				continue
			}

			if !isExpired(v) {
				continue
			}

			expired = append(expired, expiredEntry{shard: shard, key: k})
		}
	}

	if len(expired) == 0 {
		return
	}

	for _, ee := range expired {
		newVal, newTTL := e.preExpirationFn(context.Background(), ee.key)
		if newVal != nil && newTTL > 0 {
			e.Put(ee.key, newVal, newTTL)

			continue
		}

		// Only remove if the entry is still expired: a concurrent Put may have
		// refreshed it between classification and now (Peek does not change recency).
		// A non-nil value with a non-positive TTL also falls through to removal here,
		// so the stale entry is never left behind.
		if v, ok := ee.shard.Peek(ee.key); ok && isExpired(v) {
			ee.shard.Remove(ee.key)
		}
	}

	// Reconcile the counter with the exact live count, bounding any drift from
	// concurrent same-key inserts to a single cleanup interval.
	e.count.Store(int64(e.liveCount()))
}

// Put adds a value to the cache with the specified key and TTL (time-to-live).
// If ttl <= 0, the entry is not added.
//
// key: The cache key.
// val: Pointer to the value to store.
// ttl: Duration before the item expires.
func (e *ExpirationLRUCache[T]) Put(key string, val *T, ttl time.Duration) {
	if ttl <= 0 {
		// entry should be considered as already expired
		return
	}

	expiresEpochMs := time.Now().UnixMilli() + ttl.Milliseconds()

	shard := e.shard(key)

	// Track the live count incrementally so callbacks and TotalCount stay O(1)
	// instead of summing every shard. A brand-new key adds one; updating an
	// existing key adds nothing, and eviction at capacity is balanced by the
	// eviction callback. cleanUp periodically reconciles any drift.
	if !shard.Contains(key) {
		e.count.Add(1)
	}

	// add new item
	shard.Add(key, &element[T]{
		val:            val,
		expiresEpochMs: expiresEpochMs,
	})

	e.onAfterPut(int(e.count.Load()))
}

// Get retrieves a value from the cache by key. Can return already expired value.
// Returns the value pointer and remaining TTL if found, or (nil, 0) if not found.
//
// key: The cache key.
func (e *ExpirationLRUCache[T]) Get(key string) (val *T, ttl time.Duration) {
	el, found := e.shard(key).Get(key)

	if found {
		e.onCacheHit(key)

		return el.val, calculateRemainTTL(el.expiresEpochMs)
	}

	e.onCacheMiss(key)

	return nil, 0
}

func isExpired[T any](el *element[T]) bool {
	return el.expiresEpochMs > 0 && time.Now().UnixMilli() > el.expiresEpochMs
}

func calculateRemainTTL(expiresEpoch int64) time.Duration {
	if now := time.Now().UnixMilli(); now < expiresEpoch {
		return time.Duration(expiresEpoch-now) * time.Millisecond
	}

	return 0
}

// Remove deletes the entry with the given key from the cache. Removing a key
// that is not present is a no-op. The live counter is kept in sync by the LRU's
// eviction callback, so TotalCount reflects the removal.
//
// key: The cache key to remove.
func (e *ExpirationLRUCache[T]) Remove(key string) {
	e.shard(key).Remove(key)
}

// TotalCount returns the current number of items in the cache.
func (e *ExpirationLRUCache[T]) TotalCount() int {
	return int(e.count.Load())
}

// Clear removes all items from the cache.
func (e *ExpirationLRUCache[T]) Clear() {
	for _, shard := range e.shards {
		shard.Purge()
	}

	e.count.Store(0)
}
