// sharded.go
// Lock striping for the store. See doc/benchmark-before-lock-striping.md for the
// measurements that motivated this, and doc/replication-plan.md §1.5 for the design.
//
// The problem ConcurrentHMap has: one sync.Mutex over one hMap, so every goroutine in
// the server funnels through a single lock. Measured on an M2 (8 cores), parallel Get
// goes 37.4 ns/op at 1 core to 128.2 ns/op at 8 — aggregate throughput 26.8 M ops/s
// down to 7.8 M, i.e. 0.29x. Adding cores makes the map slower, the classic
// single-cacheline ping-pong: every core wanting the lock must pull that line into its
// own L1 exclusive, invalidating it everywhere else.
//
// ShardedMap splits the keyspace across numStripes independent (mutex, hMap) pairs.
// Contention divides by numStripes, and because each stripe owns its own old/new
// tables and migratepos, a rehash now stalls 1/numStripes of the keyspace instead of
// all of it.
//
// What this does NOT do: beat sync.Map on reads. sync.Map wins by taking no lock at all
// on its read path, not by having more locks. The honest claim is that striping fixes
// our scaling curve and should beat map+RWMutex under concurrency.

package store

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// stripeBits is how many of the hash's high bits pick the stripe.
	stripeBits = 5
	// numStripes is a power of two so the stripe index is a shift, never a modulo.
	numStripes = 1 << stripeBits
	// stripeShift moves the top stripeBits bits down into the low end.
	stripeShift = 64 - stripeBits

	// storeSeed is the murmur3 seed used for every stripe. It MUST match hMap.seed,
	// because one hcode both picks the stripe and indexes the buckets inside it. It is
	// 0 so that a zero-value hMap (hMap.seed == 0) is already consistent with it — no
	// constructor needed, which is what keeps ShardedMap usable as &ShardedMap{}.
	storeSeed uint64 = 0
)

// stripeOf picks the stripe from the HIGH bits of the hash.
//
// This deliberately does not reuse the low bits. hTab.insert indexes buckets with
// hcode & ht.mask — the low bits. If the stripe used those same low bits, every key
// landing in a given stripe would agree on its low bits and therefore collide into
// 1/numStripes of that stripe's buckets, turning the chains into linked lists.
//
// murmur3's finalizer (the avalanche at the bottom of the function) mixes the whole
// word, so the top bits are as well-distributed as the bottom ones.
func stripeOf(hcode uint64) uint64 {
	return hcode >> stripeShift
}

// stripe is the per-stripe state: one lock and the hMap it guards.
//
// NOT padded to a cache line, and that is a measured decision rather than an oversight.
// stripe is 48 bytes, so two or three of them share a 128-byte line (M-series), and the
// obvious worry is that neighbouring stripes' mutexes ping-pong against each other —
// reintroducing at 1/3 scale the exact effect striping exists to remove.
//
// Measured, it does not happen. benchstat over n=10, padded vs unpadded, parallel Get:
//
//	-cpu=1   ~       (p=0.190)
//	-cpu=2   -2.95%  (p=0.023)
//	-cpu=4   +6.59%  (p=0.009)
//	-cpu=8   -7.22%  (p=0.029)
//	geomean  -1.22%
//
// The signs flip across core counts, which is what noise looks like, not an effect.
// The reason is that the mutex is not alone in its line — this stripe's own hMap header
// (new, old, migratepos, seed, sweepCursor) sits in that same line and is read by
// whoever holds the lock. The "false" sharing overlaps almost entirely with sharing
// that is real and unavoidable, so separating the stripes saves a fetch that was
// already needed.
//
// BenchmarkShardedMapPadded_Get_Parallel in hashtable_test.go keeps the padded variant
// alive so this can be re-checked rather than taken on trust. If numStripes ever drops
// far enough that a few adjacent stripes get genuinely hot, re-run it before assuming
// this conclusion still holds.
type stripe struct {
	// mu guards m, exactly as ConcurrentHMap.mu guards its own hMap. A plain Mutex and
	// not an RWMutex: hMap.search mutates (it drives helpRehash and does lazy eviction),
	// so there is no genuinely read-only path to hand an RLock to.
	mu sync.Mutex
	m  hMap
}

// ShardedMap is the striped store. Zero value is ready to use — &ShardedMap{} — for
// the same reason ConcurrentHMap is: sync.Mutex's zero value is an unlocked mutex, and
// hMap lazily allocates its first table on the first insert. So an untouched
// ShardedMap allocates no bucket arrays at all.
//
// Always pass *ShardedMap. It contains mutexes and must never be copied (go vet's
// copylocks check will catch it).
type ShardedMap struct {
	stripes [numStripes]stripe

	// sweepCursor round-robins SweepExpired across stripes, one stripe per call.
	// atomic because the expiry loop goroutine calls it while connection goroutines
	// are in the single-key methods; it is the one piece of ShardedMap state that
	// lives outside any stripe's lock.
	sweepCursor atomic.Uint64
}

// stripeFor hashes the key once and returns the stripe that owns it along with the
// hash, so callers can pass hcode straight down into hMap and never re-derive it.
func (s *ShardedMap) stripeFor(key string) (*stripe, uint64) {
	hcode := murmur3([]byte(key), storeSeed)
	return &s.stripes[stripeOf(hcode)], hcode
}

// ==== single-key operations ====
// Each locks exactly one stripe. Semantics are identical to ConcurrentHMap's.

// Get returns the value for key. An expired key reports a miss (hMap.search evicts it).
func (s *ShardedMap) Get(key string) (string, bool) {
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	node, ok := st.m.search(key, hcode)
	if !ok {
		return "", false
	}
	return node.val, true
}

// Set stores key/val. A plain Set clears any existing TTL — matches Redis, where SET
// without an expiry option makes the key persistent again.
func (s *ShardedMap) Set(key string, val string) {
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	st.m.insert(key, val, hcode)
	st.m.setExpiryH(key, hcode, 0) // drop any TTL the old value may have had
}

// SetTTL stores key/val with an expiry of ttlSeconds from now. backs `set key val EX n`.
func (s *ShardedMap) SetTTL(key string, val string, ttlSeconds int64) {
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	st.m.insert(key, val, hcode)
	expireAt := time.Now().UnixNano() + ttlSeconds*int64(time.Second)
	st.m.setExpiryH(key, hcode, expireAt)
}

// Expire sets a TTL of ttlSeconds on an existing key.
// returns true if the key exists (TTL applied), false otherwise. backs `expire`.
func (s *ShardedMap) Expire(key string, ttlSeconds int64) bool {
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	expireAt := time.Now().UnixNano() + ttlSeconds*int64(time.Second)
	return st.m.setExpiryH(key, hcode, expireAt)
}

// TTL reports the remaining life of a key in whole seconds. backs `ttl`.
// sentinels match Redis:  -1 = key exists but has no expiry,  -2 = key does not exist.
func (s *ShardedMap) TTL(key string) int64 {
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	node, ok := st.m.search(key, hcode) // search also lazily evicts an already-expired key
	if !ok {
		return -2 // no such key
	}
	if node.expireAt == 0 {
		return -1 // key exists but never expires
	}

	// search guarantees the node isn't past its deadline, so remaining > 0.
	remaining := node.expireAt - time.Now().UnixNano()

	// round UP to whole seconds so a key with 0.3s left reports 1, not 0
	return (remaining + int64(time.Second) - 1) / int64(time.Second)
}

// Persist removes the TTL from a key, making it immortal again.
// returns true only if a TTL was actually removed. backs `persist`.
func (s *ShardedMap) Persist(key string) bool {
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	node, ok := st.m.search(key, hcode)
	if !ok || node.expireAt == 0 {
		return false // no such key, or it had no TTL to begin with
	}

	node.expireAt = 0
	return true
}

// Del removes a key, reporting whether it was there.
func (s *ShardedMap) Del(key string) bool {
	st, hcode := s.stripeFor(key)
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.m.del(key, hcode)
}

// ==== fan-out operations ====
//
// These touch every stripe, and they do it ONE STRIPE AT A TIME — they never hold more
// than a single lock. That means there is no instant at which the whole map is frozen,
// so none of them is a globally consistent snapshot: a key can be written into a stripe
// that has already been visited, or deleted from one that hasn't.
//
// That is the intended trade, not an oversight. Real Redis KEYS gives you the same
// non-guarantee, and locking all numStripes mutexes to fix it would reintroduce exactly
// the global stall striping exists to remove.

// Size returns the total number of entries across all stripes (and, within a stripe,
// across both the new and old tables, since during rehashing entries live in both).
func (s *ShardedMap) Size() int {
	n := 0
	for i := range s.stripes {
		st := &s.stripes[i]
		st.mu.Lock()
		if st.m.new != nil {
			n += st.m.new.used
		}
		if st.m.old != nil {
			n += st.m.old.used
		}
		st.mu.Unlock()
	}
	return n
}

// Keys returns a snapshot of every key in the map — per stripe, see the note above.
func (s *ShardedMap) Keys() []string {
	out := make([]string, 0)
	for i := range s.stripes {
		st := &s.stripes[i]
		st.mu.Lock()
		if st.m.new != nil {
			collectKeys(st.m.new, &out)
		}
		if st.m.old != nil {
			collectKeys(st.m.old, &out)
		}
		st.mu.Unlock()
	}
	return out
}

// ForEach walks every (key, value) pair. The callback returns false to stop early.
//
// The callback must not call back into the map. Only one stripe's lock is held at a
// time, so re-entering would deadlock only when the callback happens to touch a key in
// the stripe currently being walked — a deadlock that depends on the hash of whatever
// key you passed is far worse to debug than one that always fires, so treat the rule as
// absolute rather than as "usually fine".
func (s *ShardedMap) ForEach(fn func(key, val string) bool) {
	for i := range s.stripes {
		st := &s.stripes[i]

		keepGoing := true
		st.mu.Lock()
		if st.m.new != nil {
			keepGoing = walk(st.m.new, fn)
		}
		if keepGoing && st.m.old != nil {
			keepGoing = walk(st.m.old, fn)
		}
		st.mu.Unlock()

		if !keepGoing {
			return
		}
	}
}

// SweepExpired actively evicts expired keys, doing a bounded amount of work per call:
// it sweeps ONE stripe, looking at at most `budget` of that stripe's buckets, starting
// where that stripe's own cursor left off. It returns the number of keys evicted.
//
// One whole stripe per call, rather than budget/numStripes buckets in every stripe.
// That is deliberate: the server calls this every 100ms with budget=20, and 20 split
// across 32 stripes floors to zero buckets each — active expiry would silently stop
// dead. Round-robin instead, so the full keyspace is still covered every numStripes
// ticks (3.2s at the server's current settings) while only one stripe ever locks.
//
// Like ConcurrentHMap's version it only sweeps the `new` table. Anything still sitting
// in `old` during a rehash is either migrated into `new` by helpRehash (where a later
// sweep catches it) or evicted lazily on access, so nothing leaks permanently.
func (s *ShardedMap) SweepExpired(now int64, budget int) int {
	// Add returns the NEW value, so subtract one to start this cursor at stripe 0.
	idx := (s.sweepCursor.Add(1) - 1) & (numStripes - 1)
	st := &s.stripes[idx]

	st.mu.Lock()
	defer st.mu.Unlock()

	ht := st.m.new
	if ht == nil {
		return 0 // this stripe was never written to, nothing to sweep
	}

	buckets := int(ht.mask) + 1
	evicted := 0

	for i := 0; i < budget; i++ {
		if st.m.sweepCursor >= buckets {
			st.m.sweepCursor = 0 // reached the end of the table, wrap back to the start
		}

		evicted += ht.sweepBucket(st.m.sweepCursor, now)
		st.m.sweepCursor++
	}

	return evicted
}
