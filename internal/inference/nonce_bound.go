package inference

import (
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/proto/signedreq"
)

// Bounds for the replay cache. Sized generously against legitimate
// traffic (a nonce lives only for the verify TTL, 5 minutes by
// default) while capping the worst case: many short-lived Public Share
// grants mean the set of sending device IDs is no longer limited to
// the owner's own network, so an unbounded per-device map would be a
// memory-DoS surface (spec §8.5).
const (
	// DefaultNoncePerDeviceLimit caps entries per sending device.
	DefaultNoncePerDeviceLimit = 512
	// DefaultNonceTotalLimit caps entries across all devices.
	DefaultNonceTotalLimit = 65536
)

// BoundedNonceCache is a signedreq.NonceCache with per-device and
// global entry caps. Compared to signedreq.MemoryNonceCache it also
// deletes empty device buckets, so devices that stop talking (expired
// grants) release their memory.
//
// When a cap is hit after expired entries are collected, the oldest
// live entry is evicted. Evicting a live nonce narrows the replay
// window for that entry only — bounded memory wins over a marginally
// longer replay horizon, and the signature check still gates every
// request before the nonce cache is consulted.
type BoundedNonceCache struct {
	mu        sync.Mutex
	buckets   map[string]map[string]time.Time
	total     int
	perDevice int
	maxTotal  int
}

// NewBoundedNonceCache returns an empty cache. Non-positive limits
// fall back to the defaults.
func NewBoundedNonceCache(perDevice, maxTotal int) *BoundedNonceCache {
	if perDevice <= 0 {
		perDevice = DefaultNoncePerDeviceLimit
	}
	if maxTotal <= 0 {
		maxTotal = DefaultNonceTotalLimit
	}
	return &BoundedNonceCache{
		buckets:   map[string]map[string]time.Time{},
		perDevice: perDevice,
		maxTotal:  maxTotal,
	}
}

var _ signedreq.NonceCache = (*BoundedNonceCache)(nil)

// Consume implements signedreq.NonceCache.
func (c *BoundedNonceCache) Consume(deviceID, nonce string, now time.Time, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	bucket := c.buckets[deviceID]
	if seen, ok := bucket[nonce]; ok && now.Sub(seen) <= ttl {
		return false // replay
	}

	// Opportunistic GC of the touched bucket (same policy as
	// MemoryNonceCache), then bucket-level bound.
	c.gcBucketLocked(deviceID, now, ttl)
	bucket = c.buckets[deviceID]
	if bucket == nil {
		bucket = map[string]time.Time{}
		c.buckets[deviceID] = bucket
	}
	if len(bucket) >= c.perDevice {
		evictOldestLocked(bucket, &c.total)
	}

	// Global bound: GC everything first, then evict from the largest
	// bucket if the cache is still full. Full sweeps only run at the
	// cap, so the amortized cost stays low.
	if c.total >= c.maxTotal {
		for id := range c.buckets {
			c.gcBucketLocked(id, now, ttl)
		}
	}
	if c.total >= c.maxTotal {
		var largest string
		for id, b := range c.buckets {
			if largest == "" || len(b) > len(c.buckets[largest]) {
				largest = id
			}
		}
		if largest != "" {
			evictOldestLocked(c.buckets[largest], &c.total)
		}
	}

	if c.buckets[deviceID] == nil {
		c.buckets[deviceID] = map[string]time.Time{}
	}
	c.buckets[deviceID][nonce] = now
	c.total++
	return true
}

// gcBucketLocked drops expired entries from one device bucket and
// deletes the bucket entirely when it empties. Caller holds c.mu.
func (c *BoundedNonceCache) gcBucketLocked(deviceID string, now time.Time, ttl time.Duration) {
	bucket, ok := c.buckets[deviceID]
	if !ok {
		return
	}
	for k, t := range bucket {
		if now.Sub(t) > ttl {
			delete(bucket, k)
			c.total--
		}
	}
	if len(bucket) == 0 {
		delete(c.buckets, deviceID)
	}
}

// evictOldestLocked removes the entry with the oldest timestamp from
// bucket. Caller holds the cache lock.
func evictOldestLocked(bucket map[string]time.Time, total *int) {
	var oldestKey string
	var oldestAt time.Time
	for k, t := range bucket {
		if oldestKey == "" || t.Before(oldestAt) {
			oldestKey = k
			oldestAt = t
		}
	}
	if oldestKey != "" {
		delete(bucket, oldestKey)
		*total--
	}
}

// Len reports the total number of live entries (for tests/metrics).
func (c *BoundedNonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
