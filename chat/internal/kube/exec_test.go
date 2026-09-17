package kube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wsServer is the server side of a WebSocket for the exec tests: the
// handshake, unmasking of client frames, and unmasked server frames.
type wsServer struct {
	t        *testing.T
	conn     net.Conn
	br       *bufio.Reader
	protocol string
}

// upgrade performs the server handshake, choosing the first of protocols
// the client offered, and returns the hijacked connection.
func upgrade(t *testing.T, w http.ResponseWriter, r *http.Request, protocols ...string) *wsServer {
	t.Helper()
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHasToken(r.Header, "Connection", "upgrade") || r.Header.Get("Sec-WebSocket-Version") != "13" {
		t.Errorf("bad upgrade headers: %v", r.Header)
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		t.Errorf("bad Sec-WebSocket-Key %q", key)
	}
	var offered []string
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		offered = append(offered, strings.TrimSpace(p))
	}
	chosen := ""
	for _, p := range protocols {
		if contains(offered, p) {
			chosen = p
			break
		}
	}
	if chosen == "" {
		t.Errorf("no acceptable subprotocol in %v", offered)
	}
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: %s\r\n\r\n", wsAcceptKey(key), chosen)
	return &wsServer{t: t, conn: conn, br: brw.Reader, protocol: chosen}
}

// readFrame reads one client frame, which must be masked.
func (s *wsServer) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(s.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0f
	if head[1]&0x80 == 0 {
		return false, 0, nil, errors.New("client frame not masked")
	}
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(s.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(s.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if _, err := io.ReadFull(s.br, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(s.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return fin, opcode, payload, nil
}

// readMessage reads one complete data message; control frames are
// returned as they come (with their opcode) so tests can assert on them.
func (s *wsServer) readMessage() (byte, []byte, error) {
	var message []byte
	var messageOp byte
	for {
		fin, op, payload, err := s.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if op >= 0x8 {
			return op, payload, nil
		}
		if op != wsOpContinuation {
			messageOp = op
		}
		message = append(message, payload...)
		if fin {
			return messageOp, message, nil
		}
	}
}

// readChannel reads the next binary message and splits off the channel.
func (s *wsServer) readChannel() (byte, []byte, error) {
	op, msg, err := s.readMessage()
	if err != nil {
		return 0, nil, err
	}
	if op != wsOpBinary || len(msg) == 0 {
		return op, msg, fmt.Errorf("unexpected frame op %d len %d", op, len(msg))
	}
	return msg[0], msg[1:], nil
}

func (s *wsServer) writeFrame(fin bool, opcode byte, payload []byte) {
	head := []byte{opcode}
	if fin {
		head[0] |= 0x80
	}
	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n <= 0xffff:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	if _, err := s.conn.Write(append(head, payload...)); err != nil {
		s.t.Logf("server write: %v", err)
	}
}

func (s *wsServer) writeChannel(channel byte, data []byte) {
	s.writeFrame(true, wsOpBinary, append([]byte{channel}, data...))
}

func (s *wsServer) writeStatus(status Status) {
	raw, _ := json.Marshal(status)
	s.writeChannel(channelStatus, raw)
}

func (s *wsServer) writeClose(code uint16) {
	s.writeFrame(true, wsOpClose, binary.BigEndian.AppendUint16(nil, code))
}

func exitStatus(code int) Status {
	if code == 0 {
		return Status{TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"}
	}
	return Status{TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: "NonZeroExitCode", Message: fmt.Sprintf("command terminated with non-zero exit code: error executing command [sh -c exit %d], exit code %d", code, code),
		Details: &StatusDetails{Causes: []StatusCause{{Reason: "ExitCode", Message: fmt.Sprint(code)}}}}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

func TestExecEchoAndExitCodes(t *testing.T) {
	api := newFakeAPI(t)
	done := make(chan struct{})
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		q := r.URL.Query()
		if strings.Join(q["command"], " ") != "sh -c cat" || q.Get("container") != "guest" || q.Get("stdin") != "true" || q.Get("stdout") != "true" || q.Get("stderr") != "true" || q.Get("tty") != "false" {
			t.Errorf("query %v", q)
		}
		if r.URL.Path != "/api/v1/namespaces/ns/pods/sbx-1/exec" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request %s %v", r.URL.Path, r.Header)
		}
		s := upgrade(t, w, r, protocolV5, protocolV4)
		defer s.conn.Close()
		for {
			channel, data, err := s.readChannel()
			if err != nil {
				t.Errorf("server read: %v", err)
				return
			}
			switch channel {
			case channelStdin:
				s.writeChannel(channelStdout, data)
			case channelClose:
				if !bytes.Equal(data, []byte{channelStdin}) {
					t.Errorf("close payload %v", data)
				}
				s.writeChannel(channelStderr, []byte("done\n"))
				s.writeStatus(exitStatus(0))
				s.writeClose(1000)
				return
			default:
				t.Errorf("unexpected channel %d", channel)
			}
		}
	})
	api.setToken("secret")
	c := api.client()
	ctx := testContext(t)
	session, err := c.Exec(ctx, "ns", "sbx-1", "guest", []string{"sh", "-c", "cat"}, ExecOptions{Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	if session.Subprotocol() != protocolV5 {
		t.Fatalf("subprotocol %s", session.Subprotocol())
	}
	stdin := session.Stdin()
	for _, chunk := range []string{"hello ", "world\n"} {
		if _, err := io.WriteString(stdin, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after close: %v", err)
	}
	if out := readAll(t, session.Stdout()); out != "hello world\n" {
		t.Fatalf("stdout %q", out)
	}
	if out := readAll(t, session.Stderr()); out != "done\n" {
		t.Fatalf("stderr %q", out)
	}
	code, err := session.Wait()
	if err != nil || code != 0 {
		t.Fatalf("wait: %d %v", code, err)
	}
	<-done

	// Non-zero exit codes and failures without one.
	for _, tc := range []struct {
		status  Status
		code    int
		wantErr string
	}{
		{exitStatus(3), 3, ""},
		{exitStatus(127), 127, ""},
		{Status{Status: "Failure", Reason: "InternalError", Message: "container not found (\"guest\")", Code: 500}, -1, "container not found"},
		{Status{Status: "Failure", Reason: "NonZeroExitCode", Message: "no cause"}, -1, "no cause"},
	} {
		api.setExec(func(w http.ResponseWriter, r *http.Request) {
			s := upgrade(t, w, r, protocolV5)
			defer s.conn.Close()
			s.writeStatus(tc.status)
			s.writeClose(1000)
		})
		session, err := c.Exec(ctx, "ns", "sbx-1", "", []string{"sh", "-c", "exit"}, ExecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if q := api.lastRequest().Query; q.Has("container") || q.Get("stdin") != "false" {
			t.Fatalf("query %v", q)
		}
		if _, err := session.Stdin().Write([]byte("x")); err == nil {
			t.Fatal("stdin accepted without ExecOptions.Stdin")
		}
		code, err := session.Wait()
		if code != tc.code {
			t.Fatalf("exit code %d, want %d (%v)", code, tc.code, err)
		}
		if tc.wantErr == "" && err != nil {
			t.Fatal(err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Fatalf("error %v, want %q", err, tc.wantErr)
		}
		if out := readAll(t, session.Stdout()); out != "" {
			t.Fatalf("stdout %q", out)
		}
	}
	// A close without any status is an error, not an exit code.
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		s := upgrade(t, w, r, protocolV5)
		defer s.conn.Close()
		s.writeChannel(channelStdout, []byte("partial"))
		s.writeClose(1006)
	})
	session, err = c.Exec(ctx, "ns", "sbx-1", "", []string{"true"}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := session.Wait(); code != -1 || err == nil || !strings.Contains(err.Error(), "without a status") {
		t.Fatalf("wait: %d %v", code, err)
	}
	if out := readAll(t, session.Stdout()); out != "partial" {
		t.Fatalf("stdout %q", out)
	}
}

func TestExecHandshakeErrorsAndTokenRefresh(t *testing.T) {
	api := newFakeAPI(t)
	c := api.client()
	ctx := testContext(t)
	if _, err := c.Exec(ctx, "ns", "sbx-1", "", nil, ExecOptions{}); err == nil {
		t.Fatal("empty command accepted")
	}
	if _, err := c.Exec(ctx, "", "sbx-1", "", []string{"true"}, ExecOptions{}); err == nil {
		t.Fatal("empty namespace accepted")
	}
	// No handler: the fake answers 404 with a Status.
	if _, err := c.Exec(ctx, "ns", "sbx-1", "", []string{"true"}, ExecOptions{}); !IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusForbidden, "Forbidden", `pods "sbx-1" is forbidden: User "runner" cannot create resource "pods/exec"`)
	})
	if _, err := c.Exec(ctx, "ns", "sbx-1", "", []string{"true"}, ExecOptions{}); !IsForbidden(err) || !strings.Contains(err.Error(), "pods/exec") {
		t.Fatalf("want Forbidden, got %v", err)
	}
	// A server that upgrades without an acceptable subprotocol is refused.
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: base64.channel.k8s.io\r\n\r\n", wsAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
	})
	if _, err := c.Exec(ctx, "ns", "sbx-1", "", []string{"true"}, ExecOptions{}); err == nil || !strings.Contains(err.Error(), "subprotocol") {
		t.Fatalf("want a subprotocol error, got %v", err)
	}
	// A wrong accept key is refused.
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bogus\r\nSec-WebSocket-Protocol: v5.channel.k8s.io\r\n\r\n")
	})
	if _, err := c.Exec(ctx, "ns", "sbx-1", "", []string{"true"}, ExecOptions{}); err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
		t.Fatalf("want an accept error, got %v", err)
	}
	// A dial that cannot complete within the client's timeout fails.
	c.Timeout = 200 * time.Millisecond
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	if _, err := c.Exec(ctx, "ns", "sbx-1", "", []string{"true"}, ExecOptions{}); err == nil {
		t.Fatal("slow handshake accepted")
	}
	c.Timeout = DefaultTimeout

	// A rotated token: the cached one is refused once, the file is re-read.
	api.setToken("new")
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := api.config()
	cfg.Token, cfg.TokenFile = "", tokenFile
	fresh, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fresh.token.token, fresh.token.loaded = "old", time.Now()
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		s := upgrade(t, w, r, protocolV5)
		defer s.conn.Close()
		s.writeStatus(exitStatus(0))
		s.writeClose(1000)
	})
	before := len(api.recorded())
	session, err := fresh.Exec(ctx, "ns", "sbx-1", "", []string{"true"}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := session.Wait(); code != 0 || err != nil {
		t.Fatalf("wait %d %v", code, err)
	}
	reqs := api.recorded()[before:]
	if len(reqs) != 2 || reqs[0].Header.Get("Authorization") != "Bearer old" || reqs[1].Header.Get("Authorization") != "Bearer new" {
		t.Fatalf("expected a 401 retry with the new token, got %d requests", len(reqs))
	}
}

func TestExecCancellationClosesEverything(t *testing.T) {
	api := newFakeAPI(t)
	serverSaw := make(chan error, 1)
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		s := upgrade(t, w, r, protocolV5)
		defer s.conn.Close()
		s.writeChannel(channelStdout, []byte("partial"))
		// Hold the session open until the client goes away.
		for {
			op, _, err := s.readMessage()
			if err != nil {
				serverSaw <- err
				return
			}
			if op == wsOpClose {
				serverSaw <- nil
				return
			}
		}
	})
	c := api.client()
	ctx, cancel := context.WithCancel(testContext(t))
	session, err := c.Exec(ctx, "ns", "sbx-1", "", []string{"sleep", "infinity"}, ExecOptions{Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := session.Stdout().Read(buf)
	if err != nil || string(buf[:n]) != "partial" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	// A reader blocked on the socket is released by the cancellation.
	readErr := make(chan error, 1)
	go func() {
		_, err := session.Stderr().Read(make([]byte, 1))
		readErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-readErr:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stderr read after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stderr reader not released")
	}
	if _, err := session.Stdout().Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout read after cancel: %v", err)
	}
	if code, err := session.Wait(); code != -1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("wait after cancel: %d %v", code, err)
	}
	select {
	case err := <-serverSaw:
		if err != nil {
			t.Fatalf("server saw %v, want a close frame", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not see the socket close")
	}
	if _, err := session.Stdin().Write([]byte("x")); err == nil {
		t.Fatal("stdin write after cancel succeeded")
	}
	if err := session.Stdin().Close(); err != nil {
		t.Fatalf("stdin close after cancel: %v", err)
	}
	// Close is idempotent and Wait after it still answers.
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait after close: %v", err)
	}

	// Close without cancellation: the readers see EOF and Wait reports the
	// missing status.
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		s := upgrade(t, w, r, protocolV5)
		defer s.conn.Close()
		for {
			if _, _, err := s.readMessage(); err != nil {
				return
			}
		}
	})
	session, err = c.Exec(testContext(t), "ns", "sbx-1", "", []string{"sleep", "infinity"}, ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if out := readAll(t, session.Stdout()); out != "" {
		t.Fatalf("stdout %q", out)
	}
	if code, err := session.Wait(); code != -1 || err == nil || !strings.Contains(err.Error(), "without a status") {
		t.Fatalf("wait after close: %d %v", code, err)
	}
}

func TestExecFramingFragmentsPingsAndLargeMessages(t *testing.T) {
	api := newFakeAPI(t)
	big := bytes.Repeat([]byte("0123456789"), 7000) // 70000 bytes: a 64-bit length
	medium := bytes.Repeat([]byte("x"), 200)        // a 16-bit length
	stdinTotal := 100 << 10
	serverErr := make(chan error, 1)
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		s := upgrade(t, w, r, protocolV5)
		defer s.conn.Close()
		serverErr <- func() error {
			// A fragmented stdout message with a ping between fragments.
			s.writeFrame(false, wsOpBinary, []byte{channelStdout, 'a', 'b'})
			s.writeFrame(true, wsOpPing, []byte("keepalive"))
			s.writeFrame(false, wsOpContinuation, []byte("cd"))
			s.writeFrame(true, wsOpContinuation, []byte("ef"))
			op, payload, err := s.readMessage()
			if err != nil {
				return err
			}
			if op != wsOpPong || string(payload) != "keepalive" {
				return fmt.Errorf("expected a pong, got op %d %q", op, payload)
			}
			s.writeChannel(channelStdout, medium)
			s.writeChannel(channelStdout, big)
			// Then stdin arrives in frames of at most stdinFrameSize bytes.
			var got []byte
			frames := 0
			for len(got) < stdinTotal {
				channel, data, err := s.readChannel()
				if err != nil {
					return err
				}
				if channel != channelStdin {
					return fmt.Errorf("channel %d", channel)
				}
				if len(data) > stdinFrameSize {
					return fmt.Errorf("stdin frame of %d bytes", len(data))
				}
				got = append(got, data...)
				frames++
			}
			if frames < 4 {
				return fmt.Errorf("stdin arrived in %d frames", frames)
			}
			for i, b := range got {
				if b != byte(i%251) {
					return fmt.Errorf("stdin byte %d is %d", i, b)
				}
			}
			s.writeStatus(exitStatus(0))
			s.writeClose(1000)
			return nil
		}()
	})
	c := api.client()
	session, err := c.Exec(testContext(t), "ns", "sbx-1", "", []string{"cat"}, ExecOptions{Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	stdinData := make([]byte, stdinTotal)
	for i := range stdinData {
		stdinData[i] = byte(i % 251)
	}
	go func() {
		if _, err := session.Stdin().Write(stdinData); err != nil {
			t.Errorf("stdin write: %v", err)
		}
	}()
	out := readAll(t, session.Stdout())
	want := "abcdef" + string(medium) + string(big)
	if out != want {
		t.Fatalf("stdout %d bytes, want %d; prefix %q", len(out), len(want), out[:min(len(out), 20)])
	}
	if code, err := session.Wait(); code != 0 || err != nil {
		t.Fatalf("wait %d %v", code, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestExecTTYResizeAndV4(t *testing.T) {
	api := newFakeAPI(t)
	resized := make(chan string, 1)
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("tty") != "true" || q.Get("stderr") != "false" || q.Get("stdin") != "true" {
			t.Errorf("tty query %v", q)
		}
		s := upgrade(t, w, r, protocolV5)
		defer s.conn.Close()
		channel, data, err := s.readChannel()
		if err != nil {
			t.Error(err)
			return
		}
		if channel != channelResize {
			t.Errorf("channel %d", channel)
		}
		resized <- string(data)
		s.writeChannel(channelStdout, []byte("$ "))
		s.writeStatus(exitStatus(0))
		s.writeClose(1000)
	})
	c := api.client()
	session, err := c.Exec(testContext(t), "ns", "sbx-1", "guest", []string{"bash"}, ExecOptions{Stdin: true, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if got := <-resized; got != `{"Width":120,"Height":40}` {
		t.Fatalf("resize %s", got)
	}
	if out := readAll(t, session.Stdout()); out != "$ " {
		t.Fatalf("stdout %q", out)
	}
	if code, err := session.Wait(); code != 0 || err != nil {
		t.Fatalf("wait %d %v", code, err)
	}

	// A server that only speaks v4: stdin cannot be half-closed, so Close
	// sends nothing and the next frame the server sees is the close.
	proceed := make(chan struct{})
	sawOp := make(chan byte, 1)
	api.setExec(func(w http.ResponseWriter, r *http.Request) {
		s := upgrade(t, w, r, protocolV4)
		defer s.conn.Close()
		<-proceed
		s.writeStatus(exitStatus(0))
		s.writeClose(1000)
		op, _, err := s.readMessage()
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}
		sawOp <- op
	})
	session, err = c.Exec(testContext(t), "ns", "sbx-1", "guest", []string{"cat"}, ExecOptions{Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	if session.Subprotocol() != protocolV4 {
		t.Fatalf("subprotocol %s", session.Subprotocol())
	}
	if err := session.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if code, err := session.Wait(); code != 0 || err != nil {
		t.Fatalf("wait %d %v", code, err)
	}
	if op := <-sawOp; op != wsOpClose {
		t.Fatalf("server saw op %d after a v4 stdin close, want the close frame", op)
	}
}

func TestLogs(t *testing.T) {
	api := newFakeAPI(t)
	api.setLogs(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/namespaces/ns/pods/sbx-1/log" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Accept") != "text/plain" {
			t.Errorf("accept %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "container=%s\nline 1\n", q.Get("container"))
		if q.Get("follow") != "true" {
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	c := api.client()
	ctx := testContext(t)
	logs, err := c.Logs(ctx, "ns", "sbx-1", "guest", false)
	if err != nil {
		t.Fatal(err)
	}
	if out := readAll(t, logs); out != "container=guest\nline 1\n" {
		t.Fatalf("logs %q", out)
	}
	logs.Close()
	if q := api.lastRequest().Query; q.Has("follow") {
		t.Fatalf("query %v", q)
	}
	// Follow stays open until the context ends.
	fctx, cancel := context.WithCancel(ctx)
	logs, err = c.Logs(fctx, "ns", "sbx-1", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if q := api.lastRequest().Query; q.Get("follow") != "true" || q.Has("container") {
		t.Fatalf("query %v", q)
	}
	reader := bufio.NewReader(logs)
	if line, err := reader.ReadString('\n'); err != nil || line != "container=\n" {
		t.Fatalf("first line %q %v", line, err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "line 1\n" {
		t.Fatalf("second line %q %v", line, err)
	}
	cancel()
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("follow read succeeded after cancellation")
	}
	logs.Close()
	// Errors are StatusErrors.
	api.setLogs(func(w http.ResponseWriter, r *http.Request) {
		writeNotFound(w, apiPath{resource: "pods", name: "sbx-1"})
	})
	if _, err := c.Logs(ctx, "ns", "sbx-1", "", false); !IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
	if _, err := c.Logs(ctx, "ns", "", "", false); err == nil {
		t.Fatal("empty pod accepted")
	}
}
