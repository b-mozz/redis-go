package resp

import (
	"strings"
	"testing"
)

// Malformed input must produce a *ProtocolError — never a panic, never a silent
// misparse, and never a large allocation. handleConn keys off the type to decide
// reply-and-close versus reply-and-continue (doc/resp-format.md:321-333), so an
// error of the wrong type would leave a desynced connection open.
func TestReadCommandProtocolErrors(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		// Reply-only forms. A client must never send these; real Redis requires
		// every array element to begin with '$' (doc/resp-format.md:306-310).
		{"null bulk as argument", "*1\r\n$-1\r\n"},
		{"negative bulk length", "*1\r\n$-2\r\n"},

		// Wrong type where a bulk string is required. This one check is what
		// keeps '+', '-' and ':' out of the request grammar.
		{"integer as argument", "*1\r\n:5\r\n"},
		{"simple string as argument", "*1\r\n+OK\r\n"},
		{"error as argument", "*1\r\n-ERR nope\r\n"},
		{"nested array", "*1\r\n*1\r\n$1\r\na\r\n"},

		// Over the limits. These must be rejected from the header alone, before
		// anything is allocated (doc/resp-format.md:281-293).
		{"bulk over 512MB", "*1\r\n$536870913\r\n"},
		{"multibulk over 1M elements", "*2000000\r\n"},

		// Values that overflow a hand-rolled len*10+digit parser
		// (doc/resp-format.md:270-279).
		{"bulk length overflows int64", "*1\r\n$99999999999999999999\r\n"},
		{"multibulk overflows int64", "*99999999999999999999\r\n"},

		// Non-numeric counts.
		{"non-numeric multibulk", "*abc\r\n"},
		{"non-numeric bulk", "*1\r\n$abc\r\n"},
		{"empty multibulk count", "*\r\n"},

		// Framing damage.
		{"bare LF", "\n"},
		{"bulk terminator missing", "*1\r\n$1\r\nab\r\n"},
		{"bulk longer than declared", "*1\r\n$1\r\nabc\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewReader(strings.NewReader(tt.wire)).ReadCommand()
			if err == nil {
				t.Fatalf("ReadCommand(%q) = %q, want a *ProtocolError", tt.wire, got)
			}
			if _, ok := err.(*ProtocolError); !ok {
				t.Errorf("ReadCommand(%q) error is %T (%v), want *ProtocolError", tt.wire, err, err)
			}
		})
	}
}

// An oversized header line must be rejected from the buffer boundary rather than
// growing memory until the process dies. Reproduces `yes | tr -d '\n' | nc`
// (doc/resp-format.md:296-304).
func TestReadCommandLineTooLong(t *testing.T) {
	wire := "*" + strings.Repeat("1", maxLineLen+10) + "\r\n"

	_, err := NewReader(strings.NewReader(wire)).ReadCommand()
	if err == nil {
		t.Fatal("oversized line accepted, want a *ProtocolError")
	}
	if _, ok := err.(*ProtocolError); !ok {
		t.Errorf("error is %T (%v), want *ProtocolError", err, err)
	}
}

// A protocol error must leave the reply body renderable, prefix included, so the
// caller can hand it straight to Writer.Err.
func TestProtocolErrorMessage(t *testing.T) {
	_, err := NewReader(strings.NewReader("*1\r\n:5\r\n")).ReadCommand()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.HasPrefix(err.Error(), "ERR Protocol error: ") {
		t.Errorf("Error() = %q, want it to start with %q", err.Error(), "ERR Protocol error: ")
	}
}
