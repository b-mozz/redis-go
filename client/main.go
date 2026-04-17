package main

import (
    "encoding/binary"
    "fmt"
    "io"
    "log"
    "net"
)

const maxMsg = 4096

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
    header := make([]byte, 4)
    _, err := io.ReadFull(conn, header)
    if err != nil {
        return fmt.Errorf("read header: %w", err)
    }

    respLen := binary.LittleEndian.Uint32(header)
    if respLen > maxMsg {
        return fmt.Errorf("response too long: %d", respLen)
    }

    // Read response body: [status(4)][data...]
    body := make([]byte, respLen)
    _, err = io.ReadFull(conn, body)
    if err != nil {
        return fmt.Errorf("read body: %w", err)
    }

    status := binary.LittleEndian.Uint32(body[0:4])
    data := body[4:]

    switch status {
    case 0: // OK
        if len(data) > 0 {
            fmt.Printf("[ok] %s\n", data)
        } else {
            fmt.Println("[ok]")
        }
    case 1: // NX
        fmt.Println("[not found]")
    default:
        if len(data) > 0 {
            fmt.Printf("[err] %s\n", data)
        } else {
            fmt.Println("[err]")
        }
    }
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

    // Test: set, get, del, get
    query(conn, "set", "name", "bimukti")
    query(conn, "get", "name")
    query(conn, "set", "lang", "go")
    query(conn, "get", "lang")
    query(conn, "del", "name")
    query(conn, "get", "name")
    query(conn, "blah")
}