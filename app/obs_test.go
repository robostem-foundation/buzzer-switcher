package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mockWriteFrame(w *bufio.ReadWriter, byte0 byte, p []byte) error {
	header := []byte{byte0}
	if len(p) < 126 {
		header = append(header, byte(len(p)))
	} else {
		header = binary.BigEndian.AppendUint16(append(header, 126), uint16(len(p)))
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(p); err != nil {
		return err
	}
	return w.Flush()
}

func mockReadFrame(r *bufio.ReadWriter) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[1]&0x80 == 0 {
		return 0, nil, fmt.Errorf("client sent an unmasked frame")
	}
	n := int(h[1] & 0x7f)
	if n == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return 0, nil, err
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	for i := range data {
		data[i] ^= mask[i%4]
	}
	return h[0] & 0x0f, data, nil
}

func mockUpgrade(w http.ResponseWriter, r *http.Request, f func(*bufio.ReadWriter) error) error {
	c, stream, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if _, err := fmt.Fprintf(stream, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:])); err != nil {
		return err
	}
	if err := stream.Flush(); err != nil {
		return err
	}
	return f(stream)
}

func TestOBSAuthenticatedSwitch(t *testing.T) {
	serverResult := make(chan error, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverResult <- mockUpgrade(w, r, func(stream *bufio.ReadWriter) error {
			if r.Header.Get("Sec-WebSocket-Protocol") != "obswebsocket.json" {
				return fmt.Errorf("unexpected subprotocol")
			}
			if err := mockWriteFrame(stream, 0x01, []byte(`{"op":0,"d":{"rpcVersion":1,`)); err != nil {
				return err
			}
			if err := mockWriteFrame(stream, 0x89, []byte("keepalive")); err != nil {
				return err
			}
			if err := mockWriteFrame(stream, 0x80, []byte(`"authentication":{"challenge":"+IxH4CnCiqpX1rM9scsNynZzbOe4KhDeYcTNS3PDaeY=","salt":"lM1GncleQOaCu9lT1yeUZhFYnqhsLLP1G5lAGo3ixaI="}}}`)); err != nil {
				return err
			}
			op, p, err := mockReadFrame(stream)
			if err != nil || op != 10 || string(p) != "keepalive" {
				return fmt.Errorf("expected pong: %d %q %v", op, p, err)
			}
			_, p, err = mockReadFrame(stream)
			if err != nil {
				return err
			}
			var identify struct {
				Op int `json:"op"`
				D  struct {
					RPCVersion     int
					Authentication string
				} `json:"d"`
			}
			if err := json.Unmarshal(p, &identify); err != nil {
				return err
			}
			if identify.Op != 1 || identify.D.RPCVersion != 1 || identify.D.Authentication != "1Ct943GAT+6YQUUX47Ia/ncufilbe6+oD6lY+5kaCu4=" {
				return fmt.Errorf("invalid OBS identify")
			}
			if err := mockWriteFrame(stream, 0x81, []byte(`{"op":2,"d":{"negotiatedRpcVersion":1}}`)); err != nil {
				return err
			}
			_, p, err = mockReadFrame(stream)
			if err != nil {
				return err
			}
			var request struct {
				Op int `json:"op"`
				D  struct {
					RequestType string
					RequestID   string
					RequestData struct{ SceneName string }
				} `json:"d"`
			}
			if err := json.Unmarshal(p, &request); err != nil {
				return err
			}
			if request.Op != 6 || request.D.RequestType != "SetCurrentProgramScene" || request.D.RequestData.SceneName != "RoboSTEM" {
				return fmt.Errorf("unexpected OBS request: %s", p)
			}
			status := fmt.Sprintf(`{"op":7,"d":{"requestId":%q,"requestStatus":{"result":true,"code":100}}}`, request.D.RequestID)
			return mockWriteFrame(stream, 0x81, []byte(status))
		})
	}))
	defer s.Close()
	err := obsCall(context.Background(), "ws"+strings.TrimPrefix(s.URL, "http"), "supersecretpassword", "SetCurrentProgramScene", map[string]any{"sceneName": "RoboSTEM"}, nil)
	if serverErr := <-serverResult; serverErr != nil {
		t.Fatal(serverErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestWSCancelIdleRead(t *testing.T) {
	closed := make(chan struct{})
	serverResult := make(chan error, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverResult <- mockUpgrade(w, r, func(stream *bufio.ReadWriter) error {
			<-closed
			return nil
		})
	}))
	defer s.Close()
	defer close(closed)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := dialWS(ctx, "ws"+strings.TrimPrefix(s.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	result := make(chan error, 1)
	go func() { var m any; result <- c.ReadJSON(&m) }()
	cancel()
	select {
	case e := <-result:
		if e == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation did not unblock WebSocket read")
	}
}
