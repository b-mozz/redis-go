// proto_test.go
// Round-trip tests for the wire protocol: for each typed writer (Out*), writing a
// value and then reading it back with ReadValue should return the same value.
//
// This is the classic "encode then decode == identity" check. If a writer and the
// reader ever disagree about the byte layout of a type, one of these fails.

package proto

import (
	"testing"
)

// TestRoundTrip_Nil: a nil writes and reads back as a nil.
func TestRoundTrip_Nil(t *testing.T) {
	var buf []byte
	OutNil(&buf)

	val, rest, err := ReadValue(buf)
	if err != nil {
		t.Fatalf("ReadValue returned error: %v", err)
	}
	if val.Tag != TagNil {
		t.Errorf("Tag = %d, want TagNil (%d)", val.Tag, TagNil)
	}
	if len(rest) != 0 {
		t.Errorf("expected no leftover bytes, got %d", len(rest))
	}
}

// TestRoundTrip_Str: a string survives the trip with its exact contents.
func TestRoundTrip_Str(t *testing.T) {
	var buf []byte
	want := "hello world"
	OutStr(&buf, want)

	val, _, err := ReadValue(buf)
	if err != nil {
		t.Fatalf("ReadValue returned error: %v", err)
	}
	if val.Tag != TagStr {
		t.Fatalf("Tag = %d, want TagStr (%d)", val.Tag, TagStr)
	}
	if val.Str != want {
		t.Errorf("Str = %q, want %q", val.Str, want)
	}
}

// TestRoundTrip_Int: an int64 (including a negative one) round-trips exactly.
func TestRoundTrip_Int(t *testing.T) {
	var buf []byte
	var want int64 = -42
	OutInt(&buf, want)

	val, _, err := ReadValue(buf)
	if err != nil {
		t.Fatalf("ReadValue returned error: %v", err)
	}
	if val.Tag != TagInt {
		t.Fatalf("Tag = %d, want TagInt (%d)", val.Tag, TagInt)
	}
	if val.Int != want {
		t.Errorf("Int = %d, want %d", val.Int, want)
	}
}

// TestRoundTrip_Dbl: a float64 round-trips exactly.
func TestRoundTrip_Dbl(t *testing.T) {
	var buf []byte
	want := 3.14159
	OutDbl(&buf, want)

	val, _, err := ReadValue(buf)
	if err != nil {
		t.Fatalf("ReadValue returned error: %v", err)
	}
	if val.Tag != TagDbl {
		t.Fatalf("Tag = %d, want TagDbl (%d)", val.Tag, TagDbl)
	}
	if val.Dbl != want {
		t.Errorf("Dbl = %g, want %g", val.Dbl, want)
	}
}

// TestRoundTrip_Err: an error carries both its code and its message across.
func TestRoundTrip_Err(t *testing.T) {
	var buf []byte
	wantCode := ErrArg
	wantMsg := "bad number of arguments"
	OutErr(&buf, wantCode, wantMsg)

	val, _, err := ReadValue(buf)
	if err != nil {
		t.Fatalf("ReadValue returned error: %v", err)
	}
	if val.Tag != TagErr {
		t.Fatalf("Tag = %d, want TagErr (%d)", val.Tag, TagErr)
	}
	if val.Code != wantCode {
		t.Errorf("Code = %d, want %d", val.Code, wantCode)
	}
	if val.Str != wantMsg {
		t.Errorf("Str = %q, want %q", val.Str, wantMsg)
	}
}

// TestRoundTrip_Arr: an array header plus its child values decode back into the
// same sequence of elements. this is the `keys` response shape.
func TestRoundTrip_Arr(t *testing.T) {
	var buf []byte
	want := []string{"a", "bb", "ccc"}

	OutArr(&buf, uint32(len(want))) // header first: "an array of N is coming"
	for _, s := range want {
		OutStr(&buf, s)
	}

	val, _, err := ReadValue(buf)
	if err != nil {
		t.Fatalf("ReadValue returned error: %v", err)
	}
	if val.Tag != TagArr {
		t.Fatalf("Tag = %d, want TagArr (%d)", val.Tag, TagArr)
	}
	if len(val.Arr) != len(want) {
		t.Fatalf("array length = %d, want %d", len(val.Arr), len(want))
	}

	for i, child := range val.Arr {
		if child.Tag != TagStr {
			t.Errorf("element %d: Tag = %d, want TagStr", i, child.Tag)
		}
		if child.Str != want[i] {
			t.Errorf("element %d: Str = %q, want %q", i, child.Str, want[i])
		}
	}
}
