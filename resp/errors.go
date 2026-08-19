package resp

import "fmt"

// ProtocolError means the byte stream is no longer parseable. Once framing is
// lost there's no way to find the start of the next command, so handleConn must
// write the message and then close the connection — unlike a command error
// (wrong arity, unknown command), which leaves the connection usable
// (doc/resp-format.md:321-333).
//
// Its own type rather than errors.New so the caller can tell the two apart:
//
//	var pe *resp.ProtocolError
//	if errors.As(err, &pe) { w.Err(pe.Error()); w.Flush(); return }
type ProtocolError struct {
	msg string
}

// Error renders the full RESP error body, prefix included, so a caller can pass
// it straight to the writer: -ERR Protocol error: invalid multibulk length
func (e *ProtocolError) Error() string {
	return "ERR Protocol error: " + e.msg
}

func protoErrorf(format string, args ...any) *ProtocolError {
	return &ProtocolError{msg: fmt.Sprintf(format, args...)}
}
