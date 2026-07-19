package inference

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signedreq"
)

func TestBoundedNonceCache_ReplayDetection(t *testing.T) {
	c := NewBoundedNonceCache(8, 64)
	now := time.Now()
	ttl := 5 * time.Minute

	if !c.Consume("dev-a", "n1", now, ttl) {
		t.Fatalf("first consume must succeed")
	}
	if c.Consume("dev-a", "n1", now.Add(10*time.Second), ttl) {
		t.Fatalf("same nonce within TTL must be a replay")
	}
	// Different device, same nonce string — legal.
	if !c.Consume("dev-b", "n1", now, ttl) {
		t.Fatalf("same nonce on another device must succeed")
	}
	// After TTL the nonce can be reused.
	if !c.Consume("dev-a", "n1", now.Add(6*time.Minute), ttl) {
		t.Fatalf("nonce past TTL must be reusable")
	}
}

func TestBoundedNonceCache_PerDeviceCapEvicts(t *testing.T) {
	c := NewBoundedNonceCache(4, 1000)
	now := time.Now()
	ttl := time.Hour

	for i := range 4 {
		if !c.Consume("dev-a", fmt.Sprintf("n%d", i), now.Add(time.Duration(i)*time.Second), ttl) {
			t.Fatalf("consume %d", i)
		}
	}
	// 5th entry evicts the oldest (n0), never grows past the cap.
	if !c.Consume("dev-a", "n4", now.Add(10*time.Second), ttl) {
		t.Fatalf("consume at cap must still succeed")
	}
	if got := c.Len(); got != 4 {
		t.Fatalf("Len = %d, want 4 (per-device cap)", got)
	}
	// The evicted n0 is consumable again (narrowed replay window —
	// documented trade-off); the newest n4 is a replay.
	if c.Consume("dev-a", "n4", now.Add(11*time.Second), ttl) {
		t.Fatalf("n4 must still be a replay")
	}
	if !c.Consume("dev-a", "n0", now.Add(12*time.Second), ttl) {
		t.Fatalf("evicted n0 should be consumable (eviction happened)")
	}
}

func TestBoundedNonceCache_GlobalCapBounds(t *testing.T) {
	c := NewBoundedNonceCache(100, 10)
	now := time.Now()
	ttl := time.Hour

	// 20 distinct devices, one nonce each — total stays at the cap.
	for i := range 20 {
		if !c.Consume(fmt.Sprintf("dev-%d", i), "n", now.Add(time.Duration(i)*time.Second), ttl) {
			t.Fatalf("consume dev-%d", i)
		}
	}
	if got := c.Len(); got > 10 {
		t.Fatalf("Len = %d, want <= 10 (global cap)", got)
	}
}

func TestBoundedNonceCache_ExpiredBucketsReleased(t *testing.T) {
	c := NewBoundedNonceCache(8, 64)
	now := time.Now()
	ttl := time.Minute

	for i := range 5 {
		c.Consume(fmt.Sprintf("dev-%d", i), "n", now, ttl)
	}
	if got := c.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}
	// A consume far in the future GCs the touched bucket; the global
	// sweep only runs at the cap, so exercise per-bucket GC + the
	// empty-bucket delete by touching each device again.
	later := now.Add(time.Hour)
	for i := range 5 {
		c.Consume(fmt.Sprintf("dev-%d", i), "n2", later, ttl)
	}
	if got := c.Len(); got != 5 {
		t.Fatalf("Len after expiry sweep = %d, want 5 (old entries GC'd)", got)
	}
}

func TestBoundedNonceCache_ConsumeNonceIntegration(t *testing.T) {
	c := NewBoundedNonceCache(0, 0) // defaults
	now := time.Now()
	nonce := []byte("0123456789ab")
	if err := signedreq.ConsumeNonce(c, "dev", nonce, now, 5*time.Minute); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := signedreq.ConsumeNonce(c, "dev", nonce, now.Add(time.Second), 5*time.Minute); err == nil {
		t.Fatalf("replay must error")
	}
}

func TestBoundedNonceCache_Concurrent(t *testing.T) {
	c := NewBoundedNonceCache(64, 512)
	now := time.Now()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				c.Consume(fmt.Sprintf("dev-%d", g%4), fmt.Sprintf("n-%d-%d", g, i), now.Add(time.Duration(i)*time.Millisecond), time.Minute)
			}
		}()
	}
	wg.Wait()
	if got := c.Len(); got > 512 {
		t.Fatalf("Len = %d, want <= 512", got)
	}
}
