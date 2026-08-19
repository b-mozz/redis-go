package resp

import (
	"reflect"
	"testing"
)

// Cases taken from doc/resp-format.md:222-234 and Redis's sdssplitargs.
func TestSplitInlineArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"simple", "PING", []string{"PING"}},
		{"multi", "SET k v", []string{"SET", "k", "v"}},
		{"collapses blanks", "SET  k   v", []string{"SET", "k", "v"}},
		{"leading/trailing blanks", "  SET k v  ", []string{"SET", "k", "v"}},
		{"tabs count as blanks", "SET\tk\tv", []string{"SET", "k", "v"}},

		{"empty line", "", nil},
		{"all blanks", "   ", nil},

		{"double quotes", `SET greeting "hello world"`, []string{"SET", "greeting", "hello world"}},
		{"unquoted splits", `SET greeting hello world`, []string{"SET", "greeting", "hello", "world"}},
		{"single quotes", `SET greeting 'hello world'`, []string{"SET", "greeting", "hello world"}},
		{"empty quoted arg", `SET k ""`, []string{"SET", "k", ""}},

		{"escape n", `ECHO "a\nb"`, []string{"ECHO", "a\nb"}},
		{"escape r", `ECHO "a\rb"`, []string{"ECHO", "a\rb"}},
		{"escape t", `ECHO "a\tb"`, []string{"ECHO", "a\tb"}},
		{"escaped quote", `ECHO "say \"hi\""`, []string{"ECHO", `say "hi"`}},
		{"hex escape", `ECHO "\x41\x42"`, []string{"ECHO", "AB"}},
		{"hex escape lowercase", `ECHO "\x6a"`, []string{"ECHO", "j"}},
		{"nul byte via hex", `SET k "a\x00b"`, []string{"SET", "k", "a\x00b"}},
		{"single quote escapes only apostrophe", `ECHO 'it\'s'`, []string{"ECHO", "it's"}},
		{"backslash n literal in single quotes", `ECHO 'a\nb'`, []string{"ECHO", `a\nb`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitInlineArgs([]byte(tt.in))
			if err != nil {
				t.Fatalf("splitInlineArgs(%q) returned error: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitInlineArgs(%q)\n got = %q\nwant = %q", tt.in, got, tt.want)
			}
		})
	}
}

// The mustspace rule and unterminated quotes must be protocol errors, never a
// silent misparse (doc/resp-format.md:222-234).
func TestSplitInlineArgsUnbalanced(t *testing.T) {
	bad := []string{
		`SET k "a"b`,  // closing quote not followed by a space
		`SET k 'a'b`,  // same, single quotes
		`SET k "abc`,  // unterminated double quote
		`SET k 'abc`,  // unterminated single quote
		`SET k "abc\`, // trailing backslash inside a quote
	}

	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			got, err := splitInlineArgs([]byte(in))
			if err == nil {
				t.Fatalf("splitInlineArgs(%q) = %q, want a ProtocolError", in, got)
			}
			if _, ok := err.(*ProtocolError); !ok {
				t.Errorf("splitInlineArgs(%q) error is %T, want *ProtocolError", in, err)
			}
		})
	}
}
