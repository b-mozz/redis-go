package main

import (
"encoding/binary"
"fmt"
"io"
"log"
"net"
"strconv"
"strings"
"time"

"redis_go/proto"
)

const (
// how often the background expirer wakes up, and how many buckets it scans each time.
// small budget per tick keeps each pass cheap; over many ticks it covers the whole table.
sweepInterval = 100 * time.Millisecond
sweepBudget   = 20
)

const (
maxMsg = 32 << 20 // description in proto.go
maxArgs = 200
)


//  server/main.go doesn't need pointers because nothing there is append-ing. 
// The moment we wire in proto.Out* calls (in doRequest), we do use *[]byte — because that's where growth happens. The rule is
// consistent: pointer when growing, value when reading or in-place writing.


// request parser, calling it reader
type reader struct {
data []byte 
pos int // what position we are at rn
}

func (r *reader) readU32() (uint32, error) {
if r.pos + 4 > len(r.data) {
	return 0, fmt.Errorf("unexpected end of data")
}

val := binary.LittleEndian.Uint32(r.data[r.pos : r.pos + 4])
r.pos += 4
return val, nil
}

// to read strings, param: n --> length of the string
func (r *reader) readStr(n int) (string, error) {
if r.pos + n > len(r.data) {
	return "", fmt.Errorf("unexpected end of data")
}

val := string(r.data[r.pos : r.pos + n])
r.pos += n
return val, nil
}

// parseReq parses the request
// one request (param data) contains one command at a time
// nstr -> number of strings in the request
//   ┌─────┬─────┬──────┬─────┬──────┬─────┬─────────┐
//   │  3  │  3  │ "get"│  4  │ "name│  0  │   ""    │
//   │nstr │ len │ str  │ len │ "    │ len │ (empty) │
//   └─────┴─────┴──────┴─────┴──────┴─────┴─────────┘
//     4B    4B    3B    4B    4B     4B     0B
func parseReq (data []byte) ([]string, error){
r := &reader{data : data}

nstr, err := r.readU32() // nstr: number of arguments 

if err != nil {
	return nil, err
}

if nstr > maxArgs {
	return nil, fmt.Errorf("too many args: %d", nstr)
}

cmd := make([]string, 0, nstr) // make([]string, nstr) also works

for i := uint32(0); i < nstr; i++ {
	n, err := r.readU32()

	if err != nil {
		return nil, err
	}

	s, err := r.readStr(int(n))

	if err != nil {
		return nil, err
	}

	cmd = append(cmd, s)
}

return cmd, nil
}


// key value store
var store = & ConcurrentHMap{}

// doRequest writes a typed response into out using the proto package.
// One Out* call per response — caller wraps with ResponseBegin/End.
// we check command then append on out slice based on our need
func doRequest(cmd []string, out *[]byte) {
if len(cmd) == 0 {
	proto.OutErr(out, proto.ErrUnknown, "empty command")
	return
}

// now cmd[0] is always the command (get, set, del, etc)
// we can use if - else statements to check different commands, but lets use switch here
switch cmd[0] {
case "get":
	if len(cmd) != 2 {
		proto.OutErr(out, proto.ErrArg, "get only accepts exactly 1 argument")
		return
	}

	val, ok := store.Get(cmd[1]) // cmd 1 is the key

	if !ok {
		proto.OutNil(out)
		return
	}

	proto.OutStr(out, val)
	return


case "set":
	// two accepted forms:
	//   set key val            -> store with no expiry
	//   set key val EX seconds -> store, then expire after `seconds`
	if len(cmd) != 3 && len(cmd) != 5 {
		proto.OutErr(out, proto.ErrArg, "set accepts: set key val [EX seconds]")
		return
	}

	// plain set, no TTL
	if len(cmd) == 3 {
		store.Set(cmd[1], cmd[2])
		proto.OutNil(out)
		return
	}

	// len == 5: the 4th token must be EX (case-insensitive), the 5th the seconds
	if !strings.EqualFold(cmd[3], "EX") {
		proto.OutErr(out, proto.ErrArg, "set: expected EX before the seconds value")
		return
	}

	seconds, err := strconv.ParseInt(cmd[4], 10, 64)
	if err != nil || seconds <= 0 {
		proto.OutErr(out, proto.ErrArg, "set: EX seconds must be a positive integer")
		return
	}

	store.SetTTL(cmd[1], cmd[2], seconds)
	proto.OutNil(out)
	return

case "expire":
	// expire key seconds -> attach a TTL to an existing key
	if len(cmd) != 3 {
		proto.OutErr(out, proto.ErrArg, "expire accepts exactly 2 arguments: expire key seconds")
		return
	}

	seconds, err := strconv.ParseInt(cmd[2], 10, 64)
	if err != nil || seconds <= 0 {
		proto.OutErr(out, proto.ErrArg, "expire: seconds must be a positive integer")
		return
	}

	if store.Expire(cmd[1], seconds) {
		proto.OutInt(out, 1) // key existed, TTL set
	} else {
		proto.OutInt(out, 0) // no such key
	}

case "ttl":
	// ttl key -> remaining seconds, or -1 (no expiry) / -2 (missing)
	if len(cmd) != 2 {
		proto.OutErr(out, proto.ErrArg, "ttl only accepts exactly 1 argument")
		return
	}

	proto.OutInt(out, store.TTL(cmd[1]))

case "persist":
	// persist key -> strip the TTL so the key stops expiring
	if len(cmd) != 2 {
		proto.OutErr(out, proto.ErrArg, "persist only accepts exactly 1 argument")
		return
	}

	if store.Persist(cmd[1]) {
		proto.OutInt(out, 1) // a TTL was removed
	} else {
		proto.OutInt(out, 0) // no such key, or it had no TTL
	}

case "del":
	if len(cmd) != 2 {
		proto.OutErr(out, proto.ErrArg, "del only accepts exactly 1 argument")
		return
	}

	if store.Del(cmd[1]) {
		proto.OutInt(out, 1)
	} else {
		proto.OutInt(out, 0)
	}
	

case "keys":
	keys := store.Keys() // keys return a slice
	proto.OutArr(out, uint32(len(keys))) // first we update the header. "1 array coming of n length"

	for _, k := range keys {
		proto.OutStr(out, k)
	}
default:
	proto.OutErr(out, proto.ErrUnknown, "unknown command")
}

}

// networking
func oneRequest(conn net.Conn) error {
header := make([]byte, 4)

_, err := io.ReadFull(conn, header)

if err != nil {
	if err == io.EOF { // end of data stream
		return io.EOF
	}
	return fmt.Errorf("header error: %w", err)
}

msgLen := binary.LittleEndian.Uint32(header)

if msgLen > maxMsg {
	return fmt.Errorf("message too large: %d", msgLen)
}

body := make([]byte, msgLen)
_, err = io.ReadFull(conn, body)
if err != nil {
	return fmt.Errorf("body error: %w", err)
}

cmd, err := parseReq(body)

if err != nil {
	return fmt.Errorf("req parsing error: %w", err)
}

// build response with proto framing
out := make([]byte, 0, 64)
hdr := proto.ResponseBegin(&out)
doRequest(cmd, &out)
proto.ResponseEnd(&out, hdr)

_, err = conn.Write(out)
return err

}

func handleConn(conn net.Conn) {
	defer conn.Close()

	for {
		err := oneRequest(conn)

		if err != nil {
			if err != io.EOF {
				log.Println(err)
			}
			return 
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
			store.SweepExpired(time.Now().UnixNano(), sweepBudget)
		}
	}()
}

func main() {
	listener, err := net.Listen("tcp", ":1234")
	if err != nil {
		log.Fatal("listen:", err)
	}
	defer listener.Close()
	fmt.Println("listening on :1234")

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







