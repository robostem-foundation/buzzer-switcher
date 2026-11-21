//go:build !windows

package main

import (
	"context"
	"errors"
)

func captureLoopback(ctx context.Context, emit func(pcm []float32, sampleRate int)) error {
	return errors.New("desktop playback capture is available on Windows only")
}
