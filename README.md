# ExpirationLRUCache

A generic, thread-safe LRU cache for Go with per-item expiration and flexible expiration/callback hooks.

## Features
- LRU eviction policy with configurable max size
- Per-item expiration (TTL)
- Optional periodic cleanup
- Pre-expiration callback to refresh or remove items
- Cache hit/miss/put hooks
- Safe for concurrent use

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