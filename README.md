# redis_go

This is a learning project, not a production database. A small Redis-inspired in-memory key-value store written in Go, including a custom
hash table, a typed binary wire protocol, and a concurrent TCP server. 

## Why this project

I wanted to understand how an in-memory database works under the hood instead of
just using `redis-cli`. So rather than wrapping Go's built-in `map`, I tried to
implement the core pieces myself to learn the trade-offs involved:

- how a hash table is built (hashing, collision chaining, load factor, resizing)
- why Redis uses progressive rehashing instead of resizing all at once
- how to design a length-prefixed binary protocol and frame messages over TCP
- how to make a data structure safe for concurrent use with goroutines
- how to benchmark my own code against the standard library

The aim was depth over breadth: a small feature set, but a genuine attempt to
understand each piece.

## Architecture

```
client/      CLI client: builds a request, sends it, decodes the typed response
server/      TCP server + the custom hash table (ConcurrentHMap)
proto/       binary wire protocol: typed writers, framing, and a reader
doc/         design notes (todo list, Go pointer/value notes)
```

Design notes:

- **Custom hash table** (`server/hashtable.go`) using MurmurHash3 for hashing and
  separate chaining for collisions.
- **Progressive rehashing**: when the table grows, keys are migrated from the old
  table to the new one a few at a time on each operation, instead of in a single
  O(N) pass. The intent is to avoid a latency spike during a resize.
- **Concurrency**: `ConcurrentHMap` wraps the table in a mutex so it can be shared
  across goroutines. The server runs one goroutine per connection.
- **Typed binary protocol** (`proto/proto.go`): each value carries a type tag (nil,
  error, string, int, double, array), and each message is prefixed with a u32
  length so the reader knows how many bytes to expect.

## Running it

Requires Go 1.25+.

Start the server (listens on `:1234`):

```
go run ./server
```

In another terminal, run commands with the client:

```
go run ./client set name bimukti
go run ./client get name
go run ./client keys
go run ./client del name
```

## Supported commands

| Command | Description | Response |
|---|---|---|
| `set <key> <value> [EX <seconds>]` | store a key/value pair, optionally with a TTL | nil |
| `get <key>` | look up a key | string, or nil if missing |
| `del <key>` | delete a key | int: 1 if deleted, 0 if not found |
| `keys` | list all keys | array of strings |
| `expire <key> <seconds>` | attach a TTL to an existing key | int: 1 if key exists, 0 if not |
| `ttl <key>` | remaining life of a key | int: seconds left, `-1` if no TTL, `-2` if missing |
| `persist <key>` | remove a key's TTL | int: 1 if a TTL was removed, 0 otherwise |

## Expiration

Keys can carry a TTL, stored on each node as an absolute deadline. Expiry happens
two ways, mirroring Redis:

- **Lazy**: `get` (and any lookup) checks the deadline and evicts an expired key on
  access, so an expired value is never returned.
- **Active**: a background goroutine periodically sweeps a *bounded* number of buckets
  per tick and evicts expired keys, so set-and-forgotten keys don't linger in memory.
  This reuses the same "a little work at a time" idea as progressive rehashing rather
  than doing one O(N) scan.

## Benchmarks

Run them with:

```
go test -bench=. -benchmem ./server/
```

The custom `ConcurrentHMap` is compared against two common alternatives in Go:
`sync.Map` and a plain `map` guarded by an `RWMutex`. The goal is to measure the
data structure itself, which is why this does not benchmark against real Redis (an
end-to-end comparison would mostly measure network and protocol overhead, and Redis
is a mature C codebase that this isn't trying to compete with).

These numbers are from one machine (Apple M2) and are meant to show rough
trade-offs, not precise figures.

Single-threaded:

| Operation | ConcurrentHMap | sync.Map | map + RWMutex |
|---|---|---|---|
| Set | 42.8 ns/op | 72.8 ns/op | 29.3 ns/op |
| Get | 36.4 ns/op | 20.1 ns/op | 17.2 ns/op |
| Del | 98.4 ns/op | 99.3 ns/op | 59.3 ns/op |

Parallel (8 goroutines):

| Operation | ConcurrentHMap | sync.Map | map + RWMutex |
|---|---|---|---|
| Get | 126.3 ns/op | 3.8 ns/op | 77.7 ns/op |
| Mixed (90% read) | 136.1 ns/op | 9.2 ns/op | 54.3 ns/op |

A few things stand out:

- On single-threaded `Set`, `ConcurrentHMap` does reasonably well and avoids
  per-operation allocations, though a plain `map + RWMutex` is still faster.
- Under concurrent load, reads do not scale. `Get` takes a full mutex rather than a
  read lock, because progressive rehashing can mutate internal state during a read,
  so concurrent readers end up serialized. `sync.Map`, which is optimized for
  lock-free reads, is much faster here. This is a limitation of the current design
  and is the first item on the roadmap below.

## Planned improvements

Performance:

- **Shard the map (lock striping)** to address the concurrent-read bottleneck: split
  the keyspace across several independent sub-maps, each with its own lock, so reads
  on different shards don't block each other.
- Look into lock-free reads (taking the lock only when actually advancing
  migration), which could help `Get` scale under concurrency.

Features:

- interactive REPL client (read commands from stdin instead of one-shot CLI args)
- `exists`, `incr` / `decr` commands
- sorted set commands (`zadd`, `zrange`, `zscore`) — needs a new data structure

Testing:

- round-trip tests for `parseReq` and `ReadValue` / `Out*` symmetry
- concurrent stress tests for `ConcurrentHMap` under simultaneous set/del

## Notes

This is a work in progress and not complete. 