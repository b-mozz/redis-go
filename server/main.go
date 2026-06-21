package main

import (
"encoding/binary"
"fmt"
"io"
"log"
"net"

"redis_go/proto"
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
	}

	proto.OutStr(out, val)
	return


case "set":
	if len(cmd) != 3 {
		proto.OutErr(out, proto.ErrArg, "get only accepts exactly 2 argument")

		store.Set(cmd[1], cmd[2]) // key and value
		proto.OutNil(out)
	}

case "del":
	if len(cmd) != 2 {
		proto.OutErr(out, proto.ErrArg, "get only accepts exactly 1 argument")	
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
	return fmt.Errorf("too amny arguments")
}

body := make([]byte, msgLen)

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


func main() {
	listener, err := net.Listen("tcp", ":1234")
	if err != nil {
		log.Fatal("listen:", err)
	}
	defer listener.Close()
	fmt.Println("listening on :1234")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}
		go handleConn(conn)
	}
}







