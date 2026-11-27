package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// defaultMaxLineSize is the hard cap on one stdio message line (64 MiB). A
// larger line is refused deterministically and the stream is resynchronized at
// the next newline, so one oversized message cannot wedge the session or
// exhaust the process memory. This mirrors the eka-mcp transport framing.
const defaultMaxLineSize = 64 << 20

// writerBufferSize is the response writer buffer; every reply is flushed
// immediately, so the flush-per-reply framing contract stays observable.
const writerBufferSize = 64 << 10

// errLineTooLong marks a message line that exceeded the line cap.
var errLineTooLong = errors.New("protocol: message exceeds the 64 MiB line limit")

// lineTooLongResponse is the deterministic refusal for a line exceeding the
// cap: a JSON-RPC invalid-request error with a fixed message (no internals).
func lineTooLongResponse() []byte {
	return marshalResponse(rpcResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage("null"),
		Error:   &rpcError{Code: codeInvalidRequest, Message: "message exceeds the 64 MiB line limit"},
	})
}

// readLine reads one newline-terminated line, accumulating fragments up to max
// (the cap counts the message content, not the framing newline). A line that
// exceeds the cap is drained (the stream is resynchronized at the next
// newline) and reported as errLineTooLong. A final partial line (EOF without a
// trailing newline) is delivered with the EOF deferred to the next call, so
// Serve processes it before terminating.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if err == nil {
			if len(buf)+len(frag)-1 > max {
				return nil, errLineTooLong
			}
			return append(buf, frag...), nil
		}
		if err == bufio.ErrBufferFull {
			if len(buf)+len(frag) > max {
				drainLine(br)
				return nil, errLineTooLong
			}
			buf = append(buf, frag...)
			continue
		}
		if len(buf)+len(frag) > max {
			return nil, errLineTooLong
		}
		line := append(buf, frag...)
		if err == io.EOF && len(bytes.TrimSpace(line)) > 0 {
			return line, nil
		}
		return line, err
	}
}

// drainLine consumes the remainder of an oversized line so the stream is
// resynchronized at the next newline. It always terminates.
func drainLine(br *bufio.Reader) {
	for {
		_, err := br.ReadSlice('\n')
		if err == nil || err != bufio.ErrBufferFull {
			return
		}
	}
}
