// Package agent is the execution-host adapter. It has no HTTP or UI dependency.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Frame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params map[string]any  `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrStreamEnded is the agent's stream ending under the chat service: the
// execution worker closed it, the connection to the worker broke, or the
// agent wrote something the adapter could not read. The client's error
// wraps it with the cause, so the chat can say what happened and tell it
// from a failure the agent reported (chats/engine.go keeps the sandbox on
// a stream loss).
var ErrStreamEnded = errors.New("agent stream ended")

// A stream that knows why it ended (ClaudeStream) says so through Cause.
type causer interface{ Cause() error }

func (e *RPCError) Error() string { return e.Message }

// StartStream uses the same bidirectional app-server protocol over an execution
// worker connection. Closing it cancels the remote process, not the saved thread.
func StartStream(ctx context.Context, stream io.ReadWriteCloser, receive func(*Client, Frame)) (*Client, error) {
	ctx, cancel := context.WithCancel(ctx)
	c := &Client{input: stream, pending: map[string]chan Frame{}, done: make(chan struct{}), exit: make(chan struct{}), cancel: cancel}
	go func() { <-ctx.Done(); _ = stream.Close() }()
	go func() {
		defer close(c.exit)
		defer cancel()
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 65536), 8<<20)
		for scanner.Scan() {
			var f Frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				c.fail(fmt.Errorf("malformed agent frame: %w", err))
				return
			}
			if f.Method != "" {
				receive(c, f)
			} else if len(f.ID) > 0 {
				c.mu.Lock()
				ch := c.pending[string(f.ID)]
				delete(c.pending, string(f.ID))
				c.mu.Unlock()
				if ch != nil {
					ch <- f
				}
			}
		}
		err := scanner.Err()
		if cs, ok := stream.(causer); ok && cs.Cause() != nil {
			err = cs.Cause()
		}
		if err == nil {
			err = errors.New("the execution worker closed it")
		}
		c.fail(fmt.Errorf("%w: %v", ErrStreamEnded, err))
	}()
	if _, err := c.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "warden_chat", "title": "Warden", "version": "1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.Send(Frame{Method: "initialized"}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type Client struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan Frame
	next    atomic.Int64
	done    chan struct{}
	exit    chan struct{}
	err     error
	cancel  context.CancelFunc
	log     *os.File
}

func Start(ctx context.Context, executable, directory, workingDirectory string, receive func(*Client, Frame)) (*Client, error) {
	return StartWithOptions(ctx, executable, directory, workingDirectory, Options{}, receive)
}

// APIKeyMode keeps supplied credentials in memory and ignores inherited API
// settings, while retaining the existing Codex home for conversation history.
type Options struct{ APIKeyMode bool }

func StartWithOptions(ctx context.Context, executable, directory, workingDirectory string, options Options, receive func(*Client, Frame)) (*Client, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(directory, "stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	procCtx, cancel := context.WithCancel(ctx)
	args := []string{"app-server", "--listen", "stdio://"}
	if options.APIKeyMode {
		args = append(args, "-c", `cli_auth_credentials_store="ephemeral"`, "-c", `model_provider="openai"`, "-c", `forced_login_method="api"`, "-c", `model_providers.openai.base_url="https://api.openai.com/v1"`)
	}
	cmd := exec.CommandContext(procCtx, executable, args...)
	// Codex resolves host-level sandbox roots and project configuration at startup.
	// Never inherit the backend data directory as the execution workspace.
	cmd.Dir = workingDirectory
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	cmd.Env = []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "CODEX_THREAD_ID=") || strings.HasPrefix(e, "WORKSPACE_SERVER_TOKEN") {
			continue
		}
		if options.APIKeyMode && (strings.HasPrefix(e, "OPENAI_") || strings.HasPrefix(e, "CODEX_API_KEY=")) {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Stderr = &cappedWriter{file: log, left: 8 << 20}
	if options.APIKeyMode {
		cmd.Stderr = io.Discard
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		log.Close()
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		log.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		log.Close()
		return nil, err
	}
	c := &Client{cmd: cmd, input: input, pending: map[string]chan Frame{}, done: make(chan struct{}), exit: make(chan struct{}), cancel: cancel, log: log}
	// Descendants can inherit stdout and ignore SIGTERM. Bound cancellation even
	// when they keep the pipe open and prevent the scanner from reaching EOF.
	go func() {
		select {
		case <-c.exit:
			return
		case <-procCtx.Done():
		}
		select {
		case <-c.exit:
			return
		case <-time.After(3 * time.Second):
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = output.Close()
	}()
	go func() {
		defer close(c.exit)
		defer log.Close()
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 65536), 8<<20)
		for scanner.Scan() {
			var f Frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				c.fail(fmt.Errorf("malformed agent frame: %w", err))
				cancel()
				break
			}
			if f.Method != "" {
				receive(c, f)
			} else if len(f.ID) > 0 {
				c.mu.Lock()
				ch := c.pending[string(f.ID)]
				delete(c.pending, string(f.ID))
				c.mu.Unlock()
				if ch != nil {
					ch <- f
				}
			}
		}
		scanErr := scanner.Err()
		err := cmd.Wait()
		if scanErr != nil {
			err = scanErr
		}
		if err == nil {
			err = fmt.Errorf("agent process disconnected")
		}
		c.fail(err)
	}()
	if _, err = c.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "warden_chat", "title": "Warden", "version": "1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		c.Close()
		return nil, err
	}
	if err = c.Send(Frame{Method: "initialized"}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type cappedWriter struct {
	file *os.File
	left int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.left > 0 {
		part := p
		if len(part) > w.left {
			part = part[:w.left]
		}
		written, e := w.file.Write(part)
		w.left -= written
		if e != nil {
			return written, e
		}
	}
	return n, nil
}
func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	c.err = err
	close(c.done)
}
func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *Client) Send(f Frame) error {
	b, e := json.Marshal(f)
	if e != nil {
		return e
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, e = c.input.Write(append(b, '\n'))
	return e
}
func (c *Client) Reply(id json.RawMessage, result any) error {
	b, e := json.Marshal(result)
	if e != nil {
		return e
	}
	return c.Send(Frame{ID: id, Result: b})
}
func (c *Client) Call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := strconv.FormatInt(c.next.Add(1), 10)
	ch := make(chan Frame, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.Send(Frame{ID: json.RawMessage(id), Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case f := <-ch:
		if f.Error != nil {
			return nil, f.Error
		}
		var result map[string]any
		err := json.Unmarshal(f.Result, &result)
		return result, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.Err()
	}
}
func (c *Client) Close() {
	_ = c.input.Close()
	if c.cmd == nil {
		c.cancel()
		<-c.exit
		return
	}
	select {
	case <-c.exit:
	case <-time.After(time.Second):
		_ = c.cmd.Process.Kill()
		<-c.exit
	}
	c.cancel()
}
func Map(v any) map[string]any { m, _ := v.(map[string]any); return m }
func String(v any) string      { s, _ := v.(string); return s }
func Array(v any) []any        { a, _ := v.([]any); return a }
