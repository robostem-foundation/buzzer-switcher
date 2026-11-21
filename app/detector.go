package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"math/cmplx"
)

const sampleRate = 8000
const fftSize = 256
const hop = 128
const bands = 51
const periodMS = 1000 * hop / sampleRate

type feature struct {
	b   [bands]float32
	rms float32
}
type signature struct {
	seq  []feature
	ms   int
	name string
}

func wave(raw []byte) ([]float32, int, error) {
	if len(raw) < 12 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, errors.New("choose a WAV file (PCM or IEEE float)")
	}
	var f, d []byte
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		n := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		start := off + 8
		if n < 0 || n > len(raw)-start {
			return nil, 0, errors.New("truncated WAV")
		}
		if id == "fmt " {
			f = raw[start : start+n]
		}
		if id == "data" {
			d = raw[start : start+n]
		}
		off = start + n + n%2
	}
	if len(f) < 16 || len(d) == 0 {
		return nil, 0, errors.New("WAV has no audio")
	}
	enc := binary.LittleEndian.Uint16(f)
	ch := int(binary.LittleEndian.Uint16(f[2:]))
	rate := int(binary.LittleEndian.Uint32(f[4:]))
	step := int(binary.LittleEndian.Uint16(f[12:]))
	depth := int(binary.LittleEndian.Uint16(f[14:]))
	if enc == 0xfffe && len(f) >= 40 {
		enc = binary.LittleEndian.Uint16(f[24:])
	}
	width := depth / 8
	if rate < 8000 || rate > 192000 || ch < 1 || ch > 8 || width < 1 || step < ch*width || (enc != 1 && enc != 3) {
		return nil, 0, errors.New("unsupported WAV format")
	}
	if (enc == 3 && depth != 32) || (enc == 1 && depth != 8 && depth != 16 && depth != 24 && depth != 32) {
		return nil, 0, errors.New("choose an 8/16/24/32-bit PCM or 32-bit float WAV")
	}
	if len(d)/step > rate*20 {
		return nil, 0, errors.New("use an isolated end-buzzer clip under 20 seconds")
	}
	out := make([]float32, len(d)/step)
	for i := range out {
		for j := 0; j < ch; j++ {
			q := d[i*step+j*width:]
			var v float32
			switch {
			case enc == 3:
				v = math.Float32frombits(binary.LittleEndian.Uint32(q))
			case depth == 8:
				v = float32(int(q[0])-128) / 128
			case depth == 16:
				v = float32(int16(binary.LittleEndian.Uint16(q))) / 32768
			case depth == 24:
				v = float32((int32(q[0])<<8|int32(q[1])<<16|int32(q[2])<<24)>>8) / 8388608
			case depth == 32:
				v = float32(int32(binary.LittleEndian.Uint32(q))) / 2147483648
			}
			if !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0) {
				out[i] += v / float32(ch)
			}
		}
	}
	return out, rate, nil
}

type resampler struct {
	carry, count, rate int
	sum                float64
}

func (r *resampler) push(src []float32, in int, emit func(float32, int)) {
	if in < sampleRate {
		return
	}
	if r.rate != in {
		*r = resampler{rate: in}
	}
	for i, v := range src {
		r.sum += float64(v)
		r.count++
		r.carry += sampleRate
		if r.carry >= in {
			emit(float32(r.sum/float64(r.count)), i+1)
			r.carry -= in
			r.sum = 0
			r.count = 0
		}
	}
}

var window = func() [fftSize]float64 {
	var z [fftSize]float64
	for i := range z {
		z[i] = .5 - .5*math.Cos(float64(i)*2*math.Pi/float64(fftSize-1))
	}
	return z
}()
var tw = func() [fftSize / 2]complex128 {
	var z [fftSize / 2]complex128
	for i := range z {
		z[i] = cmplx.Rect(1, -2*math.Pi*float64(i)/fftSize)
	}
	return z
}()

func spectrum(p []float32) feature {
	var z [fftSize]complex128
	v := feature{}
	var rms float64
	for i, x := range p {
		rms += float64(x * x)
		z[bits.Reverse8(uint8(i))] = complex(float64(x)*window[i], 0)
	}
	v.rms = float32(math.Sqrt(rms / fftSize))
	for size := 2; size <= fftSize; size *= 2 {
		h := size / 2
		for a := 0; a < fftSize; a += size {
			for i := 0; i < h; i++ {
				p, q := a+i, a+i+h
				w := tw[i*fftSize/size] * z[q]
				z[p], z[q] = z[p]+w, z[p]-w
			}
		}
	}
	var norm float64
	for i := range v.b {
		b := 5 + i*2 // 156 Hz to 3312 Hz
		p, q := z[b], z[b+1]
		w := real(p)*real(p) + imag(p)*imag(p) + real(q)*real(q) + imag(q)*imag(q)
		v.b[i] = float32(math.Sqrt(w))
		norm += w
	}
	if norm > 1e-9 {
		k := float32(1 / math.Sqrt(norm))
		for i := range v.b {
			v.b[i] *= k
		}
	}
	return v
}

func fingerprint(raw []byte, name string) (signature, error) {
	x, in, err := wave(raw)
	if err != nil {
		return signature{}, err
	}
	s := make([]float32, 0, len(x)*sampleRate/in)
	var r resampler
	r.push(x, in, func(v float32, _ int) { s = append(s, v) })
	if len(s) < fftSize+hop*4 {
		return signature{}, errors.New("end-sound clip is too short")
	}
	var seq []feature
	peak := float32(0)
	for i := 0; i+fftSize <= len(s); i += hop {
		f := spectrum(s[i : i+fftSize])
		seq = append(seq, f)
		if f.rms > peak {
			peak = f.rms
		}
	}
	if peak < .0002 {
		return signature{}, errors.New("the reference WAV is silent")
	}
	b := 0
	for b < len(seq) && seq[b].rms < peak*.14 {
		b++
	}
	e := len(seq)
	for e > b && seq[e-1].rms < peak*.20 {
		e--
	}
	if e-b > 75 {
		e = b + 75
	}
	if e-b < 5 {
		return signature{}, fmt.Errorf("only %d ms of usable audio; use the isolated end buzzer", (e-b)*periodMS)
	}
	return signature{seq: seq[b:e], ms: (e-b-1)*periodMS + fftSize*1000/sampleRate, name: name}, nil
}

type detector struct {
	ref       signature
	r         resampler
	samples   [fftSize]float32
	n         int
	hist      []feature
	p, have   int
	threshold float32
}

func newDetector(ref signature, limit float32) *detector {
	return &detector{ref: ref, threshold: limit, hist: make([]feature, len(ref.seq))}
}
func (d *detector) reset() { d.n, d.p, d.have = 0, 0, 0; d.r = resampler{} }
func (d *detector) push(p []float32, rate int, hit func(float32)) {
	d.pushTimed(p, rate, func(score float32, _ int) { hit(score) })
}

// The native frames remaining in the packet locate this match relative to the
// packet's end. Packets may contain several candidate analysis windows.
func (d *detector) pushTimed(p []float32, rate int, hit func(score float32, remainingFrames int)) {
	if len(d.hist) == 0 {
		return
	}
	d.r.push(p, rate, func(v float32, consumed int) {
		d.samples[d.n] = v
		d.n++
		if d.n != fftSize {
			return
		}
		f := spectrum(d.samples[:])
		copy(d.samples[:], d.samples[hop:])
		d.n = fftSize - hop
		d.hist[d.p] = f
		d.p = (d.p + 1) % len(d.hist)
		if d.have < len(d.hist) {
			d.have++
		}
		if d.have < len(d.hist) {
			return
		}
		hi := float32(0)
		for _, v := range d.hist {
			if v.rms > hi {
				hi = v.rms
			}
		}
		if hi < .002 {
			return
		}
		var score, weight float32
		off := 0
		for j, a := range d.ref.seq {
			if a.rms < .0002 {
				continue
			}
			b := d.hist[(d.p+j)%len(d.hist)]
			w := float32(math.Sqrt(float64(a.rms)))
			var dot float32
			for k, x := range a.b {
				dot += x * b.b[k]
			}
			// Silent or strongly muted portions cannot pass as an active buzzer.
			if b.rms < hi*.10 {
				dot *= .2
			}
			if dot < d.threshold-.12 {
				off++
			}
			score += dot * w
			weight += w
		}
		if weight > 0 && score/weight >= d.threshold && off <= len(d.hist)/6 {
			hit(score/weight, len(p)-consumed)
		}
	})
}
