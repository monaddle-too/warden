package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// The remote command protocol: every WebSocket message starts with a
// channel byte.
const (
	channelStdin  = 0
	channelStdout = 1
	channelStderr = 2
	channelStatus = 3
	channelResize = 4
	channelClose  = 255 // v5 only: the payload byte names the channel to close

	protocolV5 = "v5.channel.k8s.io"
	protocolV4 = "v4.channel.k8s.io"

	stdinFrameSize = 32 << 10
	// channelBufferLimit bounds the bytes held for a channel the caller
	// has not read yet; beyond it the session stops reading the socket
	// until the caller catches up.
	channelBufferLimit = 16 << 20
)

// ExecOptions shape an Exec. Stdin opens the stdin channel; TTY allocates a
// terminal, which merges stderr into stdout and enables Resize.
type ExecOptions struct {
	Stdin bool
	TTY   bool
}

// Exec runs command in the pod's container over a WebSocket to pods/exec
// and returns the session at once. The v5 subprotocol is preferred (it can
// half-close stdin); v4 is accepted when the server offers only that, in
// which case closing stdin is a no-op and a command that waits for stdin's
// end never sees it. Errors from the handshake (a missing pod, RBAC) are
// *StatusError. Cancelling ctx closes the socket: the readers see EOF and
// Wait returns ctx's error unless the command had already reported its
// status.
func (c *Client) Exec(ctx context.Context, namespace, pod, container string, command []string, opts ExecOptions) (*Session, error) {
	if namespace == "" || pod == "" {
		return nil, errors.New("kube: Exec needs a namespace and a pod")
	}
	if len(command) == 0 {
		return nil, errors.New("kube: Exec needs a command")
	}
	q := url.Values{"command": command, "stdout": {"true"}, "stderr": {quoteBool(!opts.TTY)}, "stdin": {quoteBool(opts.Stdin)}, "tty": {quoteBool(opts.TTY)}}
	if container != "" {
		q.Set("container", container)
	}
	u := *c.base
	u.Path = c.base.Path + Pods.path(namespace, pod) + "/exec"
	u.RawQuery = q.Encode()
	token, err := c.token.get(false)
	if err != nil {
		return nil, err
	}
	header := http.Header{"User-Agent": {c.UserAgent}}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	dialCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	ws, err := dialWebSocket(dialCtx, &u, c.tls, header, []string{protocolV5, protocolV4}, c.timeout())
	if IsUnauthorized(err) && c.token.refreshable() {
		if token, err = c.token.get(true); err != nil {
			return nil, err
		}
		header.Set("Authorization", "Bearer "+token)
		ws, err = dialWebSocket(dialCtx, &u, c.tls, header, []string{protocolV5, protocolV4}, c.timeout())
	}
	if err != nil {
		return nil, err
	}
	s := &Session{ws: ws, ctx: ctx, protocol: ws.subprotocol, stdout: newChannelBuffer(), stderr: newChannelBuffer(), done: make(chan struct{})}
	s.stdin = &stdinWriter{s: s, open: opts.Stdin}
	go s.readLoop()
	go func() {
		select {
		case <-ctx.Done():
			s.abort()
		case <-s.done:
		}
	}()
	return s, nil
}

// Session is a running exec. Stdout and Stderr must be drained (or the
// session ends by Close or ctx) or the session stalls once
// channelBufferLimit bytes are pending on either.
type Session struct {
	ws       *wsConn
	ctx      context.Context
	protocol string
	stdin    *stdinWriter
	stdout   *channelBuffer
	stderr   *channelBuffer
	done     chan struct{}

	mu     sync.Mutex
	status *Status
	err    error
}

// Subprotocol is the negotiated remote command protocol.
func (s *Session) Subprotocol() string { return s.protocol }

// Stdin is the command's standard input. Close sends the end of input
// under v5 and is a no-op under v4. Without ExecOptions.Stdin, writes
// fail.
func (s *Session) Stdin() io.WriteCloser { return s.stdin }

// Stdout is the command's standard output (and its stderr under TTY). It
// returns io.EOF once the session has ended and the buffered output is
// read.
func (s *Session) Stdout() io.Reader { return s.stdout }

// Stderr is the command's standard error; see Stdout.
func (s *Session) Stderr() io.Reader { return s.stderr }

// Resize sets the terminal size of a TTY session.
func (s *Session) Resize(cols, rows uint16) error {
	payload, _ := json.Marshal(struct {
		Width  uint16 `json:"Width"`
		Height uint16 `json:"Height"`
	}{cols, rows})
	return s.ws.writeMessage(wsOpBinary, append([]byte{channelResize}, payload...))
}

// Wait blocks until the session ends and returns the command's exit code.
// A command that ended without an exit code (the container vanished, the
// server refused the command) returns an error; a session ended by ctx
// returns ctx's error; a session closed by Close returns an error.
func (s *Session) Wait() (int, error) {
	<-s.done
	s.mu.Lock()
	status, err := s.status, s.err
	s.mu.Unlock()
	if status != nil {
		return exitCode(status)
	}
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	var closed *wsCloseError
	if err == nil || errors.Is(err, errWebSocketClosed) || errors.As(err, &closed) {
		return -1, errors.New("kube: exec ended without a status")
	}
	return -1, fmt.Errorf("kube: exec: %w", err)
}

// Close ends the session: the socket is closed and the readers see EOF.
func (s *Session) Close() error {
	s.abort()
	<-s.done
	return nil
}

func (s *Session) abort() {
	s.ws.Close()
	s.stdout.Close()
	s.stderr.Close()
}

func (s *Session) readLoop() {
	var err error
	for {
		op, msg, e := s.ws.readMessage()
		if e != nil {
			err = e
			break
		}
		if (op != wsOpBinary && op != wsOpText) || len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case channelStdout:
			if _, e := s.stdout.Write(msg[1:]); e != nil {
				err = e
			}
		case channelStderr:
			if _, e := s.stderr.Write(msg[1:]); e != nil {
				err = e
			}
		case channelStatus:
			var status Status
			if json.Unmarshal(msg[1:], &status) == nil {
				s.mu.Lock()
				s.status = &status
				s.mu.Unlock()
			}
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.abort()
	close(s.done)
}

// exitCode reads the exit code out of the channel-3 Status.
func exitCode(status *Status) (int, error) {
	if status.Status == "Success" {
		return 0, nil
	}
	if status.Reason == "NonZeroExitCode" && status.Details != nil {
		for _, cause := range status.Details.Causes {
			if cause.Reason == "ExitCode" {
				if code, err := strconv.Atoi(cause.Message); err == nil {
					return code, nil
				}
			}
		}
	}
	return -1, statusErrorFrom(*status)
}

type stdinWriter struct {
	s      *Session
	mu     sync.Mutex
	open   bool
	closed bool
}

func (w *stdinWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.open {
		return 0, errors.New("kube: exec has no stdin")
	}
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), stdinFrameSize)
		frame := make([]byte, 0, n+1)
		frame = append(frame, channelStdin)
		frame = append(frame, p[:n]...)
		if err := w.s.ws.writeMessage(wsOpBinary, frame); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (w *stdinWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.open || w.closed {
		return nil
	}
	w.closed = true
	if w.s.protocol != protocolV5 {
		return nil
	}
	err := w.s.ws.writeMessage(wsOpBinary, []byte{channelClose, channelStdin})
	if errors.Is(err, errWebSocketClosed) {
		return nil
	}
	return err
}

// channelBuffer is a byte queue between the socket reader and one of the
// session's readers. Write blocks above channelBufferLimit; Read blocks
// until data or Close; Close is idempotent and lets Read drain what is
// there before io.EOF.
type channelBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newChannelBuffer() *channelBuffer {
	b := &channelBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *channelBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf)+len(p) > channelBufferLimit && !b.closed {
		b.cond.Wait()
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	b.buf = append(b.buf, p...)
	b.cond.Broadcast()
	return len(p), nil
}

func (b *channelBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	if len(b.buf) == 0 {
		b.buf = nil
	}
	b.cond.Broadcast()
	return n, nil
}

func (b *channelBuffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// Logs streams the container's log; with follow it stays open until the
// container ends or ctx is cancelled, which ends reads with ctx's error.
// The caller closes the reader.
func (c *Client) Logs(ctx context.Context, namespace, pod, container string, follow bool) (io.ReadCloser, error) {
	if namespace == "" || pod == "" {
		return nil, errors.New("kube: Logs needs a namespace and a pod")
	}
	q := url.Values{}
	if container != "" {
		q.Set("container", container)
	}
	if follow {
		q.Set("follow", "true")
	}
	resp, err := c.stream(ctx, http.MethodGet, Pods.path(namespace, pod)+"/log", q, "text/plain")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
