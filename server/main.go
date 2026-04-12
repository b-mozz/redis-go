package main

import(
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
)

const maxLen = 4096

func oneRequest (conn net.Conn) error{

	header := make([]byte, 4)
	_, err := io.ReadFull(conn, header)
	if err != nil{
		if err == io.EOF{
			return fmt.Errorf("EOF")
		}
		return fmt.Errorf("Reading error: %w", err)
		
	}

	msgLen := binary.LittleEndian.Uint32(header) //convert the slice to a 32 bit integer

	if msgLen > maxLen{
		return fmt.Errorf("message to large.")
	}

	//now we load our message
	message := make([]byte, msgLen)
	_, err = io.ReadFull(conn, message)

	if err != nil{
		return fmt.Errorf("reading message error: %w", err)
	}

	//now we need to write our message
	fmt.Printf("Client says this: %s\n", message)

	// write your response to the client
	reply := []byte("world")
	// now we need our write buffer
		//first 4 bytes: length of the message
		//last bytes are the message itself
	
	wbuf := make([]byte, 4 + len(reply))

	binary.LittleEndian.PutUint32(wbuf[:4], uint32(len(reply)))
	copy(wbuf[4:], reply) //cannot assign assignment operator. copy is the only way to do it in go. 
				
	_, err = conn.Write(wbuf)

	if err != nil{
		return fmt.Errorf("write error: %w", err)
	}

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