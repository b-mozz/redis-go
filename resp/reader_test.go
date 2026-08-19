package resp

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestReadCommand(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want []string
	}{
		{
			"array of bulk strings",
			"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n",
			[]string{"SET", "k", "v"},
		},
		{
			"single element",
			"*1\r\n$4\r\nPING\r\n",
			[]string{"PING"},
		},
		{
			"empty bulk argument",
			"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$0\r\n\r\n",
			[]string{"SET", "k", ""},
		},
		{
			// The reason payloads are read by length and never scanned: this
			// value contains the delimiter (doc/resp-format.md:113-123).
			"payload containing CRLF",
			"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$6\r\na\x00b\r\nc\r\n",
			[]string{"SET", "k", "a\x00b\r\nc"},
		},
		{
			"redis-cli sends uppercase",
			"*2\r\n$3\r\nGET\r\n$4\r\nname\r\n",
			[]string{"GET", "name"},
		},

		// Inline commands: no array framing, dispatched on the first byte.
		{"inline", "PING\r\n", []string{"PING"}},
		{"inline with args", "SET k v\r\n", []string{"SET", "k", "v"}},
		{"inline quoted", "SET k \"a b\"\r\n", []string{"SET", "k", "a b"}},

		// Neither carries a command; the caller loops rather than erroring
		// (doc/resp-format.md:313-318).
		{"empty array", "*0\r\n", nil},
		{"stray CRLF", "\r\n", nil},
		{"negative array is ignored", "*-1\r\n", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewReader(strings.NewReader(tt.wire)).ReadCommand()
			if err != nil {
				t.Fatalf("ReadCommand(%q) returned error: %v", tt.wire, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ReadCommand(%q)\n got = %q\nwant = %q", tt.wire, got, tt.want)
			}
		})
	}
}

// A client may put several commands in one write. Each ReadCommand returns
// exactly one, in order, and Buffered() reports what is left.
func TestReadCommandPipelined(t *testing.T) {
	const wire = "*1\r\n$4\r\nPING\r\n" +
		"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n" +
		"*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"

	want := [][]string{
		{"PING"},
		{"SET", "k", "v"},
		{"GET", "k"},
	}

	r := NewReader(strings.NewReader(wire))
	for i, w := range want {
		got, err := r.ReadCommand()
		if err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("command %d\n got = %q\nwant = %q", i, got, w)
		}
	}

	// Everything consumed; the next read would block on a real connection.
	if n := r.Buffered(); n != 0 {
		t.Errorf("Buffered() = %d after draining, want 0", n)
	}
	if _, err := r.ReadCommand(); err != io.EOF {
		t.Errorf("after the last command, err = %v, want io.EOF", err)
	}
}

// dripReader delivers at most n bytes per Read, the way a real conn does when a
// command straddles packets.
type dripReader struct {
	r io.Reader
	n int
}

func (d dripReader) Read(p []byte) (int, error) {
	if len(p) > d.n {
		p = p[:d.n]
	}
	return d.r.Read(p)
}

// The regression test for using io.ReadFull rather than bufio's Read. Read may
// return fewer bytes than asked for WITH A NIL ERROR, which silently truncates
// the value and desyncs the stream. On a normal reader the bug is invisible;
// only a drip-feed exposes it.
func TestReadCommandSplitAcrossReads(t *testing.T) {
	const wire = "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$11\r\nhello world\r\n"
	want := []string{"SET", "k", "hello world"}

	for _, chunk := range []int{1, 2, 3, 5, 7, 13} {
		t.Run("chunk"+string(rune('0'+chunk%10)), func(t *testing.T) {
			r := NewReader(dripReader{strings.NewReader(wire), chunk})
			got, err := r.ReadCommand()
			if err != nil {
				t.Fatalf("chunk=%d: %v", chunk, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("chunk=%d\n got = %q\nwant = %q", chunk, got, want)
			}
		})
	}
}

// A command cut short mid-payload is an I/O error, not a protocol error — the
// bytes may simply not have arrived yet, or the client hung up.
func TestReadCommandTruncated(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{"cut after array header", "*3\r\n"},
		{"cut after bulk header", "*3\r\n$3\r\n"},
		{"cut mid payload", "*3\r\n$3\r\nSE"},
		{"cut before terminator", "*1\r\n$3\r\nSET"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewReader(strings.NewReader(tt.wire)).ReadCommand()
			if err == nil {
				t.Fatalf("ReadCommand(%q) succeeded, want an error", tt.wire)
			}
			if _, ok := err.(*ProtocolError); ok {
				t.Errorf("ReadCommand(%q) = %v, want an I/O error not a ProtocolError", tt.wire, err)
			}
		})
	}
}

// A clean disconnect must surface as io.EOF so handleConn can tell it from a
// fault and close quietly.
func TestReadCommandEOF(t *testing.T) {
	if _, err := NewReader(strings.NewReader("")).ReadCommand(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}
