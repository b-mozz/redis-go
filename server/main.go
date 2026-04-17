package main

import(
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
)
const(
	maxMsg = 4096
	maxArgs = 200
	resOk = 0
	resNot = 1
	resErr = 2
)

//==parser==
type reader struct{
	data []byte
	pos int
}

//reader method 1
//reads nstr (number of strings) first. then keeps on reading length
func (r *reader) readU32() (uint32, error){
	if r.pos > len(r.data){
		return 0, fmt.Errorf("Unexpected end of data")
	}
	val := binary.LittleEndian.Uint32(r.data[r.pos : r.pos+4])

	r.pos += 4
	return val, nil

}

//reader method 2
//read strings
func (r *reader) readStr(n int) (string, error){
	if r.pos + n > len(r.data){
		return "", fmt.Errorf("Unexpected end of data")
	}

	msg := string(r.data[r.pos : r.pos+n])

	return msg, nil
}


//parse the request and store them in a list
func parseReq(data []byte) ([]string, error){
	r := &reader{data : data}

	//number of strings
	nstr, err := r.readU32()

	if err != nil{
		return nil, err // nil means no slice/empty result here
	}

	if nstr > maxArgs{
		return nil, fmt.Errorf("too many strings/args: %d", nstr)
	}

	cmd := make([]string, 0, nstr)

	for i := 0; i < int(nstr); i++ {
		// now we need to read the length of the string
		strLen, err := r.readU32()
		if err != nil{
			return nil, err
		}

		msg,err := r.readStr(int(strLen))

		if err != nil{
			return nil, err
		}

		cmd = append(cmd, msg)

	}

	return cmd, nil

}

// parsing section is complete
//===================================o===================o=======

//----KV Store-----
var store = map[string]string{}

type response struct{
	status uint32
	data []byte
}

func doRequest(cmd []string) response{
	if cmd[0] == "get" && len(cmd) == 2{
		val, ok := store[cmd[1]]

		if !ok{
			// key does not exist
			return response{status : resNot}
		}
		return response{status : resOk, data: []byte(val)}

	} else if cmd[0] == "set" && len(cmd) == 3{
		store[cmd[1]] = cmd[2]
		return response{status: resOk}
	} else if cmd[0] == "del" && len(cmd) == 2{
		delete(store, cmd[1])
		return response{status: resOk}
	}

	return response{status: resErr, data: []byte("Unknown command")}
}

// this function converts our response struct to raw bytes for TCP
func makeResponse(resp response) []byte{
	// in our raw bytes slice, we need to store the status and the data
	//status is always 4 byte
	//respLen = 4 + len(resp.data)

	respLen := uint32(4 + len(resp.data))

	buf := make([]byte, 4 + respLen)

	binary.LittleEndian.PutUint32(buf[0:4], respLen)
	binary.LittleEndian.PutUint32(buf[4:8], resp.status)

	copy(buf[8:], resp.data)

	return buf

}




//=============Networking========================
// --- networking ---
func oneRequest(conn net.Conn) error {
    header := make([]byte, 4)
    _, err := io.ReadFull(conn, header)
    if err != nil {
        if err == io.EOF {
            return fmt.Errorf("EOF")
        }
        return fmt.Errorf("read header: %w", err)
    }

    msgLen := binary.LittleEndian.Uint32(header)
    if msgLen > maxMsg {
        return fmt.Errorf("message too long: %d", msgLen)
    }

    body := make([]byte, msgLen)
    _, err = io.ReadFull(conn, body)
    if err != nil {
        return fmt.Errorf("read body: %w", err)
    }

    cmd, err := parseReq(body)
    if err != nil {
        return fmt.Errorf("bad request: %w", err)
    }

    resp := doRequest(cmd)
    _, err = conn.Write(makeResponse(resp))
    return err
}

func handleConn(conn net.Conn){
	defer conn.Close()
	for {
		err := oneRequest(conn)
		if err != nil {
			log.Println(err)
			return
		}
	}
}


func main() {
	listener, err := net.Listen("tcp", ":1234") //we set the tcp server

	if err != nil{
		// there is some error
		log.Println("listening error:", err)
		return
	}

	defer listener.Close() //not clear, check the doc again
	fmt.Println("listening on :1234")

	for {
		//infinite loop
		conn, err := listener.Accept()
		if err != nil{
			log.Println("accept error:", err)
			continue //why not break?
		}
		go handleConn(conn)
		//we remove conn.Close() as we have deferred it already in handleConn()
		//if we kept it: conn.Close() would have closed the connection automatically
	}
}




// func doSomething(conn net.Conn){
// 	buf := make([]byte, 64)
// 	n, err := conn.Read(buf) //not going to work for "long messages". need io.ReadFull
// 	if err != nil{
// 		log.Println("read error:", err)
// 	}

// 	fmt.Printf("client says: %s\n", buf[:n]) //print the first n bytes ffrom the buf

// 	_, err = conn.Write([]byte("world")) //write only takes a slice of binary as param
// 	if err != nil {
// 		log.Println("write error:", err)
// 	}
// }