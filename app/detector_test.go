package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func feedSignal(sig signature, x []float32, rate int, threshold float32) (int, float32) {
	d := newDetector(sig, threshold)
	hits := 0
	best := float32(0)
	for at := 0; at < len(x); at += 311 {
		end := min(at+311, len(x))
		d.push(x[at:end], rate, func(v float32) { hits++; best = max(best, v) })
	}
	return hits, best
}

func TestOfficialFixture(t *testing.T) {
	dir := os.Getenv("FIELDCAM_FIXTURES")
	if dir == "" {
		t.Skip("set FIELDCAM_FIXTURES to an installed TM Sounds fixture directory")
	}
	ref, _, err := defaultReference()
	if err != nil {
		t.Fatal(err)
	}
	cues := []string{"stop.wav", "start.wav", "warning.wav", "abort.wav", "pause.wav", "FiveSecondCountdown.wav", "Warning2.wav", "stop48000.wav", "stop44100.wav"}
	for _, name := range cues {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		x, rate, err := wave(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []float32{1, .12, .035} {
			buf := make([]float32, len(x)+rate/4)
			for i, s := range x {
				buf[i+rate/11] = s * v
			}
			hits, best := feedSignal(ref, buf, rate, .88)
			t.Logf("%s (%d Hz, gain %.3f): hits %d best %.4f", name, rate, v, hits, best)
			if (name[:4] == "stop") != (hits > 0) {
				t.Errorf("unexpected classification for %s", name)
			}
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "stop48000.wav"))
	x, rate, _ := wave(data)
	r := rand.New(rand.NewSource(9))
	for i, v := range x {
		x[i] = v*.4 + .013*float32(r.NormFloat64())
	}
	if hits, _ := feedSignal(ref, x, rate, .88); hits == 0 {
		t.Error("missed buzzer mixed with background noise")
	}
}
func TestNoSilenceMatch(t *testing.T) {
	ref, _, err := defaultReference()
	if err != nil {
		t.Fatal(err)
	}
	if hits, _ := feedSignal(ref, make([]float32, 48000*3), 48000, .88); hits != 0 {
		t.Fatal("silence matched")
	}
	r := rand.New(rand.NewSource(5))
	noise := make([]float32, 48000*3)
	for i := range noise {
		noise[i] = .07*float32(r.NormFloat64()) + .12*float32(math.Sin(float64(i)*.05))
	}
	if hits, _ := feedSignal(ref, noise, 48000, .88); hits != 0 {
		t.Fatal("noise matched")
	}
}
func TestFieldGuard(t *testing.T) {
	c := config{ICC: "ICC Field", Robo: "RoboSTEM Field"}
	if otherScene(c, c.ICC) != c.Robo || otherScene(c, c.Robo) != c.ICC || otherScene(c, "Lunch Break") != "" {
		t.Fatal("field guard failed")
	}
}
