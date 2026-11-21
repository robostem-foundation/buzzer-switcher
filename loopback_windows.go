//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// WASAPI loopback opens the default render endpoint, not the default mic.
// https://learn.microsoft.com/windows/win32/coreaudio/loopback-recording
type wasapiGUID struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

var (
	mmDeviceEnumeratorClass = wasapiGUID{0xbcde0395, 0xe52f, 0x467c, [8]byte{0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}}
	mmDeviceEnumeratorIID   = wasapiGUID{0xa95664d2, 0x9614, 0x4f35, [8]byte{0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}}
	audioClientIID          = wasapiGUID{0x1cb9ad4c, 0xdbfa, 0x4c32, [8]byte{0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2}}
	audioCaptureClientIID   = wasapiGUID{0xc8adbd64, 0xe71e, 0x48a0, [8]byte{0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}}
	ole32                   = syscall.NewLazyDLL("ole32.dll")
	coInitializeEx          = ole32.NewProc("CoInitializeEx")
	coUninitialize          = ole32.NewProc("CoUninitialize")
	coCreateInstance        = ole32.NewProc("CoCreateInstance")
	coTaskMemFree           = ole32.NewProc("CoTaskMemFree")
)

type wasapiUnknown struct{ vt *[15]uintptr }

func (o *wasapiUnknown) release() {
	syscall.SyscallN(o.vt[2], uintptr(unsafe.Pointer(o)))
}

func wasapiCheck(op string, raw uintptr) error {
	code := uint32(raw)
	if int32(code) >= 0 {
		return nil
	}
	const (
		deviceInvalidated = 0x88890004
		resourcesLost     = 0x88890026
		deviceNotFound    = 0x80070490
		serviceNotRunning = 0x88890010
	)
	switch code {
	case deviceInvalidated, resourcesLost:
		return fmt.Errorf("%s: Windows playback device changed or disconnected; restart listening (0x%08x)", op, code)
	case deviceNotFound:
		return fmt.Errorf("%s: no default Windows playback device is available (0x%08x)", op, code)
	case serviceNotRunning:
		return fmt.Errorf("%s: Windows Audio service is not running (0x%08x)", op, code)
	default:
		return fmt.Errorf("%s failed (Windows error 0x%08x)", op, code)
	}
}

type wasapiFormat struct {
	channels, rate, align, bits int
	floating                    bool
}

// WAVEFORMATEX is packed. Parse its fixed 18 byte header and the
// WAVEFORMATEXTENSIBLE extension by byte offset rather than Go struct padding.
func parseWasapiFormat(p unsafe.Pointer) (wasapiFormat, error) {
	const pcm, ieeeFloat, extensible = 1, 3, 0xfffe
	if p == nil {
		return wasapiFormat{}, errors.New("Windows supplied no playback audio format")
	}
	b := unsafe.Slice((*byte)(p), 18)
	tag := binary.LittleEndian.Uint16(b)
	f := wasapiFormat{
		channels: int(binary.LittleEndian.Uint16(b[2:])),
		rate:     int(binary.LittleEndian.Uint32(b[4:])),
		align:    int(binary.LittleEndian.Uint16(b[12:])),
		bits:     int(binary.LittleEndian.Uint16(b[14:])),
	}
	if tag == extensible {
		if binary.LittleEndian.Uint16(b[16:]) < 22 {
			return f, errors.New("Windows supplied a truncated playback audio format")
		}
		ext := unsafe.Slice((*byte)(p), 40)
		subtype := binary.LittleEndian.Uint32(ext[24:28])
		// PCM and IEEE float use the standard WAVEFORMATEX subtype GUID.
		if subtype != pcm && subtype != ieeeFloat ||
			string(ext[28:40]) != "\x00\x00\x10\x00\x80\x00\x00\xaa\x00\x38\x9b\x71" {
			return f, fmt.Errorf("unsupported Windows playback audio subtype 0x%08x", subtype)
		}
		tag = uint16(subtype)
	}
	f.floating = tag == ieeeFloat
	if (tag != pcm && tag != ieeeFloat) || f.channels < 1 || f.channels > 64 || f.rate < 1000 || f.rate > 768000 ||
		f.align != f.channels*(f.bits/8) || (f.floating && f.bits != 32 && f.bits != 64) ||
		(!f.floating && f.bits != 8 && f.bits != 16 && f.bits != 24 && f.bits != 32) {
		return f, fmt.Errorf("unsupported Windows playback audio format (tag 0x%x, %d channels, %d Hz, %d bits)", tag, f.channels, f.rate, f.bits)
	}
	return f, nil
}

// dst is mono. The device format is interleaved and arrives in native sample
// rate; no resampling or gain normalization is done here.
func (f wasapiFormat) mono(dst []float32, src []byte) {
	sz := f.bits / 8
	for i := range dst {
		frame := src[i*f.align:]
		var sum float64
		for ch := 0; ch < f.channels; ch++ {
			b := frame[ch*sz:]
			var v float64
			switch {
			case f.floating && sz == 4:
				v = float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
			case f.floating && sz == 8:
				v = math.Float64frombits(binary.LittleEndian.Uint64(b))
			case sz == 1:
				v = float64(int(b[0])-128) / 128
			case sz == 2:
				v = float64(int16(binary.LittleEndian.Uint16(b))) / 32768
			case sz == 3:
				leftAligned := int32(uint32(b[0])<<8 | uint32(b[1])<<16 | uint32(b[2])<<24)
				v = float64(leftAligned) / 2147483648
			case sz == 4:
				v = float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648
			}
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				sum += v
			}
		}
		dst[i] = float32(max(-1, min(1, sum/float64(f.channels))))
	}
}

// captureLoopback emits synchronous packets of mono PCM (only the playback
// endpoint's mix). It owns no files and never records or transmits samples.
// Call from a dedicated goroutine; emit should return promptly.
func captureLoopback(ctx context.Context, emit func(pcm []float32, sampleRate int)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return errors.New("desktop audio capture requires 64-bit Windows")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Windows Core Audio objects are obtained and released on the same COM
	// thread. A fresh goroutine normally has no prior COM apartment.
	result, _, _ := coInitializeEx.Call(0, 0) // COINIT_MULTITHREADED
	switch uint32(result) {
	case 0, 1: // S_OK, S_FALSE: both require CoUninitialize
		defer coUninitialize.Call()
	case 0x80010106: // RPC_E_CHANGED_MODE: this thread already has an apartment
	default:
		return wasapiCheck("Initialize Windows audio COM", result)
	}

	var enum, device, client, capClient *wasapiUnknown
	const clsctxAll = 23 // in-proc server/handler, local or remote if applicable
	result, _, _ = coCreateInstance.Call(
		uintptr(unsafe.Pointer(&mmDeviceEnumeratorClass)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&mmDeviceEnumeratorIID)), uintptr(unsafe.Pointer(&enum)),
	)
	if err := wasapiCheck("Find Windows audio devices", result); err != nil {
		return err
	}
	defer enum.release()

	// IMMDeviceEnumerator::GetDefaultAudioEndpoint (eRender, eMultimedia).
	result, _, _ = syscall.SyscallN(enum.vt[4], uintptr(unsafe.Pointer(enum)), 0, 1, uintptr(unsafe.Pointer(&device)))
	if err := wasapiCheck("Open the default Windows playback device", result); err != nil {
		return err
	}
	defer device.release()

	// IMMDevice::Activate.
	result, _, _ = syscall.SyscallN(device.vt[3], uintptr(unsafe.Pointer(device)),
		uintptr(unsafe.Pointer(&audioClientIID)), clsctxAll, 0, uintptr(unsafe.Pointer(&client)))
	if err := wasapiCheck("Activate Windows playback audio", result); err != nil {
		return err
	}
	defer client.release()

	// IAudioClient::GetMixFormat; the audio client allocates this with COM's
	// task allocator and expects the caller to free it.
	var format unsafe.Pointer
	result, _, _ = syscall.SyscallN(client.vt[8], uintptr(unsafe.Pointer(client)), uintptr(unsafe.Pointer(&format)))
	if err := wasapiCheck("Read the playback sample format", result); err != nil {
		return err
	}
	defer coTaskMemFree.Call(uintptr(format))
	f, err := parseWasapiFormat(format)
	if err != nil {
		return err
	}

	// IAudioClient::Initialize (shared; loopback; buffer 200ms; default
	// periodicity). Poll in short intervals, so no event-mode or driver
	// notification compatibility dependency.
	result, _, _ = syscall.SyscallN(client.vt[3], uintptr(unsafe.Pointer(client)), 0, 0x00020000,
		2_000_000, 0, uintptr(format), 0)
	if err := wasapiCheck("Initialize desktop audio loopback", result); err != nil {
		return err
	}

	// IAudioClient::GetService(IAudioCaptureClient).
	result, _, _ = syscall.SyscallN(client.vt[14], uintptr(unsafe.Pointer(client)),
		uintptr(unsafe.Pointer(&audioCaptureClientIID)), uintptr(unsafe.Pointer(&capClient)))
	if err := wasapiCheck("Open desktop audio capture", result); err != nil {
		return err
	}
	defer capClient.release()
	result, _, _ = syscall.SyscallN(client.vt[10], uintptr(unsafe.Pointer(client)))
	if err := wasapiCheck("Start desktop audio capture", result); err != nil {
		return err
	}
	defer syscall.SyscallN(client.vt[11], uintptr(unsafe.Pointer(client)))

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// IAudioCaptureClient::GetNextPacketSize.
		var pending uint32
		result, _, _ = syscall.SyscallN(capClient.vt[5], uintptr(unsafe.Pointer(capClient)), uintptr(unsafe.Pointer(&pending)))
		if err := wasapiCheck("Read desktop audio packet size", result); err != nil {
			return err
		}
		if pending == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				continue
			}
		}

		// IAudioCaptureClient::GetBuffer. Release the device buffer before
		// calling application code; when silent, data need not be valid.
		var data unsafe.Pointer
		var n, flags uint32
		result, _, _ = syscall.SyscallN(capClient.vt[3], uintptr(unsafe.Pointer(capClient)),
			uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&flags)), 0, 0)
		if err := wasapiCheck("Read desktop audio", result); err != nil {
			return err
		}
		if uint32(result) == 0x08890001 { // AUDCLNT_S_BUFFER_EMPTY
			continue
		}
		var pcm []float32
		var packetErr error
		if n > uint32(f.rate)*2 {
			packetErr = errors.New("Windows reported an invalid desktop audio packet length")
		} else if n > 0 {
			pcm = make([]float32, int(n))
			if flags&2 == 0 { // AUDCLNT_BUFFERFLAGS_SILENT
				if data == nil {
					packetErr = errors.New("Windows supplied a missing desktop audio buffer")
				} else {
					f.mono(pcm, unsafe.Slice((*byte)(data), int(n)*f.align))
				}
			}
		}
		result, _, _ = syscall.SyscallN(capClient.vt[4], uintptr(unsafe.Pointer(capClient)), uintptr(n))
		if err := wasapiCheck("Release desktop audio packet", result); err != nil {
			return err
		}
		if packetErr != nil {
			return packetErr
		}
		if len(pcm) != 0 && ctx.Err() == nil {
			emit(pcm, f.rate)
		}
	}
}
