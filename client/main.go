// client/main.go
package main

import (
    "encoding/binary"
    "fmt"
    "io"
    "log"
    "net"
)

const maxMsg = 4096

func query(conn net.Conn, text string) error {
    payload := []byte(text)
    msgLen := uint32(len(payload))
    if msgLen > maxMsg {
        return fmt.Errorf("message too long")
    }

    wbuf := make([]byte, 4+len(payload))
    binary.LittleEndian.PutUint32(wbuf[:4], msgLen)
    copy(wbuf[4:], payload)

    _, err := conn.Write(wbuf)
    if err != nil {
        return fmt.Errorf("write: %w", err)
    }

    header := make([]byte, 4)
    _, err = io.ReadFull(conn, header)
    if err != nil {
        return fmt.Errorf("read header: %w", err)
    }

    replyLen := binary.LittleEndian.Uint32(header)
    if replyLen > maxMsg {
        return fmt.Errorf("reply too long: %d", replyLen)
    }

    reply := make([]byte, replyLen)
    _, err = io.ReadFull(conn, reply)
    if err != nil {
        return fmt.Errorf("read reply: %w", err)
    }

    fmt.Printf("server says: %s\n", reply)
    return nil
}

func main() {
    conn, err := net.Dial("tcp", "127.0.0.1:1234")
    if err != nil {
        log.Fatal("connect:", err)
    }
    defer conn.Close()

    // Send multiple requests on the same connection
    if err := query(conn, "hello1"); err != nil {
        log.Println(err)
        return
    }
    if err := query(conn, "hello2"); err != nil {
        log.Println(err)
        return
    }
    if err := query(conn, "hello3"); err != nil {
        log.Println(err)
        return
    }
}