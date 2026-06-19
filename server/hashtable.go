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

package main
import "sync"

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

	// if our table is not empty
	// we need the position
	pos := hcode & ht.mask
	cur := ht.tab[pos] // we start at position head for our key

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


// now we need to implement REHASHING

// Hamp is the resizable hashTable
// during rehashing, both new and old are active
type HMap struct {
	new *hTab
	old *hTab
	migratepos int // next old position to migrate from; to new
	seed uint64 // seed for our hashing function
}

const (
    maxLoadFactor = 8 // allow up to 8 keys per slot on average
    rehashingWork = 128 // keys to migrate per operation
)

// now we have to progressive rehashing
func (m *HMap) helpRehash() {

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
func (m *HMap) triggerRehashing() {
	m.old = m.new // may seem odd, why are we assigning the old to the new one?? I have written down the explanation on my obsidian note.
	m.new = newHTab(int(m.old.mask+1) * 2) // double the size and allocated new Memory. not related to whatever newer had before. old takes care of it now
	m.migratepos = 0
}

func (m *HMap) Search(key string) (*hnode, bool) {
	// first check in the new table
	// if new table is nil, OLD MUST BE Nil. check prev function, during rehashing we assign old to new (pointer assignment, so O(1) not linear)
	// thus, if new is nil, we can return early

	if m.new == nil {
		return nil, false
	}

	// now we first call the helper rehash function
	// why? cz we check the new table first, if the rehash already brings the node to the new table, we have one less operation to complete

	m.helpRehash() // we rehash first

	// now we need to have our hash code, we extract it from our key string
	hcode := murmur3([]byte(key), m.seed)	

	node := m.new.search(key, hcode) // as i have described before, e first checl the new table

	if node == nil && m.old != nil {
		node = m.old.search(key, hcode)
	}

	if node == nil {
		return nil, false
	}

	return node, true

}

// now insert 

func (m *HMap) Insert (key string, val string) {
	if m.new == nil {
		// our new table is nil, we need to start it
		// that means our old table is also nil
		// thus, we need to initiate our new table to insert
		m.new = newHTab(4)
	}

	// now check if the key already exists, if does update the value only (google upsert. its a thing, i also did for a prev project)
	hcode := murmur3([]byte(key), m.seed)
	node, ok := m.Search(key)

	if ok {
		node.val = val
		return
	}

	// if we are here, that means we have not found the key in our table(s)
	// insert
	insertNode := &hnode{key: key, val: val, hcode: hcode, next : nil}
	m.new.insert(insertNode)

	// ok we have already inserted
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

func (m *HMap) Delete (key string) bool { // we are not returning the deleted element: why? cz Redis does not, and go's default map's delete doesnt even return a bool
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

// now we are done with progressive rehashing
// next: concurrent safety

// go uses goroutine, we need thread safety

// ========= thread safety ===========
// HMap is not safe for concurrent use: two goroutines hitting Insert/Delete/Search
// at the same time can race on the buckets. This is especially dangerous during
// progressive rehashing, where even a "read" can advance the migration and
// mutate internal state (so a plain RWMutex wouldn't be enough).
//
// ConcurrentHMap wraps HMap with a mutex so callers can share one instance
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
	m  HMap
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

// wrapper for insert function
func (c *ConcurrentHMap) Set (key string, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.m.Insert(key, val)

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





















