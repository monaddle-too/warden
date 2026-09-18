package policy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"sync"
)

// ProviderEndpoints are the exact provider routes whose SSE responses may be
// streamed through bounded inspection. Anthropic's count_tokens is JSON only
// and keeps the buffered path.
var ProviderEndpoints = map[[2]string]bool{
	{"api.openai.com", "/v1/responses"}:             true,
	{"api.openai.com", "/v1/chat/completions"}:      true,
	{"chatgpt.com", "/backend-api/codex/responses"}: true,
	{"api.anthropic.com", "/v1/messages"}:           true,
}

// ResponseLimit bounds wire and decoded bytes of an inspected stream.
const ResponseLimit = 16 * 1024 * 1024

// StreamRejected carries only fixed, non-sensitive reasons into audit records.
type StreamRejected struct{ Reason string }

func (e *StreamRejected) Error() string { return e.Reason }

func rejectStream(reason string) error { return &StreamRejected{Reason: reason} }

// ResponseStream inspects provider SSE bytes incrementally: it decodes
// identity/gzip/deflate within limits and withholds any suffix that could
// still become a registered secret with the next chunk.
type ResponseStream struct {
	secrets   [][]byte
	pending   []byte
	Limit     int
	Received  int
	Decoded   int
	Delivered int
	Finished  bool
	Reason    string
	decoder   *incrementalDecoder
}

// NewResponseStream prepares inspection for the given content encoding.
func NewResponseStream(secrets []string, encoding string, limit int) (*ResponseStream, error) {
	if encoding != "identity" && encoding != "gzip" && encoding != "deflate" {
		return nil, rejectStream("unsupported response stream encoding")
	}
	if limit <= 0 {
		limit = ResponseLimit
	}
	s := &ResponseStream{Limit: limit}
	for _, secret := range secrets {
		if secret != "" {
			s.secrets = append(s.secrets, []byte(secret))
		}
	}
	if encoding != "identity" {
		s.decoder = newIncrementalDecoder(encoding, limit)
	}
	return s, nil
}

// Feed inspects one transport chunk; an empty chunk signals end of stream.
func (s *ResponseStream) Feed(chunk []byte) ([]byte, error) {
	if s.Reason != "" || s.Finished {
		return nil, nil
	}
	s.Received += len(chunk)
	if s.Received > s.Limit {
		return nil, rejectStream("response exceeds inspection limit")
	}
	data := chunk
	if s.decoder != nil {
		// Bound expansion as well as wire bytes, including compression bombs.
		decoded, err := s.decoder.feed(chunk, s.Limit-s.Decoded+1)
		if err != nil {
			return nil, err
		}
		if s.decoder.trailing() {
			return nil, rejectStream("trailing compressed response data")
		}
		if len(chunk) == 0 && !s.decoder.eof() {
			return nil, rejectStream("truncated compressed response")
		}
		data = decoded
	}
	s.Decoded += len(data)
	if s.Decoded > s.Limit {
		return nil, rejectStream("response exceeds inspection limit")
	}
	data = append(append([]byte{}, s.pending...), data...)
	for _, secret := range s.secrets {
		if bytes.Contains(data, secret) {
			return nil, rejectStream("provider response exposed a protected credential")
		}
	}
	// Retain only suffixes that could become a secret with the next chunk.
	// Ordinary SSE events therefore pass immediately, even with long tokens.
	keep := 0
	if len(chunk) > 0 {
		for _, secret := range s.secrets {
			limit := len(secret) - 1
			if len(data) < limit {
				limit = len(data)
			}
			for size := limit; size > keep; size-- {
				if bytes.HasSuffix(data, secret[:size]) {
					keep = size
					break
				}
			}
		}
	}
	output := data[:len(data)-keep]
	s.pending = append([]byte{}, data[len(data)-keep:]...)
	s.Delivered += len(output)
	s.Finished = len(chunk) == 0
	return output, nil
}

// Clear drops retained bytes and secrets once a stream is finished.
func (s *ResponseStream) Clear() {
	s.pending = nil
	s.secrets = nil
	if s.decoder != nil {
		s.decoder.close()
		s.decoder = nil
	}
}

// Summary describes the stream for audit.
func (s *ResponseStream) Summary(phase string) map[string]any {
	return map[string]any{"phase": phase, "received_bytes": s.Received, "decoded_bytes": s.Decoded, "forwarded_bytes": s.Delivered}
}

// incrementalDecoder adapts Go's pull-based decompressors to chunk feeding.
// A goroutine reads from a blocking buffer; feed returns once that goroutine
// has consumed all input and is waiting for more (or has finished).
type incrementalDecoder struct {
	mu       sync.Mutex
	cond     *sync.Cond
	input    []byte
	closed   bool
	waiting  bool
	done     bool
	err      error
	output   []byte
	decoded  int
	limit    int
	encoding string
	started  bool
}

func newIncrementalDecoder(encoding string, limit int) *incrementalDecoder {
	d := &incrementalDecoder{encoding: encoding, limit: limit}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// Read blocks until input is available or the stream is closed.
func (d *incrementalDecoder) Read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.input) == 0 && !d.closed {
		d.waiting = true
		d.cond.Broadcast()
		d.cond.Wait()
	}
	d.waiting = false
	if len(d.input) == 0 {
		return 0, io.EOF
	}
	n := copy(p, d.input)
	d.input = d.input[n:]
	return n, nil
}

func (d *incrementalDecoder) ReadByte() (byte, error) {
	var b [1]byte
	n, err := d.Read(b[:])
	if n == 0 {
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	return b[0], nil
}

func (d *incrementalDecoder) run() {
	var reader io.ReadCloser
	var err error
	switch d.encoding {
	case "gzip":
		var zr *gzip.Reader
		zr, err = gzip.NewReader(d)
		if err == nil {
			zr.Multistream(false)
			reader = zr
		}
	default:
		reader, err = zlib.NewReader(d)
	}
	buf := make([]byte, 32*1024)
	for err == nil {
		var n int
		n, err = reader.Read(buf)
		if n > 0 {
			d.mu.Lock()
			d.output = append(d.output, buf[:n]...)
			d.decoded += n
			over := d.decoded > d.limit
			d.mu.Unlock()
			if over {
				err = errors.New("decoded limit exceeded")
			}
		}
	}
	d.mu.Lock()
	d.done = true
	d.err = err
	d.cond.Broadcast()
	d.mu.Unlock()
}

// feed appends a chunk (empty means EOF) and returns newly decoded bytes.
func (d *incrementalDecoder) feed(chunk []byte, maxOutput int) ([]byte, error) {
	d.mu.Lock()
	if !d.started {
		d.started = true
		go d.run()
	}
	if len(chunk) == 0 {
		d.closed = true
	} else {
		d.input = append(d.input, chunk...)
	}
	d.waiting = false
	d.cond.Broadcast()
	for !d.done && !(d.waiting && len(d.input) == 0) {
		d.cond.Wait()
	}
	output := d.output
	d.output = nil
	err := d.err
	done := d.done
	d.mu.Unlock()
	if done && err != nil && err != io.EOF {
		if errors.Is(err, io.ErrUnexpectedEOF) && len(chunk) == 0 {
			return nil, rejectStream("truncated compressed response")
		}
		if err.Error() == "decoded limit exceeded" || len(output) > maxOutput {
			return nil, rejectStream("response exceeds inspection limit")
		}
		return nil, rejectStream("invalid response stream encoding")
	}
	if len(output) > maxOutput {
		return nil, rejectStream("response exceeds inspection limit")
	}
	return output, nil
}

func (d *incrementalDecoder) eof() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.done && d.err == io.EOF
}

func (d *incrementalDecoder) trailing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.done && d.err == io.EOF && len(d.input) > 0
}

func (d *incrementalDecoder) close() {
	d.mu.Lock()
	d.closed = true
	d.input = nil
	d.cond.Broadcast()
	d.mu.Unlock()
}
