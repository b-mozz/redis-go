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





