package expirationcache

import (
	"context"
	"hash/maphash"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	defaultCleanUpInterval = 10 * time.Second
	defaultSize            = 10_000
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
}

// Options configures the behavior of ExpirationLRUCache.
//
// OnCacheHitFn: Optional callback invoked when a cache hit occurs.
// OnCacheMissFn: Optional callback invoked when a cache miss occurs.
// OnAfterPutFn: Optional callback invoked after a new item is put in the cache.
// CleanupInterval: How often expired items are cleaned up (default 10s).
// MaxSize: Maximum number of items in the cache (default 10,000).
type Options struct {
	OnCacheHitFn    OnCacheHitCallback
	OnCacheMissFn   OnCacheMissCallback
	OnAfterPutFn    OnAfterPutCallback
	CleanupInterval time.Duration
	MaxSize         uint
	// Shards sets the number of independent LRU shards the cache is split into,
	// rounded up to a power of two. Keys are routed to a shard by hash, so reads
	// on different shards never contend on the same lock. 0 means a single shard
	// (no sharding) and matches pre-sharding behavior. This is the default.
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
		size = int(options.MaxSize)
	}

	shardCount := nextPow2(options.Shards)

	perShard := (size + shardCount - 1) / shardCount
	if perShard < 1 {
		perShard = 1
	}

	shards := make([]*lru.Cache[string, *element[T]], shardCount)
	for i := range shards {
		l, _ := lru.New[string, *element[T]](perShard)
		shards[i] = l
	}

	c := &ExpirationLRUCache[T]{
		cleanUpInterval: defaultCleanUpInterval,
		preExpirationFn: func(ctx context.Context, key string) (val *T, ttl time.Duration) {
			return nil, 0
		},
		onCacheHit:  func(key string) {},
		onCacheMiss: func(key string) {},
		shards:      shards,
		seed:        maphash.MakeSeed(),
		mask:        uint64(shardCount - 1),
	}

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

// nextPow2 returns the smallest power of two >= n, and at least 1.
func nextPow2(n uint) int {
	if n <= 1 {
		return 1
	}

	p := 1
	for uint(p) < n {
		p <<= 1
	}

	return p
}

// shard returns the LRU shard that owns key.
func (e *ExpirationLRUCache[T]) shard(key string) *lru.Cache[string, *element[T]] {
	return e.shards[maphash.String(e.seed, key)&e.mask]
}

// totalCount sums the live element count across all shards.
func (e *ExpirationLRUCache[T]) totalCount() (count int) {
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
	var expiredKeys []string

	// check every shard for expired items and collect expired keys
	for _, shard := range e.shards {
		for _, k := range shard.Keys() {
			if v, ok := shard.Peek(k); ok {
				if isExpired(v) {
					expiredKeys = append(expiredKeys, k)
				}
			}
		}
	}

	if len(expiredKeys) > 0 {
		var keysToDelete []string

		for _, key := range expiredKeys {
			newVal, newTTL := e.preExpirationFn(context.Background(), key)
			if newVal != nil {
				e.Put(key, newVal, newTTL)
			} else {
				keysToDelete = append(keysToDelete, key)
			}
		}

		for _, key := range keysToDelete {
			e.shard(key).Remove(key)
		}
	}
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

	// add new item
	e.shard(key).Add(key, &element[T]{
		val:            val,
		expiresEpochMs: expiresEpochMs,
	})

	if e.onAfterPut != nil {
		e.onAfterPut(e.totalCount())
	}
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

// TotalCount returns the current number of items in the cache.
func (e *ExpirationLRUCache[T]) TotalCount() (count int) {
	return e.totalCount()
}

// Clear removes all items from the cache.
func (e *ExpirationLRUCache[T]) Clear() {
	for _, shard := range e.shards {
		shard.Purge()
	}
}
