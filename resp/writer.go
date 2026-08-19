package resp

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

type Writer struct {
	w *bufio.Writer
}

func NewWriter(writer io.Writer) *Writer {
	return &Writer{w: bufio.NewWriter(writer)}
}

func (writer *Writer) OK() {
	writer.w.WriteByte('+')
	writer.w.WriteString("OK")
	writer.w.WriteString("\r\n")
}

func (writer *Writer) Nil() {
	writer.w.WriteString("$-1\r\n")
}

func (writer *Writer) Bulk(s string) {
	writer.w.WriteByte('$')
	writer.w.WriteString(strconv.Itoa(len(s)))
	writer.w.WriteString("\r\n")
	writer.w.WriteString(s)
	writer.w.WriteString("\r\n")
}

func (writer *Writer) Int(n int64) {
	writer.w.WriteByte(':')
	writer.w.WriteString(strconv.FormatInt(n, 10))
	writer.w.WriteString("\r\n")
}

func (writer *Writer) Arr(n int) {
	writer.w.WriteByte('*')
	writer.w.WriteString(strconv.Itoa(n))
	writer.w.WriteString("\r\n")
}

// Simple writes a simple string: "PONG" -> +PONG\r\n
func (writer *Writer) Simple(s string) {
	writer.w.WriteByte('+')
	writer.w.WriteString(sanitize(s))
	writer.w.WriteString("\r\n")
}

// Err writes an error reply, without the leading '-':
//
//	Err("ERR unknown command 'foo'") -> -ERR unknown command 'foo'\r\n
//
// The first word is the error prefix (ERR, WRONGTYPE, READONLY) that client
// libraries pattern-match on, so copy real Redis's wording verbatim.
func (writer *Writer) Err(msg string) {
	writer.w.WriteByte('-')
	writer.w.WriteString(sanitize(msg))
	writer.w.WriteString("\r\n")
}

// sanitize replaces \r and \n with spaces.
//
// Simple strings and errors carry no length prefix — they are terminated BY
// \r\n. So a control byte inside the payload ends the reply early and the
// remainder is parsed as the next one:
//
//	Err("ERR bad cmd 'x\r\n:99'")  ->  -ERR bad cmd 'x
//	                                   :99'
//
// The client reads an error, then a bogus integer, and every reply after that
// is off by one. Message text is handler-built and interpolates client input,
// so this is not optional. Bulk needs no equivalent: it is length-prefixed and
// binary-safe.
func sanitize(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s // common case: no copy, no allocation
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func (writer *Writer) Flush() error {
	return writer.w.Flush()
}
