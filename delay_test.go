package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type timingSceneRequest struct {
	typ, scene string
	at         time.Time
}

type timingMock struct {
	address  string
	mu       sync.Mutex
	program  string
	requests chan timingSceneRequest
	result   chan error
}

func newTimingOBS(t *testing.T) *timingMock {
	t.Helper()
	m := &timingMock{program: "ICC", requests: make(chan timingSceneRequest, 20), result: make(chan error, 1)}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.result <- mockUpgrade(w, r, func(stream *bufio.ReadWriter) error {
			if err := mockWriteFrame(stream, 0x81, []byte(`{"op":0,"d":{"rpcVersion":1}}`)); err != nil {
				return err
			}
			if _, _, err := mockReadFrame(stream); err != nil {
				return err
			}
			if err := mockWriteFrame(stream, 0x81, []byte(`{"op":2,"d":{"negotiatedRpcVersion":1}}`)); err != nil {
				return err
			}
			for {
				_, payload, err := mockReadFrame(stream)
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				var req struct {
					Op int
					D  struct {
						RequestID, RequestType string
						RequestData            struct{ SceneName string }
					}
				}
				if err := json.Unmarshal(payload, &req); err != nil {
					return err
				}
				if req.Op != 6 {
					return fmt.Errorf("unexpected OBS operation: %d", req.Op)
				}
				data := `{}`
				m.mu.Lock()
				switch req.D.RequestType {
				case "GetCurrentProgramScene":
					data = fmt.Sprintf(`{"currentProgramSceneName":%q}`, m.program)
				case "SetCurrentProgramScene":
					m.program = req.D.RequestData.SceneName
				default:
					m.mu.Unlock()
					return fmt.Errorf("unexpected OBS request type: %s", req.D.RequestType)
				}
				m.mu.Unlock()
				m.requests <- timingSceneRequest{req.D.RequestType, req.D.RequestData.SceneName, time.Now()}
				msg := fmt.Sprintf(`{"op":7,"d":{"requestId":%q,"requestStatus":{"result":true,"code":100},"responseData":%s}}`, req.D.RequestID, data)
				if err := mockWriteFrame(stream, 0x81, []byte(msg)); err != nil {
					return err
				}
			}
		})
	}))
	m.address = "ws" + strings.TrimPrefix(s.URL, "http")
	t.Cleanup(s.Close)
	return m
}

func (m *timingMock) manual(scene string) {
	m.mu.Lock()
	m.program = scene
	m.mu.Unlock()
}

func (m *timingMock) next(t *testing.T, want string) timingSceneRequest {
	t.Helper()
	select {
	case req := <-m.requests:
		if req.typ != want {
			t.Fatalf("sent %s, wanted %s", req.typ, want)
		}
		return req
	case err := <-m.result:
		t.Fatalf("OBS mock ended before %s: %v", want, err)
	case <-time.After(3 * time.Second):
		t.Fatalf("did not get %s", want)
	}
	return timingSceneRequest{}
}

func TestCueDeadlineRefinesOnlyBeforeInitialDue(t *testing.T) {
	start := time.Now().Add(-5 * time.Second)
	span := 1216 * time.Millisecond
	first := start.Add(1005 * time.Millisecond)
	for _, tt := range []struct {
		name  string
		delay time.Duration
		want  time.Time
	}{
		{"default", 2 * time.Second, start.Add(2 * time.Second)},
		{"custom", 1500 * time.Millisecond, start.Add(1500 * time.Millisecond)},
		{"zero", 0, first.Add(-span)},
		{"short", 1100 * time.Millisecond, first.Add(1100*time.Millisecond - span)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := newCueDeadline(first, .74, span, tt.delay)
			q.observe(start.Add(span), .99)
			q.observe(first.Add(400*time.Millisecond), 1)
			got, ok := q.resolve(context.Background())
			if !ok || !got.Equal(tt.want) {
				t.Errorf("due=%v, want=%v", got.Sub(start), tt.want.Sub(start))
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := newCueDeadline(time.Now(), .74, span, 2*time.Second)
	if _, ok := q.resolve(ctx); ok {
		t.Error("refinement survived cancellation")
	}
}

func TestBuzzerToOBSDelay(t *testing.T) {
	dir := os.Getenv("FIELDCAM_FIXTURES")
	if dir == "" {
		t.Skip("Set FIELDCAM_FIXTURES to the official tone fixtures")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "stop48000.wav"))
	if err != nil {
		t.Fatal(err)
	}
	x, rate, err := wave(raw)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := defaultReference()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		delay, sens int
		manual      bool
		cancel      bool
	}{
		{"default-2s", 2000, 88, false, false},
		{"custom-1.6s", 1600, 74, false, false},
		{"zero-as-recognized", 0, 88, false, false},
		{"short-as-recognized", 600, 88, false, false},
		{"manual-scene-change-cancels", 1750, 88, true, false},
		{"stop-cancels", 1750, 88, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newTimingOBS(t)
			a := &app{ref: ref}
			t.Cleanup(a.closeOBS)
			onset := make(chan time.Time, 1)
			recognition := make(chan time.Time, 1)
			capture := func(ctx context.Context, emit func([]float32, int)) error {
				lead := rate / 4
				samples := make([]float32, len(x)+lead+rate/20)
				r := rand.New(rand.NewSource(42))
				for i := range samples {
					samples[i] = .013 * float32(r.NormFloat64())
				}
				for i, v := range x {
					samples[i+lead] += .4 * v
				}
				verify := newDetector(ref, float32(tc.sens)/100)
				reported := false
				t0 := time.Now()
				onset <- t0.Add(time.Duration(lead) * time.Second / time.Duration(rate))
				// A patterned chunk size includes 38 ms bursts. A candidate
				// can end anywhere inside the current native packet.
				packet := 0
				for pos := 0; pos < len(samples); {
					frames := []int{rate / 100, rate / 80, rate * 38 / 1000, rate / 100}[packet%4]
					end := min(pos+frames, len(samples))
					when := t0.Add(time.Duration(end) * time.Second / time.Duration(rate))
					if !waitUntil(ctx, when) {
						return ctx.Err()
					}
					verify.push(samples[pos:end], rate, func(_ float32) {
						if !reported {
							recognition <- time.Now()
							reported = true
						}
					})
					emit(samples[pos:end], rate)
					packet++
					pos = end
				}
				<-ctx.Done()
				return ctx.Err()
			}
			c := config{Scenes: []string{"ICC", "RoboSTEM"}, DelayMS: tc.delay, Sensitivity: tc.sens}
			a.launchWithCapture(c, m.address, true, capture)
			t.Cleanup(a.stop)
			start := <-onset
			first := m.next(t, "GetCurrentProgramScene")
			rec := <-recognition
			switch {
			case tc.cancel:
				a.stop()
				waitUntil(context.Background(), start.Add(time.Duration(tc.delay+100)*time.Millisecond))
				select {
				case req := <-m.requests:
					t.Fatalf("unexpected OBS request after stop: %s", req.typ)
				default:
				}
			case tc.manual:
				m.manual("Awards")
				m.next(t, "GetCurrentProgramScene")
				waitUntil(context.Background(), start.Add(time.Duration(tc.delay+100)*time.Millisecond))
				select {
				case req := <-m.requests:
					t.Fatalf("unexpected OBS request after manual change: %s", req.typ)
				default:
				}
			default:
				m.next(t, "GetCurrentProgramScene")
				set := m.next(t, "SetCurrentProgramScene")
				if set.scene != "RoboSTEM" {
					t.Fatalf("cut to %q; want RoboSTEM", set.scene)
				}
				t.Logf("cue to actual OBS Set: %v; first recognized after %v; Set after recognition %v", set.at.Sub(start), first.at.Sub(start), set.at.Sub(rec))
				if tc.delay <= 600 {
					// Immediate means after a match can actually be
					// recognized, not before enough cue samples arrive.
					if dt := set.at.Sub(rec); dt < 0 || dt > 75*time.Millisecond {
						t.Errorf("short cut didn't dispatch on recognition: %v", dt)
					}
				} else if delta := set.at.Sub(start) - time.Duration(tc.delay)*time.Millisecond; delta < -30*time.Millisecond || delta > 75*time.Millisecond {
					t.Errorf("actual cut missed requested onset-relative time by %v", delta)
				}
			}
		})
	}
}
