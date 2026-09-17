package kube

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A minimal RFC 6455 client: enough for the API server's pods/exec
// endpoint. Client frames are masked, server frames must not be; fragmented
// messages are reassembled; pings are answered; a close frame is answered
// and reported as errWebSocketClosed.

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA

	wsGUID           = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessageSize = 32 << 20 // reassembled message bound
)

// wsCloseError is a close frame received from the peer.
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("websocket closed: %d %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("websocket closed: %d", e.Code)
}

var errWebSocketClosed = errors.New("websocket closed")

type wsConn struct {
	conn        net.Conn
	br          *bufio.Reader
	subprotocol string

	wmu       sync.Mutex
	closeOnce sync.Once
	closed    bool
}

// dialWebSocket performs the opening handshake for u (an http or https
// URL) offering the subprotocols, with header added to the request. A
// non-101 answer from the API server is returned as a *StatusError.
func dialWebSocket(ctx context.Context, u *url.URL, tlsConfig *tls.Config, header http.Header, subprotocols []string, timeout time.Duration) (*wsConn, error) {
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	dialer := &net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("kube: dial %s: %w", host, err)
	}
	conn := raw
	if u.Scheme == "https" {
		cfg := tlsConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		tlsConn := tls.Client(raw, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("kube: TLS to %s: %w", host, err)
		}
		conn = tlsConn
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		conn.Close()
		return nil, err
	}
	nonce := base64.StdEncoding.EncodeToString(key)
	req := &http.Request{Method: http.MethodGet, URL: u, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Host: u.Host}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", nonce)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if len(subprotocols) > 0 {
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(subprotocols, ", "))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("kube: websocket handshake: %w", err)
	}
	br := bufio.NewReaderSize(conn, 32<<10)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("kube: websocket handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		err := statusError(resp)
		resp.Body.Close()
		conn.Close()
		return nil, err
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || !headerHasToken(resp.Header, "Connection", "upgrade") {
		conn.Close()
		return nil, errors.New("kube: websocket handshake: server did not upgrade")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAcceptKey(nonce) {
		conn.Close()
		return nil, errors.New("kube: websocket handshake: bad Sec-WebSocket-Accept")
	}
	chosen := resp.Header.Get("Sec-WebSocket-Protocol")
	if len(subprotocols) > 0 && !contains(subprotocols, chosen) {
		conn.Close()
		return nil, fmt.Errorf("kube: websocket handshake: server chose subprotocol %q", chosen)
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br, subprotocol: chosen}, nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func wsAcceptKey(nonce string) string {
	sum := sha1.Sum([]byte(nonce + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// readMessage returns the next complete data message, answering pings on
// the way. A close frame from the peer is answered and returned as a
// *wsCloseError; after Close, reads fail with errWebSocketClosed.
func (w *wsConn) readMessage() (opcode byte, payload []byte, err error) {
	var message []byte
	var messageOp byte
	for {
		fin, op, frame, err := w.readFrame()
		if err != nil {
			if w.isClosed() {
				return 0, nil, errWebSocketClosed
			}
			return 0, nil, err
		}
		switch op {
		case wsOpPing:
			if err := w.writeFrame(wsOpPong, frame); err != nil {
				return 0, nil, err
			}
		case wsOpPong:
		case wsOpClose:
			closeErr := &wsCloseError{Code: 1005}
			if len(frame) >= 2 {
				closeErr.Code = int(binary.BigEndian.Uint16(frame))
				closeErr.Reason = string(frame[2:])
			}
			_ = w.writeClose(1000, "")
			return 0, nil, closeErr
		case wsOpContinuation:
			if messageOp == 0 {
				return 0, nil, errors.New("websocket: continuation without a message")
			}
			message = append(message, frame...)
			if len(message) > wsMaxMessageSize {
				return 0, nil, errors.New("websocket: message too large")
			}
			if fin {
				return messageOp, message, nil
			}
		case wsOpText, wsOpBinary:
			if messageOp != 0 {
				return 0, nil, errors.New("websocket: new message inside a fragmented one")
			}
			if fin {
				return op, frame, nil
			}
			messageOp, message = op, frame
		default:
			return 0, nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
	}
}

func (w *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(w.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	opcode = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
		if length>>63 != 0 {
			return false, 0, nil, errors.New("websocket: bad frame length")
		}
	}
	if opcode >= 0x8 && (!fin || length > 125) {
		return false, 0, nil, errors.New("websocket: bad control frame")
	}
	if length > wsMaxMessageSize {
		return false, 0, nil, errors.New("websocket: frame too large")
	}
	var mask [4]byte
	if masked {
		// Servers must not mask (RFC 6455 section 5.1); tolerate it.
		if _, err := io.ReadFull(w.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, opcode, payload, nil
}

// writeMessage sends one unfragmented, masked data frame.
func (w *wsConn) writeMessage(opcode byte, payload []byte) error {
	return w.writeFrame(opcode, payload)
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closed {
		return errWebSocketClosed
	}
	return w.writeFrameLocked(opcode, payload)
}

func (w *wsConn) writeFrameLocked(opcode byte, payload []byte) error {
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	head := make([]byte, 0, 14)
	head = append(head, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n <= 0xffff:
		head = append(head, 0x80|126, byte(n>>8), byte(n))
	default:
		head = append(head, 0x80|127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	head = append(head, mask[:]...)
	body := make([]byte, len(payload))
	for i, b := range payload {
		body[i] = b ^ mask[i&3]
	}
	if _, err := w.conn.Write(head); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := w.conn.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// writeClose sends a close frame once; later writes fail.
func (w *wsConn) writeClose(code uint16, reason string) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	payload := binary.BigEndian.AppendUint16(nil, code)
	payload = append(payload, reason...)
	return w.writeFrameLocked(wsOpClose, payload)
}

func (w *wsConn) isClosed() bool {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return w.closed
}

// Close sends a close frame on a best-effort basis and closes the
// connection, which ends any blocked read.
func (w *wsConn) Close() error {
	w.closeOnce.Do(func() {
		_ = w.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = w.writeClose(1000, "")
		w.conn.Close()
	})
	return nil
}
