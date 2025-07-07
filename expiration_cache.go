package expirationcache

import (
	"context"
	"time"

	lru "github.com/hashicorp/golang-lru"
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
	lru             *lru.Cache
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
	l, _ := lru.New(defaultSize)
	c := &ExpirationLRUCache[T]{
		cleanUpInterval: defaultCleanUpInterval,
		preExpirationFn: func(ctx context.Context, key string) (val *T, ttl time.Duration) {
			return nil, 0
		},
		onCacheHit:  func(key string) {},
		onCacheMiss: func(key string) {},
		lru:         l,
	}

	if options.CleanupInterval > 0 {
		c.cleanUpInterval = options.CleanupInterval
	}

	if options.MaxSize > 0 {
		l, _ := lru.New(int(options.MaxSize))
		c.lru = l
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

	// check for expired items and collect expired keys
	for _, k := range e.lru.Keys() {
		if v, ok := e.lru.Peek(k); ok {
			if isExpired(v.(*element[T])) {
				expiredKeys = append(expiredKeys, k.(string))
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
			e.lru.Remove(key)
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
	e.lru.Add(key, &element[T]{
		val:            val,
		expiresEpochMs: expiresEpochMs,
	})

	if e.onAfterPut != nil {
		e.onAfterPut(e.lru.Len())
	}
}

// Get retrieves a value from the cache by key. Can return already expired value.
// Returns the value pointer and remaining TTL if found, or (nil, 0) if not found.
//
// key: The cache key.
func (e *ExpirationLRUCache[T]) Get(key string) (val *T, ttl time.Duration) {
	el, found := e.lru.Get(key)

	if found {
		e.onCacheHit(key)

		return el.(*element[T]).val, calculateRemainTTL(el.(*element[T]).expiresEpochMs)
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
	return e.lru.Len()
}

// Clear removes all items from the cache.
func (e *ExpirationLRUCache[T]) Clear() {
	e.lru.Purge()
}
