package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartFixture(t *testing.T) {
	dir := os.Getenv("FIELDCAM_FIXTURES")
	if dir == "" {
		t.Skip("Set FIELDCAM_FIXTURES to local tournament sounds")
	}
	ref, _, err := startReference()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"start.wav", "stop.wav", "warning.wav", "abort.wav", "pause.wav", "FiveSecondCountdown.wav", "Warning2.wav"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		x, rate, err := wave(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, gain := range []float32{1, .12, .035} {
			buf := make([]float32, len(x)+rate/3)
			for i, s := range x {
				buf[i+rate/11] = s * gain
			}
			n, score := feedSignal(ref, buf, rate, .88)
			if (n > 0) != (name == "start.wav") {
				t.Errorf("start detector: %s gain %f score %f hits %d", name, gain, score, n)
			}
		}
	}
}

func TestGuardAndSceneOrder(t *testing.T) {
	c := config{Scenes: []string{"ICC", "RoboSTEM", "Third"}, RequireStart: true, MinMatchSec: 45, MaxMatchSec: 70, DelayMS: 2000}
	if err := validateMatchSettings(c); err != nil {
		t.Fatal(err)
	}
	if err := validateScenes(c.Scenes, c); err != nil {
		t.Fatal(err)
	}
	if otherScene(c, "ICC") != "RoboSTEM" || otherScene(c, "RoboSTEM") != "Third" || otherScene(c, "Third") != "ICC" || otherScene(c, "Awards") != "" {
		t.Fatal("wrong scene order")
	}
	now := time.Now()
	g := matchGuard{}
	if _, skip := g.end(c, now); skip == "" {
		t.Fatal("accepted without a start")
	}
	for _, tc := range []struct {
		sec    int
		accept bool
	}{{4, false}, {44, false}, {45, true}, {60, true}, {70, true}, {71, false}} {
		g.start = now
		if _, skip := g.end(c, now.Add(time.Duration(tc.sec)*time.Second)); (skip == "") != tc.accept {
			t.Errorf("%ds: %q", tc.sec, skip)
		}
		if !g.start.IsZero() {
			t.Fatal("did not consume the start")
		}
	}
}

func TestOBSReusesSocketAcrossChecksAndSwitches(t *testing.T) {
	serverResult := make(chan error, 4)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverResult <- mockUpgrade(w, r, func(stream *bufio.ReadWriter) error {
			if err := mockWriteFrame(stream, 0x81, []byte(`{"op":0,"d":{"rpcVersion":1}}`)); err != nil {
				return err
			}
			_, _, err := mockReadFrame(stream)
			if err != nil {
				return err
			}
			if err = mockWriteFrame(stream, 0x81, []byte(`{"op":2,"d":{"negotiatedRpcVersion":1}}`)); err != nil {
				return err
			}
			program := "ICC"
			script := []string{"GetSceneList", "GetSceneList", "GetCurrentProgramScene", "GetCurrentProgramScene", "SetCurrentProgramScene", "GetCurrentProgramScene", "GetCurrentProgramScene", "SetCurrentProgramScene"}
			for _, want := range script {
				_, p, err := mockReadFrame(stream)
				if err != nil {
					return err
				}
				var req struct {
					D struct {
						RequestID, RequestType string
						RequestData            struct{ SceneName string }
					}
				}
				if err = json.Unmarshal(p, &req); err != nil {
					return err
				}
				if req.D.RequestType != want {
					return fmt.Errorf("sent %q; want %q", req.D.RequestType, want)
				}
				var data string
				switch want {
				case "GetSceneList":
					data = `{"scenes":[{"sceneName":"ICC"},{"sceneName":"Robo"},{"sceneName":"Third"}]}`
				case "GetCurrentProgramScene":
					data = fmt.Sprintf(`{"currentProgramSceneName":%q}`, program)
				case "SetCurrentProgramScene":
					target := req.D.RequestData.SceneName
					if (program == "ICC" && target != "Robo") || (program == "Robo" && target != "Third") {
						return fmt.Errorf("unexpected switch %s -> %s", program, target)
					}
					program = target
					data = `{}`
				}
				msg := fmt.Sprintf(`{"op":7,"d":{"requestId":%q,"requestStatus":{"result":true,"code":100},"responseData":%s}}`, req.D.RequestID, data)
				if err := mockWriteFrame(stream, 0x81, []byte(msg)); err != nil {
					return err
				}
			}
			return nil
		})
	}))
	defer s.Close()
	a := new(app)
	defer a.closeOBS()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addr := "ws" + strings.TrimPrefix(s.URL, "http")
	for range 2 {
		names, err := a.checkOBS(ctx, addr, "")
		if err != nil || len(names) != 3 {
			t.Fatalf("scene list: %v %v", names, err)
		}
	}
	obs, err := a.openOBS(ctx, addr, "")
	if err != nil {
		t.Fatal(err)
	}
	c := config{Scenes: []string{"ICC", "Robo", "Third"}}
	for range 2 {
		a.transition(ctx, 0, obs, c, time.Now())
	}
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("switches did not complete on the same socket")
	}
	if !strings.Contains(strings.Join(a.snapshot().Log, "\n"), "Switched to Third") {
		t.Fatal(a.snapshot().Log)
	}
}
