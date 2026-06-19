package proto

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"
)

// error codes carried inside a TagErr value
const (
	ErrUnknown int32 = 1 // unknown command
	ErrArg     int32 = 2 // bad number of arguments
	ErrTooBig  int32 = 3 // response exceeded MaxMsg
)

const MaxMsg = 32 << 20 // 32 MiB safety cap on a single response body

const (
    TagNil byte = 0 // nil / null result
    TagErr byte = 1 // error: code (i32) + message (string)
    TagStr byte = 2 // string: u32 length + bytes
    TagInt byte = 3 // int64, little-endian
    TagDbl byte = 4 // float64, little-endian
    TagArr byte = 5 // array: u32 element count + elements
)

// functions to handle endianNess
func appendU8(buf *[]byte, v byte) { // tag or type
	*buf = append(*buf, v)
}

func appendU32(buf *[]byte, v uint32) { // length 
    var tmp [4]byte
    binary.LittleEndian.PutUint32(tmp[:], v)
    *buf = append(*buf, tmp[:]...)
}

func appendI64(buf *[]byte, v int64) { // integers
    var tmp [8]byte
    binary.LittleEndian.PutUint64(tmp[:], uint64(v))
    *buf = append(*buf, tmp[:]...)
}

func appendF64(buf *[]byte, v float64) { // floats
    var tmp [8]byte
    binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(v))
    *buf = append(*buf, tmp[:]...)
}


// --- typed writers ---
func OutNil(out *[]byte) {
    appendU8(out, TagNil)
}

func OutStr(out *[]byte, s string) {
    appendU8(out, TagStr)
    appendU32(out, uint32(len(s)))
    *out = append(*out, s...) // special case: append allows string... into []byte
}

func OutInt(out *[]byte, v int64) {
    appendU8(out, TagInt)
    appendI64(out, v)
}

func OutDbl(out *[]byte, v float64) {
    appendU8(out, TagDbl)
    appendF64(out, v)
}

func OutErr(out *[]byte, code int32, msg string) {
    appendU8(out, TagErr)
    appendU32(out, uint32(code))
    appendU32(out, uint32(len(msg)))
    *out = append(*out, msg...) // special case: append allows string... into []byte
}

// OutArr writes only the array *header*. The caller is responsible
// for then writing exactly `n` child values.
func OutArr(out *[]byte, n uint32) {
    appendU8(out, TagArr)
    appendU32(out, n)
}

// --- message framing ---
// Each response is prefixed with a u32 body length, but we don't know the
// body size until after we've written it. ResponseBegin reserves 4 bytes
// for the prefix and returns its offset; ResponseEnd patches it in place.

func ResponseBegin(out *[]byte) int {
	header := len(*out)
	appendU32(out, 0) // placeholder; patched by ResponseEnd
	return header
}

func ResponseEnd(out *[]byte, header int) {
	msgSize := len(*out) - header - 4
	if msgSize > MaxMsg {
		// truncate body and write an error in its place
		*out = (*out)[:header+4]
		OutErr(out, ErrTooBig, "response is too big")
		msgSize = len(*out) - header - 4
	}
	binary.LittleEndian.PutUint32((*out)[header:header+4], uint32(msgSize))
}

// --- reader ---
// Mirrors the writers above: read the tag, dispatch on it, decode payload.
// Returns the decoded Value and the remaining unread bytes.

type Value struct {
	Tag  byte
	Int  int64
	Dbl  float64
	Str  string
	Arr  []Value
	Code int32 // populated only when Tag == TagErr
}

func ReadValue(buf []byte) (Value, []byte, error) {
	if len(buf) < 1 {
		return Value{}, buf, io.ErrUnexpectedEOF
	}
	tag := buf[0]
	buf = buf[1:]
	switch tag {
	case TagNil:
		return Value{Tag: tag}, buf, nil
	case TagInt:
		if len(buf) < 8 {
			return Value{}, buf, io.ErrUnexpectedEOF
		}
		v := int64(binary.LittleEndian.Uint64(buf[:8]))
		return Value{Tag: tag, Int: v}, buf[8:], nil
	case TagDbl:
		if len(buf) < 8 {
			return Value{}, buf, io.ErrUnexpectedEOF
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(buf[:8]))
		return Value{Tag: tag, Dbl: v}, buf[8:], nil
	case TagStr:
		if len(buf) < 4 {
			return Value{}, buf, io.ErrUnexpectedEOF
		}
		n := binary.LittleEndian.Uint32(buf[:4])
		buf = buf[4:]
		if uint32(len(buf)) < n {
			return Value{}, buf, io.ErrUnexpectedEOF
		}
		return Value{Tag: tag, Str: string(buf[:n])}, buf[n:], nil
	case TagErr:
		if len(buf) < 8 {
			return Value{}, buf, io.ErrUnexpectedEOF
		}
		code := int32(binary.LittleEndian.Uint32(buf[:4]))
		n := binary.LittleEndian.Uint32(buf[4:8])
		buf = buf[8:]
		if uint32(len(buf)) < n {
			return Value{}, buf, io.ErrUnexpectedEOF
		}
		return Value{Tag: tag, Code: code, Str: string(buf[:n])}, buf[n:], nil
	case TagArr:
		if len(buf) < 4 {
			return Value{}, buf, io.ErrUnexpectedEOF
		}
		n := binary.LittleEndian.Uint32(buf[:4])
		buf = buf[4:]
		arr := make([]Value, 0, n)
		for i := uint32(0); i < n; i++ {
			var v Value
			var err error
			v, buf, err = ReadValue(buf)
			if err != nil {
				return Value{}, buf, err
			}
			arr = append(arr, v)
		}
		return Value{Tag: tag, Arr: arr}, buf, nil
	default:
		return Value{}, buf, fmt.Errorf("unknown tag %d", tag)
	}
}

// --- pretty printer ---
// Renders a decoded Value as a human-readable string for debugging / CLI output.
func PrintValue(v Value) string {
	switch v.Tag {
	case TagNil:
		return "(nil)"
	case TagInt:
		return fmt.Sprintf("(int) %d", v.Int)
	case TagDbl:
		return fmt.Sprintf("(dbl) %g", v.Dbl)
	case TagStr:
		return fmt.Sprintf("%q", v.Str)
	case TagErr:
		return fmt.Sprintf("(err %d) %s", v.Code, v.Str)
	case TagArr:
		parts := make([]string, len(v.Arr))
		for i, c := range v.Arr {
			parts[i] = PrintValue(c)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return "(?)"
	}
}


