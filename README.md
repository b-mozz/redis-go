<h1 align="center">redis_go</h1>

<p align="center">
  A Redis server written from scratch in Go — custom hash table, RESP protocol,<br>
  TTL expiry, lock-striped concurrent store. Speaks to the real <code>redis-cli</code>.
</p>

<p align="center">
  <a href="https://github.com/b-mozz/redis-go/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/b-mozz/redis-go/ci.yml?branch=main&label=CI&style=flat-square&logo=githubactions&logoColor=white"></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/b-mozz/redis-go?style=flat-square&logo=go&logoColor=white"></a>
  <a href="https://pkg.go.dev/github.com/b-mozz/redis-go"><img alt="Go reference" src="https://img.shields.io/badge/pkg.go.dev-reference-007d9c?style=flat-square&logo=go&logoColor=white"></a>
</p>

<p align="center">
  <a href="#benchmarks"><img alt="Dependencies" src="https://img.shields.io/badge/dependencies-0-brightgreen?style=flat-square"></a>
  <a href="#benchmarks"><img alt="Allocations" src="https://img.shields.io/badge/allocations-0%20per%20op-brightgreen?style=flat-square"></a>
  <a href="#tests"><img alt="Tests" src="https://img.shields.io/badge/tests-race%20detector-8A2BE2?style=flat-square"></a>
  <a href="#benchmarks"><img alt="Protocol" src="https://img.shields.io/badge/protocol-RESP-DC382D?style=flat-square&logo=redis&logoColor=white"></a>
</p>

<p align="center">
  <img alt="Top language" src="https://img.shields.io/github/languages/top/b-mozz/redis-go?style=flat-square">
  <img alt="Code size" src="https://img.shields.io/github/languages/code-size/b-mozz/redis-go?style=flat-square">
  <img alt="Last commit" src="https://img.shields.io/github/last-commit/b-mozz/redis-go?style=flat-square">
</p>

## Benchmarks

Apple M2, 8 cores · Go 1.25 · `benchstat` n=10.

### vs. real redis-server

`redis-benchmark -t set,get -n 200000 -c 50`, against `redis-server 8.10.1` with
persistence off:

| | SET ops/s | GET ops/s |
|---|---|---|
| `redis-server` (C) | 169,635 | 167,504 |
| **`redis_go`** | **148,039** | **148,368** |

**Within ~13% of C Redis** on the unpipelined path.

### Lock striping

The store is split across 32 stripes, each with its own mutex and its own hash table,
selected by the high bits of the key's hash. Parallel `Get`, ns/op — lower is better:

| cores | 1 mutex | 32 stripes | speedup |
|---|---|---|---|
| 2 | 55.6 | **24.9** | 2.2x |
| 4 | 109.2 | **17.3** | **6.3x** |
| 8 | 131.2 | **22.0** | **6.0x** |

The number that matters is aggregate throughput as cores are added:

| cores | 1 mutex | 32 stripes |
|---|---|---|
| 2 | 0.69x | **1.71x** |
| 4 | 0.35x | **2.45x** |
| 8 | **0.29x** | **1.93x** |

With one lock, adding seven cores made the store **3.4x slower** — every core fighting
for the same cache line. Striping turns that around: cores now add throughput.

Both read and write paths are **zero-allocation** (`0 B/op`, `0 allocs/op`).

Caveats worth stating: `sync.Map` still beats this on pure reads (it takes no lock at
all), striping costs ~11% single-threaded, and end-to-end the server is syscall-bound —
so this win shows up in the store today, not yet at the socket.

```sh
go test -bench=. -benchmem ./internal/store/
```

## Why

To understand how an in-memory database actually works instead of just using one —
hashing and collision handling, why Redis rehashes progressively instead of all at once,
how RESP frames messages over TCP, and how to make a data structure safe under
goroutines. Depth over breadth: few features, each one built properly.

Learning project, not a production database.

## Layout

```
server/          TCP server: accept loop, command dispatch, expiry loop
internal/store/  the hash table and its concurrency
resp/            RESP protocol reader and writer
```

- **Hash table** — MurmurHash3, separate chaining.
- **Progressive rehashing** — on resize, keys migrate a few per operation instead of one
  O(N) pass, so a grow never stalls the server.
- **Lock striping** — `StripedMap` fans the keyspace across 32 independent stripes. Each
  uses a plain `Mutex`, not an `RWMutex`, because progressive rehashing means even a read
  mutates the table.
- **Expiry** — lazily on access, plus a background sweeper that does a bounded number of
  buckets per tick.

## Running it

Requires Go 1.25+.

```sh
go run ./server          # listens on :6379
```

Then use the real Redis client:

```sh
redis-cli set name bimukti
redis-cli get name
redis-cli set session abc EX 60
redis-cli ttl session
```

## Commands

`get` · `set [EX seconds]` · `del` · `exists` · `keys` · `dbsize` · `expire` · `ttl` ·
`persist` · `ping` · `echo`

`ttl` returns seconds remaining, `-1` if the key has no expiry, `-2` if it does not
exist. `keys` currently only honours `*`.

## Tests

```sh
go test -race ./...
```

Covers TTL and expiry semantics, RESP round-trips and fuzzing, and concurrency stress
tests run under the race detector — plus stripe distribution, concurrent per-stripe
rehashing, and sweep coverage across stripes. CI runs `go vet`, `go build`, and
`go test -race` on every push.

## Next

- **The connection path** — the real bottleneck: syscalls per command, buffering, and
  per-command allocations in the RESP reader.
- `incr` / `decr`, glob matching for `keys`, sorted sets.
- Persistence and replication.
