package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// defaultMaxFrameBytes is the hard cap on one stdio frame line (64 MiB),
// mirroring the eka-mcp transport. A larger line is refused deterministically
// and the stream is resynchronized at the next newline, so one oversized
// frame cannot wedge the session or exhaust memory.
const defaultMaxFrameBytes = 64 << 20

var (
	// ErrLineTooLong is returned when a frame line exceeds the byte cap.
	ErrLineTooLong = errors.New("acp: frame exceeds the line limit")
	// ErrMalformed is returned when a frame line is not valid JSON.
	ErrMalformed = errors.New("acp: malformed json-rpc frame")
)

// FrameReader decodes newline-delimited JSON-RPC messages from an agent's
// stdout with a hard per-line size cap. Blank lines are skipped; an oversized
// line is drained and refused (ErrLineTooLong); a non-JSON line is refused
// (ErrMalformed). The stream is left positioned at the next line in both
// refusal cases, so a caller may continue reading or abort.
type FrameReader struct {
	br  *bufio.Reader
	max int
}

// NewFrameReader wraps r. max <= 0 applies defaultMaxFrameBytes.
func NewFrameReader(r io.Reader, max int) *FrameReader {
	if max <= 0 {
		max = defaultMaxFrameBytes
	}
	return &FrameReader{br: bufio.NewReader(r), max: max}
}

// ReadMessage reads one frame. It returns io.EOF at a clean end of stream.
func (f *FrameReader) ReadMessage() (Message, error) {
	for {
		line, err := readLine(f.br, f.max)
		if errors.Is(err, ErrLineTooLong) {
			return Message{}, err
		}
		if err != nil {
			return Message{}, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m Message
		if uerr := json.Unmarshal(line, &m); uerr != nil {
			return Message{}, fmt.Errorf("%w: %v", ErrMalformed, uerr)
		}
		return m, nil
	}
}

// FrameWriter encodes newline-delimited JSON-RPC messages to an agent's
// stdin, flushing after every write so the agent observes each frame
// immediately. It is safe for concurrent writers (the read loop answers
// server-initiated requests while the caller sends requests).
type FrameWriter struct {
	mu sync.Mutex
	bw *bufio.Writer
}

// NewFrameWriter wraps w.
func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{bw: bufio.NewWriter(w)}
}

// WriteMessage marshals m, stamps the JSON-RPC version, appends a newline and
// flushes.
func (f *FrameWriter) WriteMessage(m Message) error {
	m.JSONRPC = jsonrpcVersion
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("acp: encode frame: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.bw.Write(b); err != nil {
		return err
	}
	if err := f.bw.WriteByte('\n'); err != nil {
		return err
	}
	return f.bw.Flush()
}

// readLine reads one newline-terminated line, accumulating fragments up to max
// (the cap counts message content, not the framing newline). A line past the
// cap is drained so the stream resynchronizes at the next newline and returned
// as ErrLineTooLong. A final partial line (EOF without a trailing newline) is
// delivered with the EOF deferred to the next call.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if err == nil {
			if len(buf)+len(frag)-1 > max {
				return nil, ErrLineTooLong
			}
			return append(buf, frag...), nil
		}
		if err == bufio.ErrBufferFull {
			if len(buf)+len(frag) > max {
				drainLine(br)
				return nil, ErrLineTooLong
			}
			buf = append(buf, frag...)
			continue
		}
		if len(buf)+len(frag) > max {
			return nil, ErrLineTooLong
		}
		line := append(buf, frag...)
		if err == io.EOF && len(bytes.TrimSpace(line)) > 0 {
			return line, nil
		}
		return line, err
	}
}

// drainLine consumes the rest of an oversized line so the stream resynchronizes
// at the next newline.
func drainLine(br *bufio.Reader) {
	for {
		_, err := br.ReadSlice('\n')
		if err == nil || err != bufio.ErrBufferFull {
			return
		}
	}
}
