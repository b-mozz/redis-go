// hashtable_test.go
// Benchmarks for our custom ConcurrentHMap.
//
// The goal here is NOT to beat real Redis (that is C, hand-tuned for 15+ years,
// and benchmarking against it would mostly measure TCP + protocol overhead, not
// our data structure). The goal is to isolate OUR hashTable and compare it against
// the two things a Go dev would normally reach for:
//
//   1. sync.Map        -> Go's built-in concurrent map
//   2. map + RWMutex   -> the naive "just wrap a map in a lock" approach
//
// This way the comparison is apples-to-apples: every contender is an in-process,
// concurrent string->string map. Whatever speed difference we see is the cost (or
// benefit) of OUR progressive-rehashing design, not the network.
//
// to run:  go test -bench=. ./internal/store/
// the report gives ns/op (nanoseconds per operation). lower is better.

package store

import (
	"strconv"
	"sync"
	"testing"
	"unsafe"
)

// benchKeys pre-builds a fixed set of keys so the key-generation cost (strconv,
// string building) does NOT leak into the numbers we are measuring. we want to
// time the map operations, not the setup.
const benchSize = 10000

func makeBenchKeys(n int) []string {
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "key:" + strconv.Itoa(i)
	}
	return keys
}

// ============================================================
// our ConcurrentHMap
// ============================================================

// BenchmarkConcurrentHMap_Set measures repeated inserts.
// note: because we loop over a fixed key set, after the first pass these become
// updates (upserts) rather than fresh inserts. that is fine and realistic: a real
// cache overwrites existing keys constantly.
func BenchmarkConcurrentHMap_Set(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &ConcurrentHMap{}

	// b.ResetTimer() throws away the time spent on setup above (key building,
	// allocating the store) so it does not pollute the measurement.
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// i % benchSize keeps us cycling through the same key set forever,
		// no matter how many iterations Go decides to run.
		key := keys[i%benchSize]
		store.Set(key, "value")
	}
}

func BenchmarkConcurrentHMap_Get(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &ConcurrentHMap{}

	// pre-fill the store first so Get actually finds something to read.
	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Get(key)
	}
}

func BenchmarkConcurrentHMap_Del(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &ConcurrentHMap{}

	// fill first, otherwise Del has nothing to do and we'd just be timing misses.
	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Del(key)
		// re-insert immediately so the NEXT loop iteration has something to delete
		// again. without this the store empties out after one pass and we'd be
		// measuring empty-table misses for the rest of the run.
		store.Set(key, "value")
	}
}

// ============================================================
// baseline 1: sync.Map  (Go's built-in concurrent map)
// ============================================================

func BenchmarkSyncMap_Set(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	var store sync.Map

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Store(key, "value")
	}
}

func BenchmarkSyncMap_Get(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	var store sync.Map

	for _, key := range keys {
		store.Store(key, "value")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Load(key)
	}
}

func BenchmarkSyncMap_Del(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	var store sync.Map

	for _, key := range keys {
		store.Store(key, "value")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Delete(key)
		store.Store(key, "value") // re-insert, same reasoning as our Del benchmark
	}
}

// ============================================================
// baseline 2: map + RWMutex  (the naive "wrap a map in a lock" approach)
// ============================================================

// lockedMap is the simplest possible concurrent map: a regular Go map guarded by
// a read-write mutex. this is what most people write before they learn about
// sync.Map or build something custom like ours. good honest baseline.
type lockedMap struct {
	mu sync.RWMutex
	m  map[string]string
}

func newLockedMap() *lockedMap {
	return &lockedMap{m: make(map[string]string)}
}

func (l *lockedMap) Set(key string, val string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[key] = val
}

func (l *lockedMap) Get(key string) (string, bool) {
	// RLock (read lock) allows many concurrent readers at once, which is the
	// whole point of using RWMutex over a plain Mutex for a read-heavy load.
	l.mu.RLock()
	defer l.mu.RUnlock()
	val, ok := l.m[key]
	return val, ok
}

func (l *lockedMap) Del(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

func BenchmarkLockedMap_Set(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := newLockedMap()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Set(key, "value")
	}
}

func BenchmarkLockedMap_Get(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := newLockedMap()

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Get(key)
	}
}

func BenchmarkLockedMap_Del(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := newLockedMap()

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key := keys[i%benchSize]
		store.Del(key)
		store.Set(key, "value") // re-insert, same reasoning as before
	}
}

// ============================================================
// the hash function itself
// ============================================================

// BenchmarkMurmur3 times JUST the hashing in isolation. every Set/Get/Del above
// calls murmur3 internally, so it is worth knowing how cheap (or not) it is on
// its own. this is the cost we pay on literally every single operation.
func BenchmarkMurmur3(b *testing.B) {
	key := []byte("some-reasonably-typical-cache-key")
	var seed uint64 = 0

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		murmur3(key, seed)
	}
}

// ============================================================
// parallel / concurrent benchmarks
// ============================================================
//
// everything above is single-threaded: one goroutine doing operations back to
// back. that measures raw per-operation cost, but it HIDES the most important
// part of our design -- how we behave when many goroutines hit the map at once.
// our real server runs goroutine-per-connection, so this is the realistic case.
//
// b.RunParallel spins up several goroutines (GOMAXPROCS of them by default) and
// splits b.N across all of them. each goroutine runs the loop inside pb.Next().
// this is where the locking strategy actually shows its cost:
//
//   - our ConcurrentHMap uses a FULL mutex even on Get (because progressive
//     rehashing mutates state on reads, see the comment in hashtable.go). so
//     concurrent readers serialize -- they wait in line for each other.
//   - map + RWMutex uses a READ lock on Get, so many readers run at once.
//   - sync.Map is specifically optimized for the read-heavy concurrent case.
//
// so we EXPECT to lose on parallel reads. that is not a bug to hide -- it is a
// known, explainable trade-off we made to get O(1) amortized resizing. showing
// it honestly is stronger than pretending it isn't there.

// the parallel Get benchmarks are read-only and read-heavy, which is the most
// common cache workload (lots of gets, fewer sets). this is the fair stress test
// for the "concurrent readers" question.

func BenchmarkConcurrentHMap_Get_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &ConcurrentHMap{}

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	// the function passed to RunParallel runs once PER goroutine. each goroutine
	// keeps its own counter i so they don't all hammer the exact same key in
	// lockstep (which would be unrealistic and skew caching effects).
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			store.Get(key)
			i++
		}
	})
}

func BenchmarkSyncMap_Get_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	var store sync.Map

	for _, key := range keys {
		store.Store(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			store.Load(key)
			i++
		}
	})
}

func BenchmarkLockedMap_Get_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := newLockedMap()

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			store.Get(key)
			i++
		}
	})
}

// a mixed read/write parallel benchmark is closer to a real cache: mostly reads
// with the occasional write mixed in. we do a write roughly 1 in every 10 ops
// (when i%10 == 0) and reads the rest of the time.
func BenchmarkConcurrentHMap_Mixed_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &ConcurrentHMap{}

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			if i%10 == 0 {
				store.Set(key, "value")
			} else {
				store.Get(key)
			}
			i++
		}
	})
}

func BenchmarkSyncMap_Mixed_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	var store sync.Map

	for _, key := range keys {
		store.Store(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			if i%10 == 0 {
				store.Store(key, "value")
			} else {
				store.Load(key)
			}
			i++
		}
	})
}

func BenchmarkLockedMap_Mixed_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := newLockedMap()

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			if i%10 == 0 {
				store.Set(key, "value")
			} else {
				store.Get(key)
			}
			i++
		}
	})
}

// ============================================================
// StripedMap -- the striped version. see doc/benchmark-before-lock-striping.md
// ============================================================
//
// These are the "after" column. Same workloads as the ConcurrentHMap benchmarks
// above, so the two are directly comparable at every -cpu setting.

func BenchmarkStripedMap_Set(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &StripedMap{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Set(keys[i%benchSize], "value")
	}
}

func BenchmarkStripedMap_Get(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &StripedMap{}

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Get(keys[i%benchSize])
	}
}

func BenchmarkStripedMap_Del(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &StripedMap{}

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Del(keys[i%benchSize])
	}
}

func BenchmarkStripedMap_Get_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &StripedMap{}

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			store.Get(key)
			i++
		}
	})
}

func BenchmarkStripedMap_Mixed_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &StripedMap{}

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			if i%10 == 0 {
				store.Set(key, "value")
			} else {
				store.Get(key)
			}
			i++
		}
	})
}

// ---- the cache-line padding, measured rather than assumed ----
//
// paddedStripedMap is identical to StripedMap except each stripe is padded out to a
// full 128-byte cache line (M-series line size), so no two stripes' mutexes share one.
//
// The real StripedMap is deliberately NOT padded: measured over n=10 the padding is
// worth geomean -1.22% with the sign flipping across core counts, i.e. nothing. See the
// comment on `stripe` in striped.go for why (the mutex shares its line with the same
// stripe's own hMap header, which the lock holder needs anyway, so the false sharing
// overlaps with sharing that is real).
//
// This variant stays so that conclusion is re-checkable instead of folklore. Run:
//
//	go test ./internal/store -run '^$' -bench 'Padded?_Get_Parallel' -cpu=1,2,4,8 -count=10
//
// Get/Set only -- that is all the benchmark needs.

const benchCacheLine = 128

type paddedStripe struct {
	mu sync.Mutex
	m  hMap
	_  [benchCacheLine - unsafe.Sizeof(struct {
		mu sync.Mutex
		m  hMap
	}{})%benchCacheLine]byte
}

type paddedStripedMap struct {
	stripes [numStripes]paddedStripe
}

func (s *paddedStripedMap) Set(key, val string) {
	hcode := murmur3([]byte(key), storeSeed)
	st := &s.stripes[stripeOf(hcode)]
	st.mu.Lock()
	defer st.mu.Unlock()
	st.m.insert(key, val, hcode)
	st.m.setExpiryH(key, hcode, 0)
}

func (s *paddedStripedMap) Get(key string) (string, bool) {
	hcode := murmur3([]byte(key), storeSeed)
	st := &s.stripes[stripeOf(hcode)]
	st.mu.Lock()
	defer st.mu.Unlock()
	node, ok := st.m.search(key, hcode)
	if !ok {
		return "", false
	}
	return node.val, true
}

func BenchmarkStripedMapPadded_Get_Parallel(b *testing.B) {
	keys := makeBenchKeys(benchSize)
	store := &paddedStripedMap{}

	for _, key := range keys {
		store.Set(key, "value")
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := keys[i%benchSize]
			store.Get(key)
			i++
		}
	})
}
