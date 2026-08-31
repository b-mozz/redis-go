// striped_test.go
// Tests for StripedMap. Three jobs here:
//
//  1. prove the striping is CORRECT — that's TestStripedMap_RaceStress under -race,
//     which is a direct port of TestConcurrentHMap_RaceStress. Same workload, same
//     contention, different lock strategy.
//  2. prove the stripe selection actually spreads keys (TestStripedMap_Distribution).
//     a wrong shift, or using the low bits, still passes every functional test — the
//     map just quietly degenerates into one busy stripe. only a distribution check
//     catches that.
//  3. prove the fan-out ops (Size / Keys / ForEach / SweepExpired) visit every stripe.

package store

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// backdateStriped is the StripedMap twin of backdate() in ttl_test.go: it pushes a
// key's deadline one second into the PAST so it counts as already expired, letting us
// test expiry instantly instead of sleeping and hoping the clock moves.
func backdateStriped(s *StripedMap, key string) {
	past := time.Now().UnixNano() - int64(time.Second)
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.m.setExpiryH(key, hcode, past)
}

// countPerStripe reports how many live entries each stripe holds.
func countPerStripe(s *StripedMap) []int {
	counts := make([]int, numStripes)
	for i := range s.stripes {
		st := &s.stripes[i]
		st.mu.Lock()
		if st.m.new != nil {
			counts[i] += st.m.new.used
		}
		if st.m.old != nil {
			counts[i] += st.m.old.used
		}
		st.mu.Unlock()
	}
	return counts
}

// ==== 1. correctness under contention ====

// The whole point of this test is the race detector:
//
//	go test -race ./internal/store/
//
// If the per-stripe locking is right this passes clean; if a stripe is ever touched
// without its own lock held, -race prints the exact conflicting access.
func TestStripedMap_RaceStress(t *testing.T) {
	s := &StripedMap{}

	const (
		numGoroutines = 8
		opsPerG       = 2000
		keySpace      = 200 // small key space so goroutines actually collide on keys
	)

	var wg sync.WaitGroup

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < opsPerG; i++ {
				key := "key:" + strconv.Itoa(i%keySpace)

				switch i % 4 {
				case 0:
					s.Set(key, "value")
				case 1:
					s.Get(key)
				case 2:
					s.Del(key)
				case 3:
					s.SetTTL(key, "value", 60)
				}
			}
		}(g)
	}

	wg.Wait()

	// give the sweeper and the fan-out readers a spin too, so those paths are covered
	// by -race as well. (no assertion -- we only care that -race stays quiet.)
	s.SweepExpired(time.Now().UnixNano(), 16)
	s.Size()
	s.Keys()
}

// Rehashing is the dangerous part: hMap.search mutates (it drives helpRehash), so even
// a "read" can migrate nodes between tables. Drive several stripes over the
// maxLoadFactor threshold concurrently and make sure -race stays quiet AND no key is
// lost in the migration.
func TestStripedMap_ConcurrentRehash(t *testing.T) {
	s := &StripedMap{}

	const (
		numGoroutines = 8
		keysPerG      = 1000 // 8000 keys over 32 stripes forces many rehashes per stripe
	)

	var wg sync.WaitGroup
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < keysPerG; i++ {
				key := "g" + strconv.Itoa(g) + ":" + strconv.Itoa(i)
				s.Set(key, "v")
				// read back immediately: this call may itself advance the rehash
				if _, ok := s.Get(key); !ok {
					t.Errorf("key %q vanished right after Set", key)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	want := numGoroutines * keysPerG
	if got := s.Size(); got != want {
		t.Fatalf("after concurrent rehash: Size = %d, want %d", got, want)
	}

	// every key must still be findable once the dust settles
	for g := 0; g < numGoroutines; g++ {
		for i := 0; i < keysPerG; i++ {
			key := "g" + strconv.Itoa(g) + ":" + strconv.Itoa(i)
			if _, ok := s.Get(key); !ok {
				t.Fatalf("key %q lost after rehashing", key)
			}
		}
	}
}

// ==== 2. the striping actually stripes ====

// A wrong shift (or reusing the low bits that hTab already uses for the bucket index)
// leaves every functional test passing while the map quietly collapses onto one stripe.
// This is the only test that notices.
func TestStripedMap_Distribution(t *testing.T) {
	s := &StripedMap{}

	const n = 10000
	for i := 0; i < n; i++ {
		s.Set("key:"+strconv.Itoa(i), "value")
	}

	counts := countPerStripe(s)

	total := 0
	for _, c := range counts {
		total += c
	}
	if total != n {
		t.Fatalf("stripe counts sum to %d, want %d", total, n)
	}

	mean := float64(n) / float64(numStripes)
	for i, c := range counts {
		if c == 0 {
			t.Errorf("stripe %d is empty after %d inserts -- stripe selection is broken", i, n)
			continue
		}
		if float64(c) > 2*mean {
			t.Errorf("stripe %d holds %d keys, more than 2x the mean of %.0f", i, c, mean)
		}
	}
}

// The stripe/bucket split must not overlap. If the stripe were taken from the low bits,
// every key inside a stripe would agree on those bits and pile into a fraction of that
// stripe's buckets. Check the two index derivations are independent by confirming that,
// within a single stripe, the bucket indexes are not all identical.
func TestStripedMap_StripeAndBucketBitsAreDisjoint(t *testing.T) {
	s := &StripedMap{}

	const n = 10000
	for i := 0; i < n; i++ {
		s.Set("key:"+strconv.Itoa(i), "value")
	}

	// find the busiest stripe and look at how its keys spread over its buckets
	busiest, best := 0, -1
	for i, c := range countPerStripe(s) {
		if c > best {
			busiest, best = i, c
		}
	}

	st := &s.stripes[busiest]
	st.mu.Lock()
	defer st.mu.Unlock()

	ht := st.m.new
	if ht == nil {
		t.Fatal("busiest stripe has no table")
	}

	occupied := 0
	for _, head := range ht.tab {
		if head != nil {
			occupied++
		}
	}

	buckets := int(ht.mask) + 1
	// with ~best keys in `buckets` buckets and a decent hash, well over a quarter of the
	// buckets should be in use. if the stripe bits overlapped the bucket bits this would
	// collapse to buckets/numStripes.
	if min := buckets / 4; occupied < min {
		t.Fatalf("stripe %d: only %d of %d buckets occupied for %d keys (want >= %d) -- "+
			"stripe index and bucket index are probably sharing bits",
			busiest, occupied, buckets, best, min)
	}
}

// ==== 3. fan-out ops reach every stripe ====

func TestStripedMap_SizeAndKeysFanOut(t *testing.T) {
	s := &StripedMap{}

	const n = 5000
	for i := 0; i < n; i++ {
		s.Set("key:"+strconv.Itoa(i), "value")
	}

	if got := s.Size(); got != n {
		t.Fatalf("Size = %d, want %d", got, n)
	}

	keys := s.Keys()
	if len(keys) != n {
		t.Fatalf("len(Keys()) = %d, want %d", len(keys), n)
	}

	// Keys must not duplicate or drop anything
	seen := make(map[string]bool, n)
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("Keys() returned %q twice", k)
		}
		seen[k] = true
	}

	// ForEach must see the same set
	count := 0
	s.ForEach(func(key, val string) bool {
		count++
		if !seen[key] {
			t.Fatalf("ForEach produced %q, which Keys() did not", key)
		}
		return true
	})
	if count != n {
		t.Fatalf("ForEach visited %d entries, want %d", count, n)
	}
}

func TestStripedMap_ForEachStopsEarly(t *testing.T) {
	s := &StripedMap{}
	for i := 0; i < 1000; i++ {
		s.Set("key:"+strconv.Itoa(i), "value")
	}

	count := 0
	s.ForEach(func(key, val string) bool {
		count++
		return count < 10 // stop after the 10th
	})

	if count != 10 {
		t.Fatalf("ForEach visited %d entries after the callback said stop at 10", count)
	}
}

// SweepExpired does one stripe per call. Over numStripes calls it must therefore have
// visited every stripe -- if the round-robin cursor were broken (or the budget were
// divided across stripes and floored to zero) some stripes would never be swept and
// their expired keys would leak until someone happened to read them.
func TestStripedMap_SweepFansOutAcrossStripes(t *testing.T) {
	s := &StripedMap{}

	const n = 2000
	for i := 0; i < n; i++ {
		key := "key:" + strconv.Itoa(i)
		s.Set(key, "value")
		backdateStriped(s, key)
	}

	if got := s.Size(); got != n {
		t.Fatalf("setup: Size = %d, want %d", got, n)
	}

	// budget generously larger than any one stripe's bucket count so a single visit
	// clears that stripe outright; the thing under test is the stripe round-robin,
	// not the within-stripe bucket cursor.
	now := time.Now().UnixNano()
	evicted := 0
	for i := 0; i < numStripes; i++ {
		evicted += s.SweepExpired(now, 4096)
	}

	if evicted != n {
		t.Fatalf("swept %d keys over %d calls, want %d -- some stripe was never visited",
			evicted, numStripes, n)
	}
	if got := s.Size(); got != 0 {
		t.Fatalf("Size = %d after sweeping every stripe, want 0", got)
	}
}

// The sweep must respect its budget: one call visits one stripe and at most `budget`
// of its buckets, so a single call cannot clear a large keyspace.
func TestStripedMap_SweepRespectsBudget(t *testing.T) {
	s := &StripedMap{}

	const n = 2000
	for i := 0; i < n; i++ {
		key := "key:" + strconv.Itoa(i)
		s.Set(key, "value")
		backdateStriped(s, key)
	}

	s.SweepExpired(time.Now().UnixNano(), 2)

	if got := s.Size(); got == 0 {
		t.Fatal("a single 2-bucket sweep cleared the whole map; the budget is not being honoured")
	}
}

// ==== 4. semantics parity with ConcurrentHMap ====
// These mirror ttl_test.go. Striping is supposed to be a pure lock refactor, so every
// one of these behaviours must survive it unchanged.

func TestStripedMap_LazyExpiryEvictsOnAccess(t *testing.T) {
	s := &StripedMap{}

	s.Set("session", "abc")
	backdateStriped(s, "session")

	if _, ok := s.Get("session"); ok {
		t.Fatal("expected expired key to be a miss on Get, but it was found")
	}
	if s.Size() != 0 {
		t.Fatalf("expected the expired key to be deleted, but Size = %d", s.Size())
	}
}

func TestStripedMap_TTLSentinels(t *testing.T) {
	s := &StripedMap{}

	if got := s.TTL("nope"); got != -2 {
		t.Errorf("TTL of a missing key = %d, want -2", got)
	}

	s.Set("plain", "v")
	if got := s.TTL("plain"); got != -1 {
		t.Errorf("TTL of a key with no expiry = %d, want -1", got)
	}

	s.SetTTL("temp", "v", 60)
	if got := s.TTL("temp"); got < 1 || got > 60 {
		t.Errorf("TTL of a 60s key = %d, want 1..60", got)
	}
}

func TestStripedMap_Persist(t *testing.T) {
	s := &StripedMap{}

	if s.Persist("nope") {
		t.Error("Persist on a missing key returned true")
	}

	s.Set("plain", "v")
	if s.Persist("plain") {
		t.Error("Persist on a key with no TTL returned true")
	}

	s.SetTTL("temp", "v", 60)
	if !s.Persist("temp") {
		t.Fatal("Persist on a key with a TTL returned false")
	}
	if got := s.TTL("temp"); got != -1 {
		t.Errorf("after Persist, TTL = %d, want -1", got)
	}
}

func TestStripedMap_PlainSetClearsTTL(t *testing.T) {
	s := &StripedMap{}

	s.SetTTL("k", "v1", 60)
	s.Set("k", "v2") // a plain SET must make the key persistent again

	if got := s.TTL("k"); got != -1 {
		t.Errorf("after a plain Set over a key with a TTL, TTL = %d, want -1", got)
	}
	if v, _ := s.Get("k"); v != "v2" {
		t.Errorf("value = %q, want %q", v, "v2")
	}
}

func TestStripedMap_ExpireAndDel(t *testing.T) {
	s := &StripedMap{}

	if s.Expire("nope", 10) {
		t.Error("Expire on a missing key returned true")
	}

	s.Set("k", "v")
	if !s.Expire("k", 60) {
		t.Fatal("Expire on an existing key returned false")
	}
	if got := s.TTL("k"); got < 1 || got > 60 {
		t.Errorf("TTL after Expire = %d, want 1..60", got)
	}

	if !s.Del("k") {
		t.Fatal("Del of an existing key returned false")
	}
	if s.Del("k") {
		t.Error("Del of an already-deleted key returned true")
	}
	if s.Size() != 0 {
		t.Errorf("Size = %d after deleting the only key, want 0", s.Size())
	}
}
