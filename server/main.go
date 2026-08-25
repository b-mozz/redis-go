package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"redis_go/internal/store"
	"redis_go/resp"
)

const (
	// how often the background expirer wakes up, and how many buckets it scans each time.
	// small budget per tick keeps each pass cheap; over many ticks it covers the whole table.
	sweepInterval = 100 * time.Millisecond
	sweepBudget   = 20
)

// key value store.
// named db, not store, because `store` is now the package name (redis_go/internal/store).
var db = &store.ShardedMap{}

func doRequest(argv []string, w *resp.Writer) {
	if len(argv) == 0 {
		return
	}

	name := strings.ToUpper(argv[0])

	switch name {
	case "GET":
		if len(argv) != 2 {
			w.Err(wrongArgs(name))
			return
		}

		val, ok := db.Get(argv[1])
		if !ok {
			w.Nil()
			return
		}

		w.Bulk(val)

	case "SET":
		// two accepted forms:
		//   SET key val            -> store with no expiry
		//   SET key val EX seconds -> store, then expire after `seconds`
		if len(argv) != 3 && len(argv) != 5 {
			w.Err(wrongArgs(name))
			return
		}

		if len(argv) == 3 {
			db.Set(argv[1], argv[2])
			w.OK()
			return
		}

		if !strings.EqualFold(argv[3], "EX") {
			w.Err("ERR syntax error")
			return
		}

		seconds, err := strconv.ParseInt(argv[4], 10, 64)
		if err != nil {
			w.Err(errNotInteger)
			return
		}
		if seconds <= 0 {
			w.Err("ERR invalid expire time in 'set' command")
			return
		}

		db.SetTTL(argv[1], argv[2], seconds)
		w.OK()

	case "EXPIRE":
		if len(argv) != 3 {
			w.Err(wrongArgs(name))
			return
		}

		seconds, err := strconv.ParseInt(argv[2], 10, 64)
		if err != nil {
			w.Err(errNotInteger)
			return
		}
		if seconds <= 0 {
			w.Err("ERR invalid expire time in 'expire' command")
			return
		}

		if db.Expire(argv[1], seconds) {
			w.Int(1)
		} else {
			w.Int(0)
		}

	case "TTL":
		if len(argv) != 2 {
			w.Err(wrongArgs(name))
			return
		}

		w.Int(db.TTL(argv[1]))

	case "PERSIST":
		if len(argv) != 2 {
			w.Err(wrongArgs(name))
			return
		}

		if db.Persist(argv[1]) {
			w.Int(1)
		} else {
			w.Int(0)
		}

	case "DEL":
		// variadic: DEL a b c -> the number actually removed
		if len(argv) < 2 {
			w.Err(wrongArgs(name))
			return
		}

		var n int64
		for _, key := range argv[1:] {
			if db.Del(key) {
				n++
			}
		}

		w.Int(n)

	case "EXISTS":
		if len(argv) < 2 {
			w.Err(wrongArgs(name))
			return
		}

		var n int64
		for _, key := range argv[1:] {
			if _, ok := db.Get(key); ok {
				n++
			}
		}

		w.Int(n)

	case "KEYS":
		// only the "*" pattern is supported; glob matching comes later.
		if len(argv) != 2 {
			w.Err(wrongArgs(name))
			return
		}

		keys := db.Keys()
		w.Arr(len(keys))
		for _, k := range keys {
			w.Bulk(k)
		}

	case "DBSIZE":
		if len(argv) != 1 {
			w.Err(wrongArgs(name))
			return
		}

		w.Int(int64(db.Size()))

	case "PING":
		switch len(argv) {
		case 1:
			w.Simple("PONG")
		case 2:
			w.Bulk(argv[1])
		default:
			w.Err(wrongArgs(name))
		}

	case "ECHO":
		if len(argv) != 2 {
			w.Err(wrongArgs(name))
			return
		}

		w.Bulk(argv[1])

	case "COMMAND":
		// redis-cli sends COMMAND DOCS on connect; an empty array satisfies it.
		w.Arr(0)

	case "SELECT":
		if len(argv) != 2 {
			w.Err(wrongArgs(name))
			return
		}
		if argv[1] != "0" {
			w.Err("ERR DB index is out of range")
			return
		}

		w.OK()

	case "CONFIG":
		// CONFIG GET <param> -> empty array rather than an error, which is what
		// client libraries expect for an unknown parameter.
		if len(argv) >= 2 && strings.EqualFold(argv[1], "GET") {
			w.Arr(0)
			return
		}

		w.Err(unknownCommand(argv))

	default:
		w.Err(unknownCommand(argv))
	}
}

const errNotInteger = "ERR value is not an integer or out of range"

func wrongArgs(name string) string {
	return fmt.Sprintf("ERR wrong number of arguments for '%s' command", strings.ToLower(name))
}

func unknownCommand(argv []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ERR unknown command '%s', with args beginning with: ", argv[0])
	for _, a := range argv[1:] {
		fmt.Fprintf(&b, "'%s', ", a)
	}
	return b.String()
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	rd := resp.NewReader(conn)
	w := resp.NewWriter(conn)

	for {
		argv, err := rd.ReadCommand()
		if err != nil {
			// A protocol error is terminal: framing is lost, so there is no way
			// to find the start of the next command. Reply, then hang up.
			var pe *resp.ProtocolError
			if errors.As(err, &pe) {
				w.Err(pe.Error())
				w.Flush()
			}
			return
		}

		if len(argv) == 0 {
			continue
		}

		doRequest(argv, w)

		// Nothing left to parse means the next read blocks, so the replies have
		// to be on the wire before we get there.
		if rd.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

// startExpiryLoop runs the active expirer in the background. every sweepInterval it
// asks the store to evict a bounded number of expired keys. this is a supplement to
// lazy expiration (in Search): lazy handles keys that get accessed, this handles keys
// that are set-and-forgotten so they don't sit in memory forever.
func startExpiryLoop() {
	ticker := time.NewTicker(sweepInterval)

	// run the loop on a goroutine so it ticks in the background instead of blocking
	// startup. for range over ticker.C fires the body once per tick, forever.
	go func() {
		for range ticker.C {
			db.SweepExpired(time.Now().UnixNano(), sweepBudget)
		}
	}()
}

func main() {
	port := flag.Int("port", 6379, "port to listen on")
	flag.Parse()

	addr := fmt.Sprintf(":%d", *port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("listen:", err)
	}
	defer listener.Close()
	fmt.Println("listening on", addr)

	startExpiryLoop() // begin background eviction of expired keys

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}
		go handleConn(conn)
	}
}
