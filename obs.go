package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type obsEnvelope struct {
	Op   int             `json:"op"`
	Data json.RawMessage `json:"d"`
}

func obsAuthentication(password, salt, challenge string) string {
	first := sha256.Sum256([]byte(password + salt))
	second := sha256.Sum256([]byte(base64.StdEncoding.EncodeToString(first[:]) + challenge))
	return base64.StdEncoding.EncodeToString(second[:])
}

type obsResponse struct {
	RequestID string `json:"requestId"`
	Status    struct {
		Result  bool   `json:"result"`
		Code    int    `json:"code"`
		Comment string `json:"comment"`
	} `json:"requestStatus"`
	Data json.RawMessage `json:"responseData"`
}

type obsSession struct {
	ws      *wsConn
	next    atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan obsResponse
	done    chan struct{}
}

func connectOBS(ctx context.Context, address, password string) (*obsSession, error) {
	w, err := dialWS(ctx, address, http.Header{"Sec-Websocket-Protocol": {"obswebsocket.json"}})
	if err != nil {
		return nil, fmt.Errorf("connecting to OBS WebSocket: %w", err)
	}
	ready := false
	defer func() {
		if !ready {
			w.Close()
		}
	}()
	if err := w.SetReadDeadline(wsDeadline(ctx, 5*time.Second)); err != nil {
		return nil, err
	}

	var msg obsEnvelope
	if err := w.ReadJSON(&msg); err != nil {
		return nil, fmt.Errorf("waiting for OBS WebSocket 5 hello: %w", err)
	}
	if msg.Op != 0 {
		return nil, errors.New("OBS did not send a WebSocket 5 hello; use OBS 28+ with its built-in WebSocket server")
	}
	var hello struct {
		RPCVersion     int `json:"rpcVersion"`
		Authentication *struct {
			Salt      string
			Challenge string
		} `json:"authentication"`
	}
	if err := json.Unmarshal(msg.Data, &hello); err != nil || hello.RPCVersion < 1 {
		return nil, errors.New("invalid OBS hello; use the built-in OBS WebSocket 5 server")
	}
	identify := map[string]any{"rpcVersion": 1, "eventSubscriptions": 0}
	if a := hello.Authentication; a != nil {
		if password == "" {
			return nil, errors.New("OBS WebSocket requires a password")
		}
		identify["authentication"] = obsAuthentication(password, a.Salt, a.Challenge)
	}
	if err := w.WriteJSON(map[string]any{"op": 1, "d": identify}); err != nil {
		return nil, fmt.Errorf("sending OBS WebSocket identification: %w", err)
	}
	if err := w.ReadJSON(&msg); err != nil {
		var e *wsCloseError
		if errors.As(err, &e) && e.code == 4009 {
			return nil, errors.New("OBS WebSocket password rejected")
		}
		return nil, fmt.Errorf("identifying with OBS WebSocket: %w", err)
	}
	if msg.Op != 2 {
		return nil, errors.New("OBS WebSocket did not accept identification")
	}

	if err := w.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	s := &obsSession{ws: w, pending: make(map[string]chan obsResponse), done: make(chan struct{})}
	ready = true
	go s.read()
	return s, nil
}

func (s *obsSession) read() {
	defer close(s.done)
	defer s.ws.Close()
	for {
		var msg obsEnvelope
		if err := s.ws.ReadJSON(&msg); err != nil {
			return
		}
		if msg.Op != 7 {
			continue
		}
		var response obsResponse
		if err := json.Unmarshal(msg.Data, &response); err != nil {
			return
		}
		s.mu.Lock()
		if ch := s.pending[response.RequestID]; ch != nil {
			select {
			case ch <- response:
			default:
			}
		}
		s.mu.Unlock()
	}
}

func (s *obsSession) close() { s.ws.Close() }

func (s *obsSession) call(ctx context.Context, request string, data map[string]any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("OBS disconnected")
	default:
	}
	id := strconv.FormatUint(s.next.Add(1), 10)
	ch := make(chan obsResponse, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	payload := map[string]any{"requestType": request, "requestId": id}
	if len(data) > 0 {
		payload["requestData"] = data
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ws.WriteJSON(map[string]any{"op": 6, "d": payload}); err != nil {
		s.close()
		return fmt.Errorf("sending OBS request: %w", err)
	}
	finish := func(response obsResponse) error {
		if !response.Status.Result {
			if response.Status.Comment != "" {
				return fmt.Errorf("OBS %s failed (%d): %s", request, response.Status.Code, response.Status.Comment)
			}
			return fmt.Errorf("OBS %s failed (%d)", request, response.Status.Code)
		}
		if result != nil && len(response.Data) > 0 {
			if err := json.Unmarshal(response.Data, result); err != nil {
				return fmt.Errorf("reading OBS data: %w", err)
			}
		}
		return nil
	}
	select {
	case response := <-ch:
		return finish(response)
	case <-ctx.Done():
		return fmt.Errorf("OBS didn’t respond: %w", ctx.Err())
	case <-s.done:
		select {
		case response := <-ch:
			return finish(response)
		default:
			return errors.New("OBS disconnected")
		}
	}
}

func obsCall(ctx context.Context, address, password, request string, data map[string]any, result any) error {
	s, err := connectOBS(ctx, address, password)
	if err != nil {
		return err
	}
	defer s.close()
	return s.call(ctx, request, data, result)
}
