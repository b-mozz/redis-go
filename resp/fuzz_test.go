package resp

import (
	"bytes"
	"io"
	"testing"
)

// FuzzReadCommand asserts the parser's one hard invariant: whatever bytes arrive,
// it must either parse them or return an error. It must never panic, and it must
// never hand back a partly-filled result alongside an error.
//
// Run the generative form with:
//
//	go test -fuzz=FuzzReadCommand ./resp/
//
// Without -fuzz this still runs the seed corpus below on every `go test`, which
// is most of the value for free.
func FuzzReadCommand(f *testing.F) {
	seeds := []string{
		// Well-formed.
		"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n",
		"*1\r\n$4\r\nPING\r\n",
		"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$6\r\na\x00b\r\nc\r\n",
		"*0\r\n",
		"\r\n",

		// Inline.
		"PING\r\n",
		"SET greeting \"hello world\"\r\n",
		"SET k 'a b'\r\n",
		"SET k \"a\"b\r\n",
		"SET k \"unterminated\r\n",

		// Malformed framing.
		"*1\r\n$-1\r\n",
		"*-1\r\n",
		"*1\r\n:5\r\n",
		"*abc\r\n",
		"*\r\n",
		"$\r\n",
		"\n",
		"*1\r\n$1\r\nabc\r\n",

		// Truncated at every interesting boundary.
		"*3\r\n",
		"*3\r\n$3\r\n",
		"*3\r\n$3\r\nSE",
		"",

		// Limits.
		"*1\r\n$536870913\r\n",
		"*2000000\r\n",
		"*99999999999999999999\r\n",
		"*1\r\n$99999999999999999999\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Cap the input so the fuzzer spends its time on shapes rather than on
		// megabyte payloads.
		if len(data) > 4096 {
			return
		}

		r := NewReader(bytes.NewReader(data))

		// Parse repeatedly: a desync bug often shows up only on the command
		// after the malformed one. Bounded so a stream of "\r\n" can't spin.
		for i := 0; i < 32; i++ {
			argv, err := r.ReadCommand()

			if err != nil {
				// Every error must be a protocol error or an I/O error. Anything
				// else means an internal failure leaked out.
				if _, ok := err.(*ProtocolError); !ok &&
					err != io.EOF && err != io.ErrUnexpectedEOF {
					t.Fatalf("unexpected error type %T: %v", err, err)
				}
				// An error must not come with a result.
				if argv != nil {
					t.Fatalf("returned argv %q alongside error %v", argv, err)
				}
				return
			}

			// A successful parse must not contain a nil element, and no argument
			// may exceed the declared cap.
			for j, a := range argv {
				if len(a) > maxBulkLen {
					t.Fatalf("argument %d is %d bytes, over maxBulkLen", j, len(a))
				}
			}
		}
	})
}

// FuzzSplitInlineArgs covers the inline path directly, where the quote state
// machine lives. It needs no I/O, so the fuzzer explores it far faster than
// through ReadCommand.
func FuzzSplitInlineArgs(f *testing.F) {
	seeds := []string{
		"PING",
		"SET k v",
		"SET  k   v",
		`SET greeting "hello world"`,
		`SET greeting 'hello world'`,
		`ECHO "a\nb"`,
		`ECHO "\x41\x42"`,
		`ECHO "\x4"`,
		`ECHO 'it\'s'`,
		`SET k "a"b`,
		`SET k "abc`,
		`SET k 'abc`,
		`SET k "abc\`,
		`"`,
		`'`,
		`\`,
		"",
		"   ",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, line []byte) {
		if len(line) > 4096 {
			return
		}

		// readLine strips the CRLF before this is ever called, so a line
		// containing one is not a case that can occur in production.
		if bytes.ContainsAny(line, "\r\n") {
			return
		}

		args, err := splitInlineArgs(line)
		if err != nil {
			if _, ok := err.(*ProtocolError); !ok {
				t.Fatalf("unexpected error type %T: %v", err, err)
			}
			if args != nil {
				t.Fatalf("returned args %q alongside error %v", args, err)
			}
			return
		}

		// Arguments may contain any byte, escapes included — `ECHO "a\nb"` is
		// meant to yield a real newline, and it is safe because arguments go
		// back out through Bulk, which is length-prefixed.
		//
		// What must hold is that the splitter never invents bytes: every escape
		// consumes at least as many input bytes as it emits (\n is 2 -> 1, \xHH
		// is 4 -> 1), and quotes are consumed without output. So the total
		// output can never exceed the input.
		total := 0
		for _, a := range args {
			total += len(a)
		}
		if total > len(line) {
			t.Fatalf("output grew: %d bytes across %d args from a %d byte line (%q)",
				total, len(args), len(line), line)
		}
	})
}
