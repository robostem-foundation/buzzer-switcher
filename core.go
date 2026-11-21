package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed iq-end-fingerprint.json
var officialJSON []byte

//go:embed iq-start-fingerprint.json
var officialStartJSON []byte

const officialName = "Built-in: official VEX IQ end cue"

func defaultReference() (signature, []byte, error) {
	return loadReference("Stop.wav", officialName, officialJSON)
}

func startReference() (signature, []byte, error) {
	return loadReference("Start.wav", "Built-in: official VEX IQ start cue", officialStartJSON)
}

func loadReference(filename, name string, builtin []byte) (signature, []byte, error) {
	if runtime.GOOS == "windows" {
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "ProgramW6432"} {
			root := os.Getenv(env)
			if root == "" {
				continue
			}
			path := filepath.Join(root, "VEX", "Tournament Manager", "Sounds", filename)
			info, e := os.Stat(path)
			if e != nil || info.Size() > 6<<20 || !info.Mode().IsRegular() {
				continue
			}
			if raw, e := os.ReadFile(path); e == nil {
				if fp, e := fingerprint(raw, "Installed Tournament Manager: "+filename); e == nil {
					return fp, raw, nil
				}
			}
		}
	}
	var w struct {
		MS  int `json:"m"`
		Seq []struct {
			B   [bands]float32 `json:"b"`
			RMS float32        `json:"r"`
		} `json:"s"`
	}
	if err := json.Unmarshal(builtin, &w); err != nil {
		return signature{}, nil, err
	}
	if len(w.Seq) < 5 || w.MS < 32 || w.MS >= 2000 {
		return signature{}, nil, errors.New("Invalid built-in buzzer reference")
	}
	fp := signature{ms: w.MS, name: name}
	for _, f := range w.Seq {
		fp.seq = append(fp.seq, feature{f.B, f.RMS})
	}
	return fp, nil, nil
}

type config struct {
	Action, Address, Password, ICC, Robo string
	Sensitivity                          int
	Scenes                               []string
	DelayMS                              int
	RequireStart                         bool
	MinMatchSec, MaxMatchSec             int
}
type state struct {
	Status    string   `json:"status"`
	Reference string   `json:"reference"`
	Mode      string   `json:"mode"`
	Level     float32  `json:"level"`
	CanPlay   bool     `json:"canPlay"`
	Log       []string `json:"log"`
}
type app struct {
	action                  sync.Mutex
	mu                      sync.Mutex
	state                   state
	cancel                  context.CancelFunc
	ref                     signature
	raw                     []byte
	run                     uint64
	meterAt                 time.Time
	obsMu                   sync.Mutex
	obs                     *obsSession
	obsAddress, obsPassword string
}

func (a *app) change(run uint64, status, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if run != 0 && run != a.run {
		return
	}
	if status != "" {
		a.state.Status = status
	}
	if msg != "" {
		a.state.Log = append(a.state.Log, time.Now().Format("15:04:05")+"  "+msg)
		if len(a.state.Log) > 36 {
			a.state.Log = a.state.Log[len(a.state.Log)-36:]
		}
	}
}
func (a *app) snapshot() state {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state
	if time.Since(a.meterAt) > 350*time.Millisecond {
		s.Level = 0
	}
	s.Log = append([]string{}, s.Log...)
	return s
}
func (a *app) stop() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.mu.Lock()
	a.run++
	a.state.Mode = "Off"
	a.state.Status = "Stopped"
	a.state.Level = 0
	a.mu.Unlock()
}
func (a *app) replace(raw []byte, name string) error {
	fp, err := fingerprint(raw, name)
	if err != nil {
		return err
	}
	a.stop()
	a.mu.Lock()
	a.raw = raw
	a.ref = fp
	a.state.Reference = name
	a.state.CanPlay = len(raw) > 0
	a.mu.Unlock()
	a.change(0, "Stopped", "Reference set: "+name)
	return nil
}
func wsURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("enter the OBS WebSocket address")
	}
	if !strings.Contains(s, "://") {
		if ip := net.ParseIP(s); ip != nil && strings.Contains(s, ":") {
			s = "[" + s + "]"
		}
		s = "ws://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u == nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") || (u.Scheme != "ws" && u.Scheme != "wss") {
		return "", errors.New("enter a host or host:port, like 127.0.0.1:4455")
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "4455")
	}
	return u.String(), nil
}
func (a *app) openOBS(ctx context.Context, address, password string) (*obsSession, error) {
	a.obsMu.Lock()
	defer a.obsMu.Unlock()
	if a.obs != nil {
		if address == a.obsAddress && password == a.obsPassword {
			select {
			case <-a.obs.done:
			default:
				return a.obs, nil
			}
		}
		a.obs.close()
		a.obs = nil
	}
	session, err := connectOBS(ctx, address, password)
	if err != nil {
		return nil, err
	}
	a.obs, a.obsAddress, a.obsPassword = session, address, password
	return session, nil
}

func (a *app) closeOBS() {
	a.obsMu.Lock()
	defer a.obsMu.Unlock()
	if a.obs != nil {
		a.obs.close()
		a.obs = nil
	}
	a.obsAddress, a.obsPassword = "", ""
}

func (a *app) checkOBS(ctx context.Context, address, password string) ([]string, error) {
	// The socket belongs to the app, not to this one request.
	session, err := a.openOBS(context.Background(), address, password)
	if err != nil {
		return nil, err
	}
	return session.scenes(ctx)
}

func obsScenes(ctx context.Context, address, password string) ([]string, error) {
	session, err := connectOBS(ctx, address, password)
	if err != nil {
		return nil, err
	}
	defer session.close()
	return session.scenes(ctx)
}
func (s *obsSession) scenes(ctx context.Context) ([]string, error) {
	var v struct {
		Scenes []struct {
			Name string `json:"sceneName"`
		} `json:"scenes"`
	}
	if err := s.call(ctx, "GetSceneList", nil, &v); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(v.Scenes))
	for _, scene := range v.Scenes {
		names = append(names, scene.Name)
	}
	return names, nil
}

func order(c config) []string {
	if c.Scenes != nil {
		return c.Scenes
	}
	return []string{c.ICC, c.Robo}
}
func validateScenes(names []string, c config) error {
	scenes := order(c)
	if len(scenes) < 2 {
		return errors.New("Add at least two scenes.")
	}
	seen := make(map[string]bool, len(scenes))
	for _, s := range scenes {
		if strings.TrimSpace(s) == "" {
			return errors.New("A scene name is blank.")
		}
		if seen[s] {
			return fmt.Errorf("Scene %q is listed twice.", s)
		}
		seen[s] = true
		found := false
		for _, x := range names {
			if x == s {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("OBS has no scene named %q.", s)
		}
	}
	return nil
}
func validateMatchSettings(c config) error {
	if c.DelayMS < 0 || c.DelayMS > 30000 {
		return errors.New("Use a delay between 0 and 30 seconds.")
	}
	if c.RequireStart && (c.MinMatchSec < 1 || c.MaxMatchSec > 600 || c.MinMatchSec > c.MaxMatchSec) {
		return errors.New("Use a match window between 1 and 600 seconds. The minimum can’t be higher than the maximum.")
	}
	return nil
}

func (s *obsSession) current(ctx context.Context) (string, error) {
	var v struct {
		Name string `json:"currentProgramSceneName"`
	}
	if err := s.call(ctx, "GetCurrentProgramScene", nil, &v); err != nil {
		return "", err
	}
	if v.Name == "" {
		return "", errors.New("OBS did not report a live scene.")
	}
	return v.Name, nil
}

func otherScene(c config, now string) string {
	scenes := order(c)
	if len(scenes) < 2 {
		return ""
	}
	for i, name := range scenes {
		if name == now {
			return scenes[(i+1)%len(scenes)]
		}
	}
	return ""
}

type matchGuard struct{ start time.Time }

func (g *matchGuard) end(c config, now time.Time) (time.Duration, string) {
	if !c.RequireStart {
		return 0, ""
	}
	if g.start.IsZero() {
		return 0, "No start buzzer."
	}
	elapsed := now.Sub(g.start)
	g.start = time.Time{}
	if elapsed < time.Duration(c.MinMatchSec)*time.Second || elapsed > time.Duration(c.MaxMatchSec)*time.Second {
		return elapsed, fmt.Sprintf("%.1fs after start; outside %d–%ds.", elapsed.Seconds(), c.MinMatchSec, c.MaxMatchSec)
	}
	return elapsed, ""
}
func (a *app) awaiting(c config, armed bool) string {
	if c.RequireStart {
		return "Waiting for start buzzer"
	}
	if armed {
		return "Waiting for end buzzer"
	}
	return "Listening for end buzzer"
}

// Accept the first match immediately, but refine its onset if there is time
// before the cut. A fuzzy threshold can match a frame window that only partly
// overlaps the cue; the best overlapping window often aligns more closely.
// Never delay a zero or already-due cut just to gather extra audio.
const cueAlignmentWindow = 250 * time.Millisecond

type cueDeadline struct {
	mu          sync.Mutex
	refineUntil time.Time
	best        time.Time
	score       float32
	span, delay time.Duration
}

func newCueDeadline(hit time.Time, score float32, span, delay time.Duration) *cueDeadline {
	deadline := hit.Add(delay - span)
	refineUntil := hit.Add(cueAlignmentWindow)
	if deadline.Before(refineUntil) {
		refineUntil = deadline
	}
	return &cueDeadline{refineUntil: refineUntil, best: hit, score: score, span: span, delay: delay}
}

func (q *cueDeadline) observe(hit time.Time, score float32) {
	if hit.After(q.refineUntil) {
		return
	}
	q.mu.Lock()
	if score > q.score {
		q.best, q.score = hit, score
	}
	q.mu.Unlock()
}

func waitUntil(ctx context.Context, due time.Time) bool {
	timer := time.NewTimer(time.Until(due))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func (q *cueDeadline) resolve(ctx context.Context) (time.Time, bool) {
	if !waitUntil(ctx, q.refineUntil) {
		return time.Time{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.best.Add(q.delay - q.span), true
}

func (a *app) transition(ctx context.Context, run uint64, obs *obsSession, c config, due time.Time) {
	a.transitionAt(ctx, run, obs, c, func(context.Context) (time.Time, bool) { return due, true })
}

func (a *app) transitionAt(ctx context.Context, run uint64, obs *obsSession, c config, resolve func(context.Context) (time.Time, bool)) {
	now, err := obs.current(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.change(run, "OBS check failed", err.Error())
		}
		return
	}
	to := otherScene(c, now)
	if to == "" {
		a.change(run, "No switch", fmt.Sprintf("Skipped: %q isn’t in the scene order.", now))
		return
	}
	a.change(run, "Next: "+to, fmt.Sprintf("%s → %s", now, to))
	due, ok := resolve(ctx)
	if !ok || !waitUntil(ctx, due) {
		return
	}
	again, err := obs.current(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.change(run, "OBS check failed", err.Error())
		}
		return
	}
	if again != now {
		a.change(run, "No switch", "Canceled: the live scene changed.")
		return
	}
	if ctx.Err() != nil {
		return
	}
	err = obs.call(ctx, "SetCurrentProgramScene", map[string]any{"sceneName": to}, nil)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		a.change(run, "Switch failed", err.Error())
		return
	}
	a.change(run, a.awaiting(c, true), "Switched to "+to+".")
}
func (a *app) launch(c config, address string, armed bool) {
	a.launchWithCapture(c, address, armed, captureLoopback)
}

// Injecting the packet source lets tests exercise cue detection all the way
// through the actual OBS requests without requiring a Windows audio endpoint.
func (a *app) launchWithCapture(c config, address string, armed bool, capture func(context.Context, func([]float32, int)) error) {
	a.stop()
	if err := validateMatchSettings(c); err != nil {
		a.change(0, "Check settings", err.Error())
		return
	}
	var startRef signature
	if c.RequireStart {
		var err error
		startRef, _, err = startReference()
		if err != nil {
			a.change(0, "Start buzzer unavailable", err.Error())
			return
		}
	}
	var obs *obsSession
	if armed {
		var err error
		obs, err = a.openOBS(context.Background(), address, c.Password)
		if err != nil {
			a.change(0, "OBS not connected", err.Error())
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Lock()
	run, ref := a.run, a.ref
	if armed {
		a.state.Mode = "Armed"
	} else {
		a.state.Mode = "Listen only"
	}
	a.state.Status = "Opening Windows playback…"
	a.mu.Unlock()
	limit := c.Sensitivity
	if limit < 74 || limit > 97 {
		limit = 88
	}
	if obs != nil {
		go func() {
			select {
			case <-ctx.Done():
			case <-obs.done:
				a.mu.Lock()
				active := a.run == run
				if active {
					a.state.Mode = "Off"
					a.state.Status = "OBS disconnected"
					a.state.Level = 0
				}
				a.mu.Unlock()
				if active {
					a.change(run, "", "OBS disconnected. Check OBS and arm again.")
					cancel()
				}
			}
		}()
	}
	go func() {
		defer cancel()
		d := newDetector(ref, float32(limit)/100)
		start := newDetector(startRef, float32(limit)/100)
		guard := matchGuard{}
		var lastEnd, lastStart, levelAt, previous time.Time
		var pending *cueDeadline
		err := capture(ctx, func(pcm []float32, rate int) {
			now := time.Now()
			if previous.IsZero() {
				a.change(run, a.awaiting(c, armed), "Windows playback connected.")
			} else if now.Sub(previous) > 250*time.Millisecond {
				d.reset()
				start.reset()
			}
			previous = now
			if now.Sub(levelAt) > 100*time.Millisecond {
				peak := float32(0)
				for _, v := range pcm {
					if v < 0 {
						v = -v
					}
					if v > peak {
						peak = v
					}
				}
				a.mu.Lock()
				if a.run == run {
					a.state.Level = float32(math.Min(float64(peak)*3, 1))
					a.meterAt = now
				}
				a.mu.Unlock()
				levelAt = now
			}
			if c.RequireStart {
				start.pushTimed(pcm, rate, func(score float32, remaining int) {
					at := now.Add(-time.Duration(remaining) * time.Second / time.Duration(rate))
					if ctx.Err() != nil || at.Sub(lastStart) < 2*time.Second {
						return
					}
					lastStart = at
					guard.start = at.Add(-time.Duration(startRef.ms) * time.Millisecond)
					a.change(run, "Waiting for end buzzer", fmt.Sprintf("Start sound matched (%d%%).", int(score*100+.5)))
				})
			}
			d.pushTimed(pcm, rate, func(score float32, remaining int) {
				at := now.Add(-time.Duration(remaining) * time.Second / time.Duration(rate))
				if ctx.Err() != nil {
					return
				}
				if at.Sub(lastEnd) < 2*time.Second {
					if pending != nil {
						pending.observe(at, score)
					}
					return
				}
				lastEnd = at
				pending = nil
				span := time.Duration(ref.ms) * time.Millisecond
				onset := at.Add(-span)
				elapsed, reason := guard.end(c, onset)
				if reason != "" {
					a.change(run, a.awaiting(c, armed), fmt.Sprintf("End ignored (%d%%). %s", int(score*100+.5), reason))
					return
				}
				msg := fmt.Sprintf("End sound matched (%d%%).", int(score*100+.5))
				if c.RequireStart {
					msg += fmt.Sprintf(" %ds after start.", int(elapsed.Round(time.Second)/time.Second))
				}
				a.change(run, "Buzzer detected", msg)
				if obs != nil {
					pending = newCueDeadline(at, score, span, time.Duration(c.DelayMS)*time.Millisecond)
					go a.transitionAt(ctx, run, obs, c, pending.resolve)
				}
			})
		})
		if err != nil && ctx.Err() == nil {
			a.change(run, "Capture stopped", err.Error())
			a.mu.Lock()
			if a.run == run {
				a.state.Mode = "Off"
				a.state.Level = 0
			}
			a.mu.Unlock()
		}
	}()
}
