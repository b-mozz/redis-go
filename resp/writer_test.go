package resp

import (
	"bytes"
	"testing"
)

// Byte-exact reply tests. Every expectation here is copied from the table in
// doc/resp-format.md, "Replies for this server's commands" — if one of these
// fails, redis-cli sees something it did not expect.
func TestWriterReplies(t *testing.T) {
	tests := []struct {
		name string
		call func(*Writer)
		want string
	}{
		{"OK", func(w *Writer) { w.OK() }, "+OK\r\n"},
		{"simple string", func(w *Writer) { w.Simple("PONG") }, "+PONG\r\n"},

		{"nil bulk", func(w *Writer) { w.Nil() }, "$-1\r\n"},
		{"bulk", func(w *Writer) { w.Bulk("Alice") }, "$5\r\nAlice\r\n"},

		// $0 and $-1 are different replies: a key holding "" versus no key at
		// all. Conflating them is a live bug risk in the get handler.
		{"empty bulk is not nil", func(w *Writer) { w.Bulk("") }, "$0\r\n\r\n"},

		// Length is a BYTE count, not a rune count: é is two bytes.
		{"bulk counts bytes not runes", func(w *Writer) { w.Bulk("café") }, "$5\r\ncafé\r\n"},

		// Bulk is length-prefixed, so payloads need no escaping and may contain
		// the delimiter itself.
		{"bulk is binary safe", func(w *Writer) { w.Bulk("a\x00b\r\nc") }, "$6\r\na\x00b\r\nc\r\n"},

		{"int", func(w *Writer) { w.Int(1) }, ":1\r\n"},
		{"int zero", func(w *Writer) { w.Int(0) }, ":0\r\n"},

		// ttl relies on signed integers for its -1 (no expiry) and -2 (no key)
		// sentinels.
		{"int negative", func(w *Writer) { w.Int(-2) }, ":-2\r\n"},

		{"array header", func(w *Writer) { w.Arr(2) }, "*2\r\n"},

		// keys with no matches is an EMPTY array, never a null one.
		{"empty array", func(w *Writer) { w.Arr(0) }, "*0\r\n"},

		{"error", func(w *Writer) { w.Err("ERR unknown command 'foo'") }, "-ERR unknown command 'foo'\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			tt.call(w)

			// Nothing reaches the underlying writer until Flush — the methods
			// only append to bufio's buffer.
			if buf.Len() != 0 {
				t.Errorf("wrote %q before Flush; replies must stay buffered", buf.String())
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("\n got = %q\nwant = %q", got, tt.want)
			}
		})
	}
}

// A control byte inside a simple string or error would terminate the reply early
// and the remainder would be parsed as the next one. Both must be sanitized.
func TestWriterSanitizesControlBytes(t *testing.T) {
	tests := []struct {
		name string
		call func(*Writer)
		want string
	}{
		{"err with CRLF", func(w *Writer) { w.Err("ERR bad 'x\r\n:99'") }, "-ERR bad 'x  :99'\r\n"},
		{"err with LF", func(w *Writer) { w.Err("ERR bad\nthing") }, "-ERR bad thing\r\n"},
		{"simple with CRLF", func(w *Writer) { w.Simple("PO\r\nNG") }, "+PO  NG\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			tt.call(w)
			w.Flush()

			got := buf.String()
			if got != tt.want {
				t.Errorf("\n got = %q\nwant = %q", got, tt.want)
			}
			// The invariant, independent of the exact replacement: exactly one
			// CRLF, at the very end.
			if bytes.Count([]byte(got), []byte("\r\n")) != 1 {
				t.Errorf("%q contains %d CRLFs, want exactly 1 (the terminator)",
					got, bytes.Count([]byte(got), []byte("\r\n")))
			}
		})
	}
}

// Bulk must NOT be sanitized — it is length-prefixed, so control bytes are data.
func TestWriterBulkIsNotSanitized(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Bulk("a\r\nb")
	w.Flush()

	const want = "$4\r\na\r\nb\r\n"
	if got := buf.String(); got != want {
		t.Errorf("\n got = %q\nwant = %q", got, want)
	}
}

// A full KEYS reply: header then children, in one flush.
func TestWriterArraySequence(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	keys := []string{"name", "foo"}
	w.Arr(len(keys))
	for _, k := range keys {
		w.Bulk(k)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	const want = "*2\r\n$4\r\nname\r\n$3\r\nfoo\r\n"
	if got := buf.String(); got != want {
		t.Errorf("\n got = %q\nwant = %q", got, want)
	}
}

// Replies accumulate in one buffer and leave in a single write, which is what
// makes pipelining fast.
func TestWriterBuffersUntilFlush(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	w.Simple("PONG")
	w.OK()
	w.Int(1)

	if buf.Len() != 0 {
		t.Fatalf("wrote %q before Flush", buf.String())
	}
	w.Flush()

	const want = "+PONG\r\n+OK\r\n:1\r\n"
	if got := buf.String(); got != want {
		t.Errorf("\n got = %q\nwant = %q", got, want)
	}
}
