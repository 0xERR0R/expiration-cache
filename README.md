# ExpirationLRUCache

A generic, thread-safe LRU cache for Go with per-item expiration and flexible expiration/callback hooks.

## Features
- LRU eviction policy with configurable max size
- Per-item expiration (TTL)
- Optional periodic cleanup
- Pre-expiration callback to refresh or remove items
- Cache hit/miss/put hooks
- Safe for concurrent use
- Optional N-way sharding to remove single-lock read contention on multi-core systems

## Installation

```
go get github.com/0xERR0R/expiration-cache
```

## Usage

### Basic Usage
```go
import (
    "context"
    "time"
    "github.com/0xERR0R/expiration-cache"
)

func main() {
    cache := expirationcache.NewCache[string](context.Background(), expirationcache.Options{})
    v := "hello"
    cache.Put("key1", &v, 5*time.Second)
    val, ttl := cache.Get("key1")
    if val != nil {
        println(*val, ttl.String())
    }
}
```

### With Expiration
```go
cache := expirationcache.NewCache[int](context.Background(), expirationcache.Options{CleanupInterval: time.Second})
v := 42
cache.Put("answer", &v, 2*time.Second)
time.Sleep(3 * time.Second)
val, _ := cache.Get("answer") // val will be nil (expired)
```

### With Callbacks
```go
cache := expirationcache.NewCache[string](context.Background(), expirationcache.Options{
    OnCacheHitFn: func(key string) { println("hit:", key) },
    OnCacheMissFn: func(key string) { println("miss:", key) },
    OnAfterPutFn: func(size int) { println("cache size:", size) },
})
v := "data"
cache.Put("k", &v, time.Second)
cache.Get("k") // prints: hit: k
cache.Get("notfound") // prints: miss: notfound
```

### With Pre-Expiration Function
```go
refreshFn := func(ctx context.Context, key string) (*string, time.Duration) {
    refreshed := "refreshed-value"
    return &refreshed, 5 * time.Second // refresh value and TTL
}
cache := expirationcache.NewCacheWithOnExpired[string](context.Background(), expirationcache.Options{}, refreshFn)
v := "old"
cache.Put("k", &v, time.Second)
// After 1s, the refreshFn will be called before removal, and the value will be refreshed.
```

### With Sharding
Every `Get` takes the LRU's lock to update recency, so on multi-core systems all reads
serialize on a single mutex. Set `Shards` to split the cache into N independent LRUs
(rounded up to a power of two); keys are routed to a shard by hash, so reads on
different shards never contend. `Shards: 0` (the default) means a single shard —
byte-for-byte identical to the unsharded cache.

```go
cache := expirationcache.NewCache[string](context.Background(), expirationcache.Options{
    MaxSize: 100_000,
    Shards:  16, // ~one or two shards per core is a good starting point
})
```

`MaxSize` is divided across shards (each holds `ceil(MaxSize/Shards)` items), so eviction
is per-shard rather than globally LRU — a standard sharded-cache trade-off that loosens
as the shard count grows.