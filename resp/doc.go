// Package resp implements the RESP wire protocol: a request parser (reader.go),
// a reply writer (writer.go), and the protocol error type (errors.go).
//
// handleConn builds one Reader per connection and calls ReadCommand in a loop.
// Each call returns exactly one command as argv, or blocks until one arrives.
//
//	rd := resp.NewReader(conn)
//	for {
//	    argv, err := rd.ReadCommand()   // ["SET", "k", "v"]
//	    ...
//	}
//
// A client only ever sends two shapes, so unlike a reply parser this needs no
// value tree and no recursion — requests cannot nest:
//
//	*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n     an array of bulk strings
//	SET k v\r\n                                   an inline command (nc, telnet)
//
// The call tree for the array form. readLine runs once for the array header and
// again inside each element:
//
//	ReadCommand
//	├─ readLine()              -> "*3"          the array header
//	├─ parseCount("3")         -> n = 3
//	└─ n × readBulkString()
//	     ├─ readLine()         -> "$3"          this element's header
//	     └─ io.ReadFull(3+2)   -> "SET\r\n"     the payload, read by LENGTH
//
// Two rules hold the parser together:
//
//  1. Headers are read by delimiter, payloads by length. Never the reverse — a
//     payload may contain \r\n, so scanning for one would truncate it and desync
//     the stream.
//
//  2. Every length is range-checked before it reaches make().
//
// Errors come in two kinds and callers must tell them apart: a *ProtocolError
// means framing is lost, so reply and close the connection; io.EOF means the
// client hung up cleanly.
//
// Design notes live outside the source:
//
//	doc/resp-format.md     the wire format, and why the parser is shaped this way
//	doc/bufio-notes.md     bufio.Reader — the cursor model, ReadSlice, short reads
package resp
