// ttl_test.go
// Correctness tests for TTL / expiration:
//   - lazy eviction: an expired key is dropped when you access it
//   - active sweeper: a background pass drops expired keys you never touch
//   - the ttl / persist sentinel values (-1 no expiry, -2 missing)

package store

import (
	"strconv"
	"testing"
	"time"
)

// backdate pushes a key's deadline one second into the PAST, so it counts as
// already expired. this lets us test expiry instantly instead of sleeping in
// the test and hoping the clock moves.
func backdate(c *ConcurrentHMap, key string) {
	past := time.Now().UnixNano() - int64(time.Second)
	c.m.setExpiry(key, past)
}

// lazy expiry: reading an expired key should be a miss, AND it should actually
// remove the key (not just hide it), so Size drops to 0.
func TestLazyExpiry_EvictsOnAccess(t *testing.T) {
	c := &ConcurrentHMap{}

	c.Set("session", "abc")
	backdate(c, "session")

	_, ok := c.Get("session")
	if ok {
		t.Fatal("expected expired key to be a miss on Get, but it was found")
	}

	if c.Size() != 0 {
		t.Fatalf("expected the expired key to be deleted, but Size = %d", c.Size())
	}
}

// active sweeper: keys we never touch should still get cleaned up by the
// background sweep, while live (no-TTL) keys must be left alone.
func TestActiveSweeper_EvictsExpiredKeepsLive(t *testing.T) {
	c := &ConcurrentHMap{}

	liveKeys := []string{"live1", "live2", "live3"}
	deadKeys := []string{"dead1", "dead2", "dead3"}

	for _, key := range liveKeys {
		c.Set(key, "v")
	}

	for _, key := range deadKeys {
		c.Set(key, "v")
		backdate(c, key)
	}

	// the sweep is bounded (only a few buckets per call), so run it enough times
	// to cover the whole table.
	for i := 0; i < 64; i++ {
		c.SweepExpired(time.Now().UnixNano(), 4)
	}

	if c.Size() != len(liveKeys) {
		t.Fatalf("expected %d live keys left, but Size = %d", len(liveKeys), c.Size())
	}

	for _, key := range liveKeys {
		_, ok := c.Get(key)
		if !ok {
			t.Errorf("live key %q was wrongly evicted", key)
		}
	}
}

// budget check: a single small-budget sweep looks at only a few buckets, so it
// must NOT be able to clear a large, fully-expired table in one call.
func TestSweeper_RespectsBudget(t *testing.T) {
	c := &ConcurrentHMap{}

	const numKeys = 200
	for i := 0; i < numKeys; i++ {
		key := "key:" + strconv.Itoa(i)
		c.Set(key, "v")
		backdate(c, key)
	}

	sizeBefore := c.Size()

	// budget of 2 buckets: nowhere near enough to reach all 200 keys at once.
	c.SweepExpired(time.Now().UnixNano(), 2)

	if sizeBefore > 0 && c.Size() == 0 {
		t.Fatal("a budget-2 sweep cleared the entire table; the budget was not respected")
	}
}

// ttl returns remaining seconds, with -1 (no expiry) and -2 (missing) sentinels.
func TestTTL_Sentinels(t *testing.T) {
	c := &ConcurrentHMap{}

	// a key that does not exist -> -2
	got := c.TTL("missing")
	if got != -2 {
		t.Errorf("TTL(missing) = %d, want -2", got)
	}

	// a key with no expiry -> -1
	c.Set("noexp", "v")
	got = c.TTL("noexp")
	if got != -1 {
		t.Errorf("TTL(no expiry) = %d, want -1", got)
	}

	// a key with a 100s TTL -> some value in (0, 100]
	c.SetTTL("withexp", "v", 100)
	got = c.TTL("withexp")
	if got < 1 || got > 100 {
		t.Errorf("TTL(withexp) = %d, want a value in (0, 100]", got)
	}
}

// persist removes a TTL. it returns true only when there was actually a TTL to
// remove, and afterwards the key should report -1 (no expiry).
func TestPersist(t *testing.T) {
	c := &ConcurrentHMap{}

	// nothing to persist on a missing key
	if c.Persist("missing") {
		t.Error("Persist(missing) = true, want false")
	}

	// nothing to persist on a key that never had a TTL
	c.Set("noexp", "v")
	if c.Persist("noexp") {
		t.Error("Persist(key with no TTL) = true, want false")
	}

	// a key WITH a TTL -> persist succeeds, and the TTL is gone afterwards
	c.SetTTL("k", "v", 100)

	if !c.Persist("k") {
		t.Error("Persist(key with TTL) = false, want true")
	}

	got := c.TTL("k")
	if got != -1 {
		t.Errorf("after Persist, TTL = %d, want -1", got)
	}
}

// a plain Set over a key that had a TTL should clear that TTL (Redis semantics).
func TestPlainSet_ClearsTTL(t *testing.T) {
	c := &ConcurrentHMap{}

	c.SetTTL("k", "v", 100)
	c.Set("k", "v2")

	got := c.TTL("k")
	if got != -1 {
		t.Errorf("after a plain Set over a TTL key, TTL = %d, want -1", got)
	}
}
