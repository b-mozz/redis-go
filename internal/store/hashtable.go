// hashtable.go
// Previously we implemented our read, delete, set with Go's built in Map,
// but rehashing causes latency, the bigger the table --> more latency (O(N)).
// To solve this, we will clone the redis hashMap. Progressive rehashing; using two tables.
//
// HOWEVER, I have checked Go's recent development of their default maps.
// They have switched to [swiss tables] --> open probing.
// This is significantly faster than chaining (O(N) worst case deletion), tho easy to maintain.
// It will be better to figure out a way to do progressive rehashing while maintaining advanced open probing algorithms.

// Note: #PERFORMANCE tag is used to note down critical design decisions
// this is subject to change after I run some benchmark alongside the C redis

package store

import (
	"sync"
	"time"
)

// ==== Hash Function ====
// We need a fast hash function. In the tutorial they advise:
// "Do not use cryptographic hash functions for hashtables because they are slow and overkill."
// We need something reliable and fast. Just good enough for hashTables.
// Hash functions for hashtables, such as FNV, Murmur.
// Redis previously used MurmurHash2, but recently moved to SipHash-1-2.
// We will go with murmur3 function.

// grabbed from source: https://github.com/aappleby/smhasher/blob/master/src/MurmurHash3.cpp
// #PERFORMANCE
func murmur3(key []byte, seed uint64) uint64 {
	data := key
	length := len(data)
	h := seed
	const c1, c2 uint64 = 0xcc9e2d51, 0x1b873593

	// body
	for i := length - length%4; i >= 4; i -= 4 {
		k := uint64(data[i-4]) | uint64(data[i-3])<<8 | uint64(data[i-2])<<16 | uint64(data[i-1])<<24
		k *= c1
		k = (k << 15) | (k >> 49)
		k *= c2
		h ^= k
		h = (h << 13) | (h >> 51)
		h = h*5 + 0xe6546b64
	}

	// tail
	var k uint64
	switch length & 3 {
	case 3:
		k ^= uint64(data[length-3]) << 16
		fallthrough
	case 2:
		k ^= uint64(data[length-2]) << 8
		fallthrough
	case 1:
		k ^= uint64(data[length-1])
		k *= c1
		k = (k << 15) | (k >> 49)
		k *= c2
		h ^= k
	}

	// finalization (avalanche)
	h ^= uint64(length)
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}

// ==== Fixed hashTable ====

// hnode is a node in the linked list chain
type hnode struct {
	key   string
	val   string
	hcode uint64 // for caching. we store the uint64 val from the hashFunction. if that matches, then we check is the string key matches
	next  *hnode

	// expireAt is an absolute deadline in Unix nanoseconds (time.Now().UnixNano()).
	// 0 means "never expires" — a real timestamp is always > 0, so 0 is a safe sentinel.
	// Absolute (not seconds-remaining) so it stays correct across restarts (AOF replay).
	expireAt int64
}

// expired reports if the key is past its deadline. now is passed in (not read here)
// so tests can fake the clock and the sweeper can read the clock once per tick.
func (n *hnode) expired(now int64) bool {
	return n.expireAt != 0 && now >= n.expireAt
}

// htab is a FIXED size hashTable (basically array of linked list HEADS)
// mask will be size of the hashTable - 1
// for example: 17 % 4 == 17 & uint64(3)
type hTab struct {
	tab []*hnode
	mask uint64 // bit operations are faster than multiplication and divison (CSCI 260)
	used int 
}

// initializer: we are keeping a separate to ensure the size is always power of 2 (for bitwise AND)
// param: n --> size of the hashTable
func newHTab(n int) (*hTab) {
	if n > 0 && (n & (n-1)) == 0 {
		return &hTab{
			tab: make([]*hnode, n), // #PERFORMANCE. avoids O(N) initialization latency by default. 
			mask: uint64(n - 1),
			used: 0, // why is size 0? shouldn't it be n?? 
		}
	}

	panic("hTab size must be power of 2")
}

// insert: inserts a node
// new nodes go at the front of the chain: O(1) worst case
func (ht *hTab) insert(node *hnode) {
	pos := node.hcode & ht.mask // hcode: cached hashValue
	node.next = ht.tab[pos] // point to current head
	ht.tab[pos] = node
	ht.used++
}

// function search
//params: key, and our hashCode
func (ht *hTab) search(key string, hcode uint64) *hnode {
	if ht.tab == nil {
		return nil
	}

	pos := hcode & ht.mask
	cur := ht.tab[pos]

	for cur != nil {
		if cur.hcode == hcode && cur.key == key {
			return cur
		}
		cur = cur.next
	}

	return nil 

}

// function delete
func (ht *hTab) delete(key string, hcode uint64) *hnode {
	if ht.tab == nil {
		return nil
	}

	pos := hcode & ht.mask
	
	// rest is basic leetcode
	var prev *hnode // nil pointer
	cur := ht.tab[pos]

	for cur != nil {
		if cur.hcode == hcode && cur.key == key {
			if prev == nil {
				// the head node is the one we want to delete
				ht.tab[pos] = cur.next
			} else {
				prev.next = cur.next // skipping current altogether	
			}
			cur.next = nil
			ht.used--
			return cur
		} 
		prev = cur
		cur = cur.next
	}

	return nil

}

// sweepBucket walks the chain at bucket `pos` and unlinks every expired node.
// It returns how many nodes it evicted. `now` is the current time in Unix nanos.
// This is the same relink-while-walking pattern as delete(), just applied to
// every expired node in the chain instead of one matching key.
func (ht *hTab) sweepBucket(pos int, now int64) int {
	evicted := 0
	var prev *hnode
	cur := ht.tab[pos]

	for cur != nil {
		if cur.expired(now) {
			next := cur.next

			if prev == nil {
				ht.tab[pos] = next // head was expired, move head forward
			} else {
				prev.next = next // skip over the expired node
			}

			cur.next = nil // cut the evicted node loose
			ht.used--
			evicted++
			cur = next // prev stays put: we removed cur, so its predecessor is unchanged
		} else {
			prev = cur
			cur = cur.next
		}
	}

	return evicted
}


// now we need to implement REHASHING

// Hamp is the resizable hashTable
// during rehashing, both new and old are active
type hMap struct {
	new *hTab
	old *hTab
	migratepos int // next old position to migrate from; to new
	seed uint64 // seed for our hashing function

	// sweepCursor is the next bucket the active expirer (SweepExpired) will look at.
	// it lets the sweep resume where it left off instead of scanning from 0 each time.
	sweepCursor int
}

const (
    maxLoadFactor = 8 // allow up to 8 keys per slot on average
    rehashingWork = 128 // keys to migrate per operation
)

// now we have to progressive rehashing
func (m *hMap) helpRehash() {

	// if older is empty, return, rehashing is done or not needed
	if m.old == nil {
		return
	}

	work := 0 // how much work we have done so far

	for work < rehashingWork && m.old.used > 0 {
		for m.migratepos <= int(m.old.mask) && m.old.tab[m.migratepos] == nil {
			m.migratepos++
		}
		if m.migratepos > int(m.old.mask) {
			break
		}

		// now we move the first node from this slot to the newer table
		slot := &m.old.tab[m.migratepos]
		node := *slot
		*slot = node.next // move to the next node
		node.next = nil //cut off what we have moved so far
		m.new.insert(node)
		m.old.used--
		work++
	}

	if m.old.used == 0 {
		// we have fully migrated, drop it
		m.old = nil
	}

}

// now we need a function to trigger rehashing
func (m *hMap) triggerRehashing() {
	m.old = m.new // may seem odd, why are we assigning the old to the new one?? I have written down the explanation on my obsidian note.
	m.new = newHTab(int(m.old.mask+1) * 2) // double the size and allocated new Memory. not related to whatever newer had before. old takes care of it now
	m.migratepos = 0
}

func (m *hMap) Search(key string) (*hnode, bool) {
	// first check in the new table
	// if new table is nil, OLD MUST BE Nil. check prev function, during rehashing we assign old to new (pointer assignment, so O(1) not linear)
	// thus, if new is nil, we can return early

	if m.new == nil {
		return nil, false
	}

	// now we first call the helper rehash function
	// why? cz we check the new table first, if the rehash already brings the node to the new table, we have one less operation to complete

	m.helpRehash() // we rehash first

	hcode := murmur3([]byte(key), m.seed)

	node := m.new.search(key, hcode) // check the new table first
	foundInOld := false

	if node == nil && m.old != nil {
		node = m.old.search(key, hcode)
		foundInOld = true // remember where we found it, so lazy eviction deletes from the right table
	}

	if node == nil {
		return nil, false
	}

	// lazy expiration: if the key is past its deadline, evict it right now and report
	// a miss. this way an expired key is never handed back, even before the background
	// sweeper (A3) gets a chance to clean it up.
	//
	// the expireAt != 0 test is deliberately repeated here, even though expired()
	// already does it. Go evaluates arguments eagerly, so without this guard the clock
	// is read BEFORE expired() gets a chance to short-circuit. time.Now() costs ~32ns
	// on darwin/arm64 — half of a whole lookup — and most keys carry no TTL at all.
	// see doc/benchmark-before-lock-striping.md
	if node.expireAt != 0 && node.expired(time.Now().UnixNano()) {
		if foundInOld {
			m.old.delete(key, hcode)
		} else {
			m.new.delete(key, hcode)
		}
		return nil, false
	}

	return node, true

}

// now insert 

func (m *hMap) Insert (key string, val string) {
	if m.new == nil {
		// first insert: start the new table (old is nil too at this point)
		m.new = newHTab(4)
	}

	// if the key already exists, update the value only (google upsert. its a thing, i also did for a prev project)
	hcode := murmur3([]byte(key), m.seed)
	node, ok := m.Search(key)

	if ok {
		node.val = val
		return
	}

	insertNode := &hnode{key: key, val: val, hcode: hcode, next : nil}
	m.new.insert(insertNode)

	// but do we need to trigger rehashing??

	if m.old == nil {
		// m.old == nil means rehashing is not triggered so we check if we need to or not
		threshold := int(m.new.mask+1) * maxLoadFactor

		if m.new.used >= threshold {
			m.triggerRehashing()
		}
	}

	m.helpRehash()
}

func (m *hMap) Delete (key string) bool { // we are not returning the deleted element: why? cz Redis does not, and go's default map's delete doesnt even return a bool
	if m.new == nil {
		// we have no element 
		return false
	}
	m.helpRehash()

	hcode := murmur3([]byte(key), m.seed)
	node := m.new.delete(key, hcode)

	if node == nil && m.old != nil {
		// also check the old table is it exists and we have not found anything from the new table
		node = m.old.delete(key, hcode)
	}

	return node != nil
}

// setExpiry sets the absolute deadline (Unix nanos) on an existing key.
// pass expireAt = 0 to clear the TTL (make the key immortal again).
// returns false if the key doesn't exist. NOT thread-safe on its own — callers
// go through the ConcurrentHMap wrappers which hold the lock.
func (m *hMap) setExpiry(key string, expireAt int64) bool {
	node, ok := m.Search(key)
	if !ok {
		return false
	}
	node.expireAt = expireAt
	return true
}

// now we are done with progressive rehashing
// next: concurrent safety

// go uses goroutine, we need thread safety

// ========= thread safety ===========
// hMap is not safe for concurrent use: two goroutines hitting Insert/Delete/Search
// at the same time can race on the buckets. This is especially dangerous during
// progressive rehashing, where even a "read" can advance the migration and
// mutate internal state (so a plain RWMutex wouldn't be enough).
//
// ConcurrentHMap wraps hMap with a mutex so callers can share one instance
// across goroutines safely. Every method that touches m must Lock() first and
// Unlock() when done — the idiomatic pattern is:
//
//	func (c *ConcurrentHMap) Insert(k, v string) {
//	    c.mu.Lock()
//	    defer c.mu.Unlock()
//	    c.m.Insert(k, v)
//	}
//
// Always pass *ConcurrentHMap around — never copy by value, because sync.Mutex
// must not be copied after first use (go vet will flag this).
type ConcurrentHMap struct {
	// mu serializes access to m: at most one goroutine can hold it, and any
	// other goroutine calling Lock() blocks until Unlock() is called. It's
	// held for the duration of any Insert / Delete / Search so only one
	// operation touches the table at a time.
	//
	// Declared as a value (not *sync.Mutex) because sync.Mutex's zero value is
	// a valid, unlocked mutex — no constructor or explicit init needed;
	// `var c ConcurrentHMap` is ready to use. It lives on the struct (rather
	// than as a package-level lock) so the lock's scope matches the data it
	// protects, and two independent ConcurrentHMaps don't needlessly block
	// each other.
	mu sync.Mutex // guards m. must be held for any read or write of m
	m  hMap
}


// wrapper for Search function
func (c *ConcurrentHMap) Get (key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	node, ok := c.m.Search(key)
	if !ok {
		return "", false
	}

	return node.val, true
}

// wrapper for insert function.
// a plain Set clears any existing TTL on the key — matches Redis, where SET without
// an expiry option makes the key persistent again.
func (c *ConcurrentHMap) Set (key string, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.m.Insert(key, val)
	c.m.setExpiry(key, 0) // drop any TTL the old value may have had
}

// SetTTL stores key/val with an expiry of ttlSeconds from now.
// backs the `set key val EX seconds` command.
func (c *ConcurrentHMap) SetTTL (key string, val string, ttlSeconds int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.m.Insert(key, val)
	expireAt := time.Now().UnixNano() + ttlSeconds*int64(time.Second)
	c.m.setExpiry(key, expireAt)
}

// Expire sets a TTL of ttlSeconds on an existing key.
// returns true if the key exists (TTL applied), false otherwise. backs `expire`.
func (c *ConcurrentHMap) Expire (key string, ttlSeconds int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	expireAt := time.Now().UnixNano() + ttlSeconds*int64(time.Second)
	return c.m.setExpiry(key, expireAt)
}

// TTL reports the remaining life of a key in whole seconds. backs `ttl`.
// sentinels match Redis:  -1 = key exists but has no expiry,  -2 = key does not exist.
func (c *ConcurrentHMap) TTL (key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	node, ok := c.m.Search(key) // Search also lazily evicts the key if it's already expired
	if !ok {
		return -2 // no such key
	}
	if node.expireAt == 0 {
		return -1 // key exists but never expires
	}

	// Search guarantees the node isn't past its deadline, so remaining > 0.
	remaining := node.expireAt - time.Now().UnixNano()

	// round UP to whole seconds so a key with 0.3s left reports 1, not 0
	return (remaining + int64(time.Second) - 1) / int64(time.Second)
}

// Persist removes the TTL from a key, making it immortal again.
// returns true only if a TTL was actually removed. backs `persist`.
func (c *ConcurrentHMap) Persist (key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	node, ok := c.m.Search(key)
	if !ok || node.expireAt == 0 {
		return false // no such key, or it had no TTL to begin with
	}

	node.expireAt = 0
	return true
}

// wrapper for delete function
func (c *ConcurrentHMap) Del (key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	ok := c.m.Delete(key)

	return ok
}

// Size returns the total number of entries across both the new and old tables
// (during progressive rehashing, entries live in both).
func (c *ConcurrentHMap) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	if c.m.new != nil {
		n += c.m.new.used
	}
	if c.m.old != nil {
		n += c.m.old.used
	}
	return n
}

// SweepExpired actively evicts expired keys, doing a BOUNDED amount of work per call:
// it looks at at most `budget` buckets, starting from where the last call stopped
// (m.sweepCursor). This spreads expiry cleanup across many small calls instead of one
// O(N) stall — the same "a little at a time" idea as progressive rehashing. `now` is
// the current time in Unix nanos. It returns the number of keys evicted.
//
// It only sweeps the `new` table. Anything still sitting in `old` during a rehash is
// either migrated into `new` by helpRehash (where a later sweep will catch it) or
// evicted lazily on access, so nothing leaks permanently.
func (c *ConcurrentHMap) SweepExpired(now int64, budget int) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	ht := c.m.new
	if ht == nil {
		return 0 // map was never used, nothing to sweep
	}

	buckets := int(ht.mask) + 1
	evicted := 0

	for i := 0; i < budget; i++ {
		if c.m.sweepCursor >= buckets {
			c.m.sweepCursor = 0 // reached the end of the table, wrap back to the start
		}

		evicted += ht.sweepBucket(c.m.sweepCursor, now)
		c.m.sweepCursor++
	}

	return evicted
}

// ForEach walks every (key, value) pair in the map. The callback returns
// false to stop iteration early. The mutex is held for the entire walk,
// so the callback must not call back into the map (it would deadlock).
func (c *ConcurrentHMap) ForEach(fn func(key, val string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m.new != nil && !walk(c.m.new, fn) {
		return
	}
	if c.m.old != nil {
		walk(c.m.old, fn)
	}
}

func walk(ht *hTab, fn func(key, val string) bool) bool {
	for _, head := range ht.tab {
		for cur := head; cur != nil; cur = cur.next {
			if !fn(cur.key, cur.val) {
				return false
			}
		}
	}
	return true
}

// Keys returns a snapshot of every key in the map.
func (c *ConcurrentHMap) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0)
	if c.m.new != nil {
		collectKeys(c.m.new, &out)
	}
	if c.m.old != nil {
		collectKeys(c.m.old, &out)
	}
	return out
}

func collectKeys(ht *hTab, out *[]string) {
	for _, head := range ht.tab {
		for cur := head; cur != nil; cur = cur.next {
			*out = append(*out, cur.key)
		}
	}
}





















