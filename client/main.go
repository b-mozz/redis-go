package main

import (
    "encoding/binary"
    "fmt"
    "io"
    "log"
    "net"

    "redis_go/proto"
)

const maxMsg = 32 << 20 // matching server maxMsg

func buildReq(cmd []string) []byte {
    // Calculate total size: nstr(4) + for each string: len(4) + data
    size := 4
    for _, s := range cmd {
        size += 4 + len(s)
    }

    // Build: [outer len][nstr][len1][str1][len2][str2]...
    buf := make([]byte, 4+size)
    binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
    binary.LittleEndian.PutUint32(buf[4:8], uint32(len(cmd)))

    pos := 8
    for _, s := range cmd {
        binary.LittleEndian.PutUint32(buf[pos:pos+4], uint32(len(s)))
        pos += 4
        copy(buf[pos:], s)
        pos += len(s)
    }

    return buf
}

func readResponse(conn net.Conn) error {

    // Read outer length
    // every message starts with a 4 byte header(how long the message is), thus we have max len 4 for header
    header := make([]byte, 4)
    _, err := io.ReadFull(conn, header)
    if err != nil {
        return fmt.Errorf("read header: %w", err)
    }

    respLen := binary.LittleEndian.Uint32(header)
    if respLen > maxMsg {
        return fmt.Errorf("response too long: %d", respLen)
    }

    // proto already has a function named readValue, server uses it
    // instead of rewriting this function, we will use the same one to keep consistency
    
    // first we read the response byte to our body
    body := make([]byte, respLen)
    _, err = io.ReadFull(conn, body)
    if err != nil {
        return fmt.Errorf("read body: %w", err)
    }

    // proto already has ReadValue, server uses it too — reuse for consistency.
    // ReadValue returns the leftover tail because it's used recursively (e.g. for arrays).
    // Here the body is exactly one value, so we discard the tail.
    val, _, err := proto.ReadValue(body)
    if err != nil {
        return fmt.Errorf("decode: %w", err)
    }

    fmt.Println(proto.PrintValue(val))
    return nil
}

func query(conn net.Conn, cmd ...string) error {
    _, err := conn.Write(buildReq(cmd))
    if err != nil {
        return err
    }
    return readResponse(conn)
}

func main() {
    conn, err := net.Dial("tcp", "127.0.0.1:1234")
    if err != nil {
        log.Fatal("connect:", err)
    }
    defer conn.Close()

    query(conn, "set", "name", "bimukti") // -> (nil)
    query(conn, "get", "name")             // -> "bimukti"
    query(conn, "set", "lang", "go")       // -> (nil)
    query(conn, "get", "lang")             // -> "go"
    query(conn, "keys")                    // -> array ["name","lang"]
    query(conn, "del", "name")             // -> (int) 1
    query(conn, "get", "name")             // -> (nil)
    query(conn, "del", "nope")             // -> (int) 0
    query(conn, "get")                     // -> (error) wrong arg count
    query(conn, "blah")                    // -> (error) unknown command
}