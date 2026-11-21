package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const maxWSMessage = 4 << 20

type wsConn struct {
	net.Conn
	reader     *bufio.Reader
	mu         sync.Mutex
	closeOnce  sync.Once
	stopCancel func() bool
	ctx        context.Context
}

type wsCloseError struct{ code uint16 }

func (e *wsCloseError) Error() string { return fmt.Sprintf("WebSocket closed (code %d)", e.code) }

func dialWS(ctx context.Context, rawURL string, headers http.Header) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("expected a valid ws:// or wss:// address")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	dial := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 20 * time.Second}
	address := net.JoinHostPort(u.Hostname(), port)
	var conn net.Conn
	if u.Scheme == "wss" {
		d := tls.Dialer{NetDialer: dial, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}
		conn, err = d.DialContext(ctx, "tcp", address)
	} else {
		conn, err = dial.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, err
	}
	w := &wsConn{Conn: conn, reader: bufio.NewReader(conn), ctx: ctx}
	w.stopCancel = context.AfterFunc(ctx, func() { _ = conn.Close() })
	ready := false
	defer func() {
		if !ready {
			_ = w.Close()
		}
	}()
	if err := conn.SetDeadline(wsDeadline(ctx, 5*time.Second)); err != nil {
		return nil, err
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	// Write an HTTP/1.1 upgrade directly, keeping its buffered reader for frames.
	req := &http.Request{Method: "GET", URL: u, Host: u.Host, ProtoMajor: 1, ProtoMinor: 1, Header: headers.Clone()}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(w.reader, req)
	if err != nil {
		return nil, err
	}
	expected := sha1.Sum([]byte(key + websocketGUID))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("WebSocket upgrade rejected: HTTP %d", resp.StatusCode)
	}
	if !wsHeaderContains(resp.Header.Values("Connection"), "Upgrade") ||
		!wsHeaderContains(resp.Header.Values("Upgrade"), "websocket") ||
		resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(expected[:]) ||
		resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		return nil, errors.New("invalid WebSocket upgrade response")
	}
	if protocol := resp.Header.Get("Sec-WebSocket-Protocol"); protocol != "" && !wsHeaderContains(req.Header.Values("Sec-WebSocket-Protocol"), protocol) {
		return nil, errors.New("unexpected WebSocket subprotocol")
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	ready = true
	return w, nil
}

func wsDeadline(ctx context.Context, max time.Duration) time.Time {
	d := time.Now().Add(max)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		return cd
	}
	return d
}

func wsHeaderContains(lines []string, wanted string) bool {
	for _, l := range lines {
		for _, v := range strings.Split(l, ",") {
			if strings.EqualFold(strings.TrimSpace(v), wanted) {
				return true
			}
		}
	}
	return false
}

func (w *wsConn) Close() error {
	var err error
	w.closeOnce.Do(func() {
		if w.stopCancel != nil {
			w.stopCancel()
		}
		err = w.Conn.Close()
	})
	return err
}

func (w *wsConn) writeFrame(op byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := w.SetWriteDeadline(wsDeadline(w.ctx, 5*time.Second)); err != nil {
		return err
	}
	header := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 65535:
		header = binary.BigEndian.AppendUint16(append(header, 0x80|126), uint16(n))
	default:
		header = binary.BigEndian.AppendUint64(append(header, 0x80|127), uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	wire := append(header, mask[:]...)
	for i, b := range payload {
		wire = append(wire, b^mask[i%4])
	}
	nw, err := w.Write(wire)
	if err == nil && nw != len(wire) {
		return io.ErrShortWrite
	}
	return err
}

func (w *wsConn) WriteJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > maxWSMessage {
		return errors.New("outbound WebSocket message is too large")
	}
	return w.writeFrame(1, payload)
}

func (w *wsConn) ReadJSON(v any) error {
	var message []byte
	inProgress := false
	for {
		var header [2]byte
		if _, err := io.ReadFull(w.reader, header[:]); err != nil {
			return err
		}
		fin := header[0]&0x80 != 0
		op := header[0] & 0x0f
		if header[0]&0x70 != 0 || header[1]&0x80 != 0 {
			return errors.New("invalid WebSocket frame flags")
		}
		size := uint64(header[1] & 0x7f)
		if op >= 8 && (!fin || size > 125) {
			return errors.New("invalid WebSocket control frame")
		}
		switch size {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.reader, b[:]); err != nil {
				return err
			}
			size = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.reader, b[:]); err != nil {
				return err
			}
			size = binary.BigEndian.Uint64(b[:])
		}
		if size > maxWSMessage || (op < 8 && size+uint64(len(message)) > maxWSMessage) {
			return errors.New("inbound WebSocket message is too large")
		}
		data := make([]byte, int(size))
		if _, err := io.ReadFull(w.reader, data); err != nil {
			return err
		}
		switch op {
		case 0:
			if !inProgress {
				return errors.New("unexpected WebSocket continuation")
			}
			message = append(message, data...)
		case 1, 2:
			// Both servers ordinarily send text, but some TM versions may
			// send the JSON body in a binary frame instead.
			if inProgress {
				return errors.New("unexpected new WebSocket message")
			}
			message = data
			inProgress = true
		case 8:
			_ = w.writeFrame(8, data)
			if len(data) >= 2 {
				return &wsCloseError{binary.BigEndian.Uint16(data[:2])}
			}
			return io.EOF
		case 9:
			if err := w.writeFrame(10, data); err != nil {
				return err
			}
			continue
		case 10:
			continue
		default:
			return errors.New("unknown WebSocket frame type")
		}
		if fin {
			return json.Unmarshal(message, v)
		}
	}
}
