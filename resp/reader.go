package resp

import (
	"bufio"
	"io"
	"strconv"
)

// Limits on anything a client can make us allocate, enforced before make().
const (
	maxBulkLen   = 512 << 20 // 512MB, matches Redis's proto-max-bulk-len (in spec)
	maxMultiBulk = 1 << 20   // 1024*1024 elements (not in spec; Redis's value)
	maxLineLen   = 64 << 10  // cap on any single header or inline line
)

// Reader parses RESP requests off one connection. Not safe for concurrent use —
// parse state is the bufio cursor, so construct one per connection.
type Reader struct {
	r *bufio.Reader // reader
}

// NewReader returns a Reader over rd. The buffer size doubles as the maximum
// line length, since ReadSlice reports ErrBufferFull past it.
func NewReader(rd io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(rd, maxLineLen)}
}

// Buffered reports how many bytes are already in memory. Zero means the next
// ReadCommand will block on the socket, which is exactly when handleConn must
// flush pending replies — otherwise a pipelining client waits for replies while
// the server waits for bytes (doc/resp-format.md, "Flush before you block").
func (reader *Reader) Buffered() int {
	return reader.r.Buffered()
}

// ReadCommand reads one client request and returns it as argv.
//
// Two shapes are accepted (doc/resp-format.md:21-27):
//
//	*N\r\n followed by N bulk strings  — what every real client sends
//	a bare line                        — inline command, for nc/telnet and PING_INLINE
//
// Return contract, which handleConn depends on:
//
//	argv, nil            — execute it
//	nil,  nil            — nothing to do (stray CRLF, *0); loop and call again
//	nil,  *ProtocolError — framing is lost; reply, then CLOSE the connection
//	nil,  other error    — I/O; io.EOF is a clean client disconnect
func (reader *Reader) ReadCommand() ([]string, error) {
	line, err := reader.readLine()
	if err != nil {
		return nil, err
	}

	// Empty line: the client sent a stray CRLF. Not an error (doc/resp-format.md:313-318).
	if len(line) == 0 {
		return nil, nil
	}

	// No command starts with '*', so one byte decides which grammar this is.
	// line aliases bufio's buffer, so splitInlineArgs must copy before reading further.
	if line[0] != '*' {
		return splitInlineArgs(line)
	}

	n, err := parseCount(line[1:], maxMultiBulk, "invalid multibulk length")
	if err != nil {
		return nil, err
	}

	// *0 and *-1 carry no command. Real Redis discards the line and reads the
	// next one rather than erroring, so we do too.
	if n <= 0 {
		return nil, nil
	}

	// Cap the preallocation: n is attacker-controlled up to maxMultiBulk, and
	// make([]string, 0, n) on a *1048576 header whose elements never arrive would
	// pin 16MB per connection. Let append grow it instead.
	argv := make([]string, 0, int(min(n, 16)))

	for i := int64(0); i < n; i++ {
		s, err := reader.readBulkString()
		if err != nil {
			return nil, err
		}
		argv = append(argv, s)
	}

	return argv, nil
}

// parseCount parses the count from a header line — the "3" of "*3" — and
// range-checks it before any caller allocates.
//
// Callers reject negatives themselves, since the meaning differs: *-1 is
// ignored, $-1 is an error.
func parseCount(b []byte, max int64, errMsg string) (int64, error) {
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n > max {
		return 0, protoErrorf("%s", errMsg)
	}
	return n, nil
}

// readLine returns the next line with its trailing CRLF stripped, without
// interpreting it: "*3\r\n" -> "*3", "\r\n" -> "".
//
// Only for headers and inline commands, never bulk payloads — it stops at the
// first \n, which would truncate binary data.
//
// The result aliases bufio's buffer and dies at the next read, so parse or
// string() it immediately. See doc/bufio-notes.md for why ReadSlice rather than
// ReadBytes/ReadString.
func (reader *Reader) readLine() ([]byte, error) {
	line, err := reader.r.ReadSlice('\n')

	if err != nil {
		if err == bufio.ErrBufferFull {
			return nil, protoErrorf("too big inline request")
		}

		return nil, err
	}

	// shortest legal line is /r/n? why? cz ReadSlice returns /n and also the /r before it
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, protoErrorf("unbalanced line")
	}

	return line[:len(line)-2], nil
}

// readBulkString reads one element — $<len>\r\n<len bytes>\r\n — and returns just
// the payload: "$5\r\nAlice\r\n" -> "Alice".
//
// The '$' check here is what restricts the request grammar to bulk strings, and
// so what spares ReadCommand any case for '+', '-' or ':'.
//
// io.ReadFull rather than Read, which may return short with a nil error and
// silently truncate the value (doc/bufio-notes.md).
func (reader *Reader) readBulkString() (string, error) {
	line, err := reader.readLine()

	if err != nil {
		return "", err
	}

	if len(line) == 0 || line[0] != '$' {
		return "", protoErrorf("expected '$'")
	}

	n, err := parseCount(line[1:], maxBulkLen, "invalid bulk length")

	if err != nil {
		return "", err
	}

	if n < 0 {
		return "", protoErrorf("invalid bulk length")
	}

	buf := make([]byte, n+2)

	_, err = io.ReadFull(reader.r, buf)

	if err != nil {
		return "", err
	}

	if buf[n] != '\r' || buf[n+1] != '\n' {
		return "", protoErrorf("bad string terminator")
	}

	return string(buf[:n]), nil

}

// splitInlineArgs splits an inline command line into argv, following Redis's
// sdssplitargs — quoting included, which the spec omits:
//
//	SET greeting "hello world"  -> ["SET", "greeting", "hello world"]
//	SET greeting hello world    -> ["SET", "greeting", "hello", "world"]
//	SET k "a"b                  -> *ProtocolError (mustspace)
//
// An all-whitespace line yields nil, nil; the caller loops.
//
// Two nested loops around a three-state machine — outer per argument, inner per
// byte, state = unquoted / "double" / 'single'. State table and a worked trace
// are in doc/resp-format.md, "Inline commands".
//
// A plain function, not a method: no I/O, so it table-tests without a socket.
func splitInlineArgs(line []byte) ([]string, error) {
	var args []string
	p := 0

	for {
		// Skip the run of blanks between arguments. Repeated spaces collapse,
		// so "SET  k   v" is three args, not five.
		for p < len(line) && isSpace(line[p]) {
			p++
		}
		if p >= len(line) {
			return args, nil // nil, nil for an empty or all-blank line
		}

		// cur accumulates one argument. It's a fresh []byte rather than a slice
		// of line because escapes mean the result isn't always a contiguous
		// span of the input — and because string(cur) must copy out of bufio's
		// buffer anyway (see readLine).
		var cur []byte
		inq, insq, done := false, false, false

		for !done {
			switch {
			case inq: // inside "double quotes"
				switch {
				// \xHH — needs all four bytes present to be an escape
				case line[p] == '\\' && p+3 < len(line) && line[p+1] == 'x' &&
					isHexDigit(line[p+2]) && isHexDigit(line[p+3]):
					cur = append(cur, hexToInt(line[p+2])<<4|hexToInt(line[p+3]))
					p += 3

				case line[p] == '\\' && p+1 < len(line):
					p++
					var c byte
					switch line[p] {
					case 'n':
						c = '\n'
					case 'r':
						c = '\r'
					case 't':
						c = '\t'
					case 'b':
						c = '\b'
					case 'a':
						c = '\a'
					default:
						c = line[p] // \" and \\ land here
					}
					cur = append(cur, c)

				case line[p] == '"':
					// mustspace: a closing quote may only be followed by a
					// space or end of line, so `"a"b` is an error rather than
					// silently parsing as `ab`.
					if p+1 < len(line) && !isSpace(line[p+1]) {
						return nil, protoErrorf("unbalanced quotes in request")
					}
					inq, done = false, true

				default:
					cur = append(cur, line[p]) // spaces included: that's the point of quoting
				}

			case insq: // inside 'single quotes' — only \' is an escape
				switch {
				case line[p] == '\\' && p+1 < len(line) && line[p+1] == '\'':
					p++
					cur = append(cur, '\'')

				case line[p] == '\'':
					if p+1 < len(line) && !isSpace(line[p+1]) {
						return nil, protoErrorf("unbalanced quotes in request")
					}
					insq, done = false, true

				default:
					cur = append(cur, line[p])
				}

			default: // unquoted
				switch line[p] {
				case ' ', '\n', '\r', '\t', '\v', '\f':
					done = true
				case '"':
					inq = true
				case '\'':
					insq = true
				default:
					cur = append(cur, line[p])
				}
			}

			p++

			// C walks a NUL-terminated string and lets the '\0' case end the
			// token. We have a length instead, so end-of-line is checked here.
			if p >= len(line) {
				if inq || insq {
					return nil, protoErrorf("unbalanced quotes in request")
				}
				done = true
			}
		}

		// string(nil) is "", so `SET k ""` correctly yields an empty argument
		// rather than dropping it.
		args = append(args, string(cur))
	}
}

// isSpace matches C's isspace, which is what sdssplitargs splits on.
func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// isHexDigit and hexToInt decode the \xHH escape, either case.
func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexToInt(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
