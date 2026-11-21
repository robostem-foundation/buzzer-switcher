//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	user                = syscall.NewLazyDLL("user32.dll")
	kernel              = syscall.NewLazyDLL("kernel32.dll")
	gdi                 = syscall.NewLazyDLL("gdi32.dll")
	controls            = syscall.NewLazyDLL("comctl32.dll")
	dialogs             = syscall.NewLazyDLL("comdlg32.dll")
	multimedia          = syscall.NewLazyDLL("winmm.dll")
	registerClass       = user.NewProc("RegisterClassExW")
	createWindow        = user.NewProc("CreateWindowExW")
	defaultProc         = user.NewProc("DefWindowProcW")
	showWindow          = user.NewProc("ShowWindow")
	updateWindow        = user.NewProc("UpdateWindow")
	destroyWindow       = user.NewProc("DestroyWindow")
	getMessage          = user.NewProc("GetMessageW")
	translateMessage    = user.NewProc("TranslateMessage")
	dispatchMessage     = user.NewProc("DispatchMessageW")
	postQuit            = user.NewProc("PostQuitMessage")
	dialogMessage       = user.NewProc("IsDialogMessageW")
	windowText          = user.NewProc("GetWindowTextW")
	windowRect          = user.NewProc("GetWindowRect")
	windowTextLength    = user.NewProc("GetWindowTextLengthW")
	setWindowText       = user.NewProc("SetWindowTextW")
	send                = user.NewProc("SendMessageW")
	enableWindow        = user.NewProc("EnableWindow")
	positionWindow      = user.NewProc("SetWindowPos")
	focusWindow         = user.NewProc("SetFocus")
	loadCursor          = user.NewProc("LoadCursorW")
	loadIcon            = user.NewProc("LoadIconW")
	getColorBrush       = user.NewProc("GetSysColorBrush")
	getColor            = user.NewProc("GetSysColor")
	fillRect            = user.NewProc("FillRect")
	getSysMetrics       = user.NewProc("GetSystemMetrics")
	systemParameters    = user.NewProc("SystemParametersInfoW")
	adjustWindowRect    = user.NewProc("AdjustWindowRectEx")
	setDPIAware         = user.NewProc("SetProcessDPIAware")
	getDPIForSystem     = user.NewProc("GetDpiForSystem")
	messageBox          = user.NewProc("MessageBoxW")
	setTimer            = user.NewProc("SetTimer")
	killTimer           = user.NewProc("KillTimer")
	getModule           = kernel.NewProc("GetModuleHandleW")
	makeFont            = gdi.NewProc("CreateFontW")
	deleteGDI           = gdi.NewProc("DeleteObject")
	createPen           = gdi.NewProc("CreatePen")
	getStockGDI         = gdi.NewProc("GetStockObject")
	selectGDI           = gdi.NewProc("SelectObject")
	drawEllipse         = gdi.NewProc("Ellipse")
	moveGDI             = gdi.NewProc("MoveToEx")
	lineGDI             = gdi.NewProc("LineTo")
	initControls        = controls.NewProc("InitCommonControlsEx")
	pickFile            = dialogs.NewProc("GetOpenFileNameW")
	extendedDialogError = dialogs.NewProc("CommDlgExtendedError")
	playSound           = multimedia.NewProc("PlaySoundW")
	ui                  *winUI
)

const (
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmSetFont         = 0x0030
	wmDrawItem        = 0x002b
	wmCommand         = 0x0111
	wmTimer           = 0x0113
	wmHScroll         = 0x0114
	wsVisible         = 0x10000000
	wsChild           = 0x40000000
	wsTab             = 0x00010000
	wsVScroll         = 0x00200000
	wsClipChildren    = 0x02000000
	wsExClientEdge    = 0x00000200
	wsExControlParent = 0x00010000
	windowStyle       = 0x00c00000 | 0x00080000 | 0x00020000 | wsClipChildren // caption, system menu, minimize
	idScenes          = 101
	idLoad            = 102
	idPlay            = 103
	idReset           = 104
	idListen          = 105
	idArm             = 106
	idStop            = 107
	idAddress         = 108
	idPassword        = 109
	idThreshold       = 112
	idPlaylist        = 113
	idCandidate       = 114
	idAdd             = 115
	idRemove          = 116
	idUp              = 117
	idDown            = 118
	idDelay           = 119
	idRequireStart    = 120
	idMin             = 121
	idMax             = 122
	idHelpOBS         = 151
	idHelpPlaylist    = 152
	idHelpReference   = 153
	idHelpDetect      = 154
	idHelpStart       = 155
	idHelpRun         = 156
)

type winRect struct{ left, top, right, bottom int32 }
type winPoint struct{ x, y int32 }
type winDrawItem struct {
	controlType, controlID, itemID, action, state uint32
	window, dc                                    uintptr
	rect                                          winRect
	itemData                                      uintptr
}
type winMessage struct {
	window         uintptr
	event          uint32
	wparam, lparam uintptr
	time           uint32
	point          winPoint
	private        uint32
}
type winClass struct {
	size, style                        uint32
	proc                               uintptr
	clsExtra, wndExtra                 int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
	smallIcon                          uintptr
}
type winFile struct {
	size                         uint32
	owner, instance              uintptr
	filter, customFilter         *uint16
	maxCustomFilter, filterIndex uint32
	file                         *uint16
	maxFile                      uint32
	fileTitle                    *uint16
	maxFileTitle                 uint32
	initialDir, title            *uint16
	flags                        uint32
	fileOffset, fileExtension    uint16
	defaultExtension             *uint16
	customData, hook             uintptr
	templateName                 *uint16
	reserved                     uintptr
	reservedCount, flagsEx       uint32
}

type winTool struct {
	size, flags     uint32
	window, id      uintptr
	rect            winRect
	instance        uintptr
	text            *uint16
	param, reserved uintptr
}
type helpText struct {
	title, body string
}

var help = map[int]helpText{
	idHelpOBS:       {"OBS connection", "In OBS, open Tools → WebSocket Server Settings. Enable the server and enter the password here. Leave 127.0.0.1:4455 if OBS is on this PC."},
	idHelpPlaylist:  {"Scene order", "Add scenes in the order you want them to go live. A switch moves to the next scene, or back to the first at the end of the list. If the live scene isn’t listed, nothing switches."},
	idHelpReference: {"End buzzer", "Uses Tournament Manager’s Stop.wav if it is installed. Otherwise it uses the built-in end cue. Load WAV lets you pick your own. Play WAV is available only while stopped. Use it for a dry run, not on the live PA."},
	idHelpDetect:    {"Detection settings", "A higher threshold is stricter. Switch delay is measured from the start of the end buzzer. Very short delays still wait until the sound is recognized."},
	idHelpStart:     {"Start buzzer", "When checked, a start buzzer unlocks one switch. The end buzzer must arrive within the minimum and maximum time after it. Leave the range at 45–70 seconds for a one-minute match."},
	idHelpRun:       {"Listening and switching", "Start with Listen Only. The app listens to the default Windows playback device. Arm Switching allows OBS cuts. Stop cancels a pending cut. Use Stop before breaks or unrelated timers."},
}

// Local preferences intentionally contain neither OBS password nor running state.
type winSettings struct {
	Address      string   `json:"obsAddress"`
	Scenes       []string `json:"sceneOrder"`
	DelayMS      int      `json:"delayMs"`
	Sensitivity  int      `json:"threshold"`
	RequireStart bool     `json:"requireStart"`
	MinMatchSec  int      `json:"minMatchSec"`
	MaxMatchSec  int      `json:"maxMatchSec"`
}

func defaultWinSettings() winSettings {
	return winSettings{Address: "127.0.0.1:4455", Scenes: []string{"ICC Field", "RoboSTEM Field"}, DelayMS: 2000, Sensitivity: 88, RequireStart: true, MinMatchSec: 45, MaxMatchSec: 70}
}
func winSettingsPath() (string, error) {
	root := os.Getenv("LOCALAPPDATA")
	if root == "" {
		var err error
		root, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(root, "FieldCam Buzzer Switcher", "settings.json"), nil
}
func (s winSettings) normalized(old winSettings) winSettings {
	if _, e := wsURL(s.Address); e != nil || len(s.Address) > 256 {
		s.Address = old.Address
	}
	if s.DelayMS < 0 || s.DelayMS > 30000 {
		s.DelayMS = old.DelayMS
	}
	if s.Sensitivity < 74 || s.Sensitivity > 97 {
		s.Sensitivity = old.Sensitivity
	}
	if e := validateMatchSettings(config{RequireStart: true, MinMatchSec: s.MinMatchSec, MaxMatchSec: s.MaxMatchSec}); e != nil {
		s.MinMatchSec, s.MaxMatchSec = old.MinMatchSec, old.MaxMatchSec
	}
	if s.Scenes == nil {
		s.Scenes = old.Scenes
	}
	seen := make(map[string]bool)
	list := make([]string, 0, min(128, len(s.Scenes)))
	for _, n := range s.Scenes {
		if len(list) >= 128 {
			break
		}
		if len(n) > 512 || strings.TrimSpace(n) == "" || strings.ContainsAny(n, "\x00\r\n") || seen[n] {
			continue
		}
		seen[n] = true
		list = append(list, n)
	}
	s.Scenes = list
	return s
}
func loadWinSettings() winSettings {
	defaults := defaultWinSettings()
	path, e := winSettingsPath()
	if e != nil {
		return defaults
	}
	f, e := os.Open(path)
	if e != nil {
		return defaults
	}
	defer f.Close()
	var got = defaults
	if e = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&got); e != nil {
		return defaults
	}
	return got.normalized(defaults)
}
func storeWinSettings(s winSettings) error {
	path, e := winSettingsPath()
	if e != nil {
		return e
	}
	dir := filepath.Dir(path)
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, "prefs-*.json")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	e = json.NewEncoder(f).Encode(s)
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

type winResult struct {
	id    uint64
	names []string
	addr  string
	prefs config
	arm   bool
	err   error
}

type winUI struct {
	settings                                           winSettings
	playlist                                           []string
	tooltip                                            uintptr
	focusTooltip                                       uintptr
	shownHint                                          int
	tipText                                            []*uint16
	app                                                *app
	instance, window, font                             uintptr
	scale                                              int
	h                                                  map[int]uintptr
	ref, percent, slider, state, level, recent, notice uintptr
	pending                                            bool
	pendingID                                          uint64
	cancel                                             context.CancelFunc
	completed                                          chan winResult
	last                                               state
	lastLog, lastState                                 string
	memory                                             runtime.Pinner
	playing                                            []byte
	initError                                          error
}

func winString(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return p
}
func showError(hwnd uintptr, title, msg string) {
	messageBox.Call(hwnd, uintptr(unsafe.Pointer(winString(msg))), uintptr(unsafe.Pointer(winString(title))), 0x10)
}
func (w *winUI) px(n int) int { return (n*w.scale + 48) / 96 }

func (w *winUI) make(class, name string, style, ex uintptr, id, x, y, wide, high int) uintptr {
	h, _, e := createWindow.Call(ex, uintptr(unsafe.Pointer(winString(class))), uintptr(unsafe.Pointer(winString(name))),
		wsChild|wsVisible|style, uintptr(w.px(x)), uintptr(w.px(y)), uintptr(w.px(wide)), uintptr(w.px(high)),
		w.window, uintptr(id), w.instance, 0)
	if h == 0 {
		if w.initError == nil {
			w.initError = fmt.Errorf("Could not create %s control: %w", class, e)
		}
		return 0
	}
	if w.font != 0 {
		send.Call(h, wmSetFont, w.font, 1)
	}
	if id != 0 {
		w.h[id] = h
	}
	return h
}
func (w *winUI) makeLabel(s string, x, y, width int) uintptr {
	return w.make("STATIC", s, 0x80, 0, 0, x, y, width, 20) // no mnemonic prefix
}
func (w *winUI) makeEdit(s string, id, x, y, width int, password bool) {
	var style uintptr = wsTab | 0x0080 // auto horizontal scroll
	if password {
		style |= 0x0020
	}
	h := w.make("EDIT", s, style, wsExClientEdge, id, x, y, width, 25)
	send.Call(h, 0x00c5, 256, 0) // EM_LIMITTEXT
}
func (w *winUI) makeCombo(s string, id, x, y, width int) {
	h := w.make("COMBOBOX", "", wsTab|wsVScroll|0x0002|0x0040, 0, id, x, y, width, 160)
	send.Call(h, 0x0141, 256, 0) // CB_LIMITTEXT
	winText(h, s)
}
func (w *winUI) button(s string, id, x, y, width int) {
	w.make("BUTTON", s, wsTab, 0, id, x, y, width, 28)
}
func winText(h uintptr, s string) {
	setWindowText.Call(h, uintptr(unsafe.Pointer(winString(s))))
}
func readWinText(h uintptr) string {
	n, _, _ := windowTextLength.Call(h)
	if n > 11000 {
		n = 11000
	}
	buf := make([]uint16, int(n)+1)
	windowText.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func (w *winUI) hint(id, x, y int) {
	// Real focusable button with an accessible name, but no drawn frame.
	h := w.make("BUTTON", "Information: "+help[id].title, wsTab|0x400b, 0, id, x, y, 24, 23)
	if h == 0 {
		return
	}
	body := winString(help[id].body)
	w.tipText = append(w.tipText, body)
	ti := winTool{size: uint32(unsafe.Sizeof(winTool{})), flags: 0x11, window: w.window, id: h, text: body}
	if w.tooltip != 0 {
		send.Call(w.tooltip, 0x0432, 0, uintptr(unsafe.Pointer(&ti))) // TTM_ADDTOOLW, hover
	}
	if w.focusTooltip != 0 {
		ti.flags = 0xa1 // TTF_IDISHWND | TTF_TRACK | TTF_ABSOLUTE
		send.Call(w.focusTooltip, 0x0432, 0, uintptr(unsafe.Pointer(&ti)))
	}
}

func (w *winUI) newTooltip() uintptr {
	h, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(winString("tooltips_class32"))), 0,
		0x80000000|0x03, 0, 0, 0, 0, w.window, 0, w.instance, 0)
	if h != 0 {
		positionWindow.Call(h, ^uintptr(0), 0, 0, 0, 0, 0x0013)
		send.Call(h, 0x0418, 0, uintptr(w.px(340))) // TTM_SETMAXTIPWIDTH
	}
	return h
}
func (w *winUI) hideHint() {
	if w.shownHint == 0 || w.focusTooltip == 0 {
		return
	}
	ti := winTool{size: uint32(unsafe.Sizeof(winTool{})), window: w.window, id: w.h[w.shownHint]}
	send.Call(w.focusTooltip, 0x0411, 0, uintptr(unsafe.Pointer(&ti))) // TTM_TRACKACTIVATE
	w.shownHint = 0
	send.Call(w.tooltip, 0x0401, 1, 0) // TTM_ACTIVATE
}
func (w *winUI) focusHint(id int) {
	if w.shownHint == id || w.focusTooltip == 0 {
		return
	}
	w.hideHint()
	h := w.h[id]
	r := winRect{}
	if ok, _, _ := windowRect.Call(h, uintptr(unsafe.Pointer(&r))); ok == 0 {
		return
	}
	send.Call(w.tooltip, 0x041c, 0, 0) // TTM_POP
	send.Call(w.tooltip, 0x0401, 0, 0)
	at := uintptr(uint16(r.left)) | uintptr(uint16(r.bottom+2))<<16
	send.Call(w.focusTooltip, 0x0412, 0, at) // TTM_TRACKPOSITION
	ti := winTool{size: uint32(unsafe.Sizeof(winTool{})), window: w.window, id: h}
	send.Call(w.focusTooltip, 0x0411, 1, uintptr(unsafe.Pointer(&ti)))
	w.shownHint = id
}

func winMixColor(toward, from uintptr, percent uintptr) uintptr {
	var color uintptr
	for i := 0; i < 24; i += 8 {
		a, b := toward>>i&255, from>>i&255
		color |= ((a*percent + b*(100-percent) + 50) / 100) << i
	}
	return color
}
func (w *winUI) paintHint(d *winDrawItem) {
	const btnFace, btnText, highlight = 15, 18, 13
	bg, _, _ := getColorBrush.Call(btnFace)
	fillRect.Call(d.dc, uintptr(unsafe.Pointer(&d.rect)), bg)
	front, _, _ := getColor.Call(btnText)
	back, _, _ := getColor.Call(btnFace)
	color := winMixColor(front, back, 58)
	focus := d.state&0x0010 != 0
	if focus || d.state&0x0001 != 0 {
		color = front
	}
	pen, _, _ := createPen.Call(0, uintptr(max(1, w.px(1))), color)
	if pen == 0 {
		return
	}
	defer deleteGDI.Call(pen)
	oldPen, _, _ := selectGDI.Call(d.dc, pen)
	defer selectGDI.Call(d.dc, oldPen)
	brush, _, _ := getStockGDI.Call(5) // NULL_BRUSH
	oldBrush, _, _ := selectGDI.Call(d.dc, brush)
	defer selectGDI.Call(d.dc, oldBrush)
	size := int32(w.px(13))
	l := (d.rect.left + d.rect.right - size) / 2
	t := (d.rect.top + d.rect.bottom - size) / 2
	drawEllipse.Call(d.dc, uintptr(l), uintptr(t), uintptr(l+size), uintptr(t+size))
	m := l + (size-1)/2
	dot := t + int32(w.px(3))
	moveGDI.Call(d.dc, uintptr(m), uintptr(dot), 0)
	lineGDI.Call(d.dc, uintptr(m), uintptr(dot+int32(max(1, w.px(1)))))
	moveGDI.Call(d.dc, uintptr(m), uintptr(t+int32(w.px(5))), 0)
	lineGDI.Call(d.dc, uintptr(m), uintptr(t+int32(w.px(10))))
	if focus {
		fc, _, _ := getColor.Call(highlight)
		halo, _, _ := createPen.Call(0, uintptr(max(1, w.px(1))), fc)
		if halo != 0 {
			selectGDI.Call(d.dc, halo)
			rim := int32(w.px(2))
			drawEllipse.Call(d.dc, uintptr(l-rim), uintptr(t-rim), uintptr(l+size+rim), uintptr(t+size+rim))
			selectGDI.Call(d.dc, pen)
			deleteGDI.Call(halo)
		}
	}
}

func (w *winUI) construct() {
	w.font, _, _ = makeFont.Call(uintptr(-int64(w.px(15))), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(winString("Segoe UI"))))
	w.tooltip = w.newTooltip()
	w.focusTooltip = w.newTooltip()
	w.makeLabel("OBS WebSocket address", 16, 11, 169)
	w.hint(idHelpOBS, 181, 8)
	w.makeLabel("OBS WebSocket password", 284, 11, 254)
	w.makeEdit(w.settings.Address, idAddress, 16, 31, 254, false)
	w.makeEdit("", idPassword, 284, 31, 254, true)
	w.button("Load OBS Scenes", idScenes, 16, 64, 147)

	w.makeLabel("Scene order", 16, 106, 88)
	w.hint(idHelpPlaylist, 94, 103)
	w.make("LISTBOX", "", wsTab|wsVScroll|0x0101, wsExClientEdge, idPlaylist, 16, 128, 370, 81)
	w.playlist = append([]string{}, w.settings.Scenes...)
	w.showPlaylist(0)
	w.button("Move Up", idUp, 400, 128, 138)
	w.button("Move Down", idDown, 400, 155, 138)
	w.button("Remove", idRemove, 400, 182, 138)
	w.makeCombo("", idCandidate, 16, 217, 370)
	send.Call(w.h[idCandidate], 0x1703, 0, uintptr(unsafe.Pointer(winString("Choose or type a scene"))))
	w.button("Add Scene", idAdd, 400, 214, 138)

	w.makeLabel("Reference:", 16, 252, 70)
	w.ref = w.makeLabel("", 105, 252, 435)
	w.hint(idHelpReference, 79, 247)
	w.button("Load WAV…", idLoad, 16, 273, 104)
	w.button("Play WAV", idPlay, 126, 273, 89)
	w.button("Reset WAV", idReset, 221, 273, 96)

	w.makeLabel("Match threshold", 16, 317, 111)
	w.slider = w.make("msctls_trackbar32", "", wsTab|0x0010, 0, idThreshold, 137, 312, 106, 27)
	send.Call(w.slider, 0x0406, 1, 74)
	send.Call(w.slider, 0x0407, 1, 97)
	send.Call(w.slider, 0x0405, 1, uintptr(w.settings.Sensitivity))
	w.percent = w.makeLabel(fmt.Sprintf("%d%%", w.settings.Sensitivity), 250, 317, 45)
	w.makeLabel("Switch delay (s)", 306, 317, 109)
	w.makeEdit(strconv.FormatFloat(float64(w.settings.DelayMS)/1000, 'f', -1, 64), idDelay, 423, 313, 76, false)
	w.hint(idHelpDetect, 111, 313)

	w.make("BUTTON", "Require start buzzer", wsTab|0x0003, 0, idRequireStart, 16, 345, 152, 24)
	if w.settings.RequireStart {
		send.Call(w.h[idRequireStart], 0x00f1, 1, 0)
	} // BM_SETCHECK
	w.makeLabel("Min (s)", 212, 348, 47)
	w.makeEdit(strconv.Itoa(w.settings.MinMatchSec), idMin, 261, 344, 54, false)
	w.makeLabel("Max (s)", 340, 348, 49)
	w.makeEdit(strconv.Itoa(w.settings.MaxMatchSec), idMax, 392, 344, 54, false)
	w.hint(idHelpStart, 169, 345)

	w.button("Listen Only", idListen, 16, 382, 104)
	w.button("Arm Switching", idArm, 126, 382, 115)
	w.button("Stop", idStop, 247, 382, 76)
	w.hint(idHelpRun, 330, 384)

	w.state = w.makeLabel("", 16, 425, 524)
	w.makeLabel("Playback level", 16, 449, 108)
	w.level = w.make("msctls_progress32", "", 0x0001, 0, 0, 123, 451, 415, 13)
	w.notice = w.makeLabel("", 16, 475, 524)
	w.recent = w.make("EDIT", "", wsVScroll|0x0004|0x0040|0x0800, wsExClientEdge, 0, 16, 496, 522, 57)
	w.refresh()
}
func (w *winUI) selected() int {
	n, _, _ := send.Call(w.h[idPlaylist], 0x0188, 0, 0) // LB_GETCURSEL
	i := int(int32(n))
	if i < 0 || i >= len(w.playlist) {
		return -1
	}
	return i
}
func (w *winUI) showPlaylist(index int) {
	h := w.h[idPlaylist]
	send.Call(h, 0x0184, 0, 0)
	for _, name := range w.playlist {
		send.Call(h, 0x0180, 0, uintptr(unsafe.Pointer(winString(name))))
	}
	if index >= 0 && index < len(w.playlist) {
		send.Call(h, 0x0186, uintptr(index), 0)
	}
}
func (w *winUI) requireStart() bool {
	s, _, _ := send.Call(w.h[idRequireStart], 0x00f0, 0, 0)
	return s == 1
}
func (w *winUI) prefs() (config, error) {
	val, _, _ := send.Call(w.slider, 0x0400, 0, 0)
	c := config{
		Address: readWinText(w.h[idAddress]), Password: readWinText(w.h[idPassword]),
		Scenes:      append([]string{}, w.playlist...),
		Sensitivity: int(val), DelayMS: 2000, RequireStart: w.requireStart(), MinMatchSec: 45, MaxMatchSec: 70,
	}
	input := strings.TrimSpace(readWinText(w.h[idDelay]))
	if input != "" {
		v, e := strconv.ParseFloat(input, 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 30 {
			return c, errors.New("Switch delay must be 0 to 30 seconds.")
		}
		c.DelayMS = int(math.Round(v * 1000))
	}
	if c.RequireStart {
		min, e := strconv.Atoi(strings.TrimSpace(readWinText(w.h[idMin])))
		max, f := strconv.Atoi(strings.TrimSpace(readWinText(w.h[idMax])))
		if e != nil || f != nil {
			return c, errors.New("Enter whole seconds for the minimum and maximum match time.")
		}
		c.MinMatchSec, c.MaxMatchSec = min, max
	}
	if err := validateMatchSettings(c); err != nil {
		return c, err
	}
	return c, nil
}
func (w *winUI) refresh() {
	s := w.app.snapshot()
	stateLine := s.Mode + "  |  " + s.Status
	if w.lastState != stateLine {
		winText(w.state, stateLine)
		w.lastState = stateLine
	}
	if s.Reference != w.last.Reference {
		winText(w.ref, s.Reference)
	}
	if s.Level != w.last.Level {
		send.Call(w.level, 0x0402, uintptr(s.Level*100), 0) // PBM_SETPOS
	}
	all := strings.Join(s.Log, "\r\n")
	if all != w.lastLog {
		winText(w.recent, all)
		send.Call(w.recent, 0x00b1, ^uintptr(0), ^uintptr(0)) // EM_SETSEL (-1,-1)
		send.Call(w.recent, 0x00b7, 0, 0)                     // EM_SCROLLCARET
		w.lastLog = all
	}
	w.last = s
	w.refreshEnabled()
}
func (w *winUI) refreshEnabled() {
	ready := uintptr(1)
	if w.pending {
		ready = 0
	}
	edit := ready
	if w.last.Mode == "Armed" {
		edit = 0
	}
	if w.last.Mode == "Armed" || w.last.Mode == "Listen only" {
		// Restart listening after changing any detection setting.
		enableWindow.Call(w.slider, 0)
	} else {
		enableWindow.Call(w.slider, ready)
	}
	for _, id := range []int{idLoad, idReset, idListen, idArm} {
		enableWindow.Call(w.h[id], ready)
	}
	for _, id := range []int{idScenes, idAddress, idPassword} {
		enableWindow.Call(w.h[id], edit)
	}
	for _, id := range []int{idCandidate, idPlaylist, idAdd, idDelay, idRequireStart} {
		enableWindow.Call(w.h[id], edit)
	}
	start := edit
	if !w.requireStart() {
		start = 0
	}
	enableWindow.Call(w.h[idMin], start)
	enableWindow.Call(w.h[idMax], start)
	ix := w.selected()
	for _, item := range []struct {
		id int
		ok bool
	}{{idRemove, ix >= 0}, {idUp, ix > 0}, {idDown, ix >= 0 && ix < len(w.playlist)-1}} {
		v := edit
		if !item.ok {
			v = 0
		}
		enableWindow.Call(w.h[item.id], v)
	}
	if !w.last.CanPlay || w.last.Mode != "Off" {
		ready = 0
	}
	enableWindow.Call(w.h[idPlay], ready)
}
func (w *winUI) selectScenes(h uintptr, names []string) {
	prev := readWinText(h)
	send.Call(h, 0x014b, 0, 0) // CB_RESETCONTENT
	for _, name := range names {
		send.Call(h, 0x0143, 0, uintptr(unsafe.Pointer(winString(name)))) // CB_ADDSTRING
	}
	winText(h, prev)
}

func (w *winUI) cancelRequest() {
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	w.pendingID++
	w.pending = false
}
func (w *winUI) verify(prefs config, arm bool) {
	addr, err := wsURL(prefs.Address)
	if err != nil {
		w.err(err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.pendingID++
	id := w.pendingID
	w.pending = true
	winText(w.notice, "Connecting to OBS…")
	w.refreshEnabled()
	go func() {
		defer cancel()
		names, e := w.app.checkOBS(ctx, addr, prefs.Password)
		if e == nil && arm {
			e = validateScenes(names, prefs)
		}
		select {
		case w.completed <- winResult{id: id, names: names, addr: addr, prefs: prefs, arm: arm, err: e}:
		case <-ctx.Done():
		}
	}()
}
func (w *winUI) finish(r winResult) {
	if r.id != w.pendingID {
		return
	}
	w.cancelRequest()
	if r.names != nil {
		w.selectScenes(w.h[idCandidate], r.names)
	}
	if r.err != nil {
		w.err(r.err)
	} else if r.arm {
		w.stopPlayback()
		w.app.launch(r.prefs, r.addr, true)
		winText(w.notice, "Switching armed.")
	} else {
		winText(w.notice, fmt.Sprintf("OBS connected. %d scenes loaded.", len(r.names)))
	}
	w.refresh()
}
func (w *winUI) err(err error) {
	winText(w.notice, "")
	showError(w.window, "FieldCam Buzzer Switcher", err.Error())
}
func (w *winUI) stopPlayback() {
	if len(w.playing) > 0 {
		playSound.Call(0, 0, 0)
		w.memory.Unpin()
		w.playing = nil
	}
}
func (w *winUI) play() {
	s := w.app.snapshot()
	if s.Mode != "Off" || !s.CanPlay {
		return
	}
	w.stopPlayback()
	w.app.mu.Lock()
	w.playing = w.app.raw
	w.app.mu.Unlock()
	if len(w.playing) == 0 {
		return
	}
	w.memory.Pin(&w.playing[0])
	ok, _, _ := playSound.Call(uintptr(unsafe.Pointer(&w.playing[0])), 0, 0x0004|0x0001|0x0002) // MEMORY, ASYNC, NODEFAULT
	if ok == 0 {
		w.stopPlayback()
		w.err(errors.New("Could not play the reference WAV."))
	} else {
		winText(w.notice, "Playing reference WAV.")
	}
}
func (w *winUI) chooseWAV() {
	filter := utf16.Encode([]rune("WAV audio (*.wav)\x00*.wav\x00\x00"))
	fullPath := make([]uint16, 32768)
	d := winFile{owner: w.window, filter: &filter[0], filterIndex: 1, file: &fullPath[0], maxFile: uint32(len(fullPath)),
		title: winString("Select Tournament Manager end WAV"), flags: 0x00080000 | 0x00001000 | 0x00000800 | 0x00000008, defaultExtension: winString("wav")}
	d.size = uint32(unsafe.Sizeof(d))
	ok, _, _ := pickFile.Call(uintptr(unsafe.Pointer(&d)))
	if ok == 0 {
		code, _, _ := extendedDialogError.Call()
		if code != 0 {
			w.err(fmt.Errorf("Could not open the file picker (0x%04x).", code))
		}
		return
	}
	path := syscall.UTF16ToString(fullPath)
	raw, err := readReference(path)
	if err == nil {
		w.stopPlayback()
		err = w.app.replace(raw, "Loaded: "+filepath.Base(path))
	}
	if err != nil {
		w.err(err)
	} else {
		winText(w.notice, "WAV loaded.")
	}
}
func readReference(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxSize = 6 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSize {
		return nil, errors.New("WAV must be under 6 MB.")
	}
	return data, nil
}

func (w *winUI) save() {
	s := w.settings
	s.Address = readWinText(w.h[idAddress])
	s.Scenes = append([]string{}, w.playlist...)
	threshold, _, _ := send.Call(w.slider, 0x0400, 0, 0)
	s.Sensitivity = int(threshold)
	s.RequireStart = w.requireStart()
	raw := strings.TrimSpace(readWinText(w.h[idDelay]))
	if raw == "" {
		s.DelayMS = 2000
	} else if n, e := strconv.ParseFloat(raw, 64); e == nil && !math.IsNaN(n) && n >= 0 && n <= 30 {
		s.DelayMS = int(math.Round(n * 1000))
	}
	if min, e := strconv.Atoi(strings.TrimSpace(readWinText(w.h[idMin]))); e == nil {
		if max, e := strconv.Atoi(strings.TrimSpace(readWinText(w.h[idMax]))); e == nil {
			s.MinMatchSec, s.MaxMatchSec = min, max
		}
	}
	if e := storeWinSettings(s.normalized(w.settings)); e != nil {
		showError(w.window, "FieldCam Buzzer Switcher", "Could not save your settings: "+e.Error())
	}
}

func (w *winUI) command(id int) {
	if _, ok := help[id]; ok {
		w.focusHint(id)
		return
	}
	if w.pending && id != idStop {
		return
	}
	switch id {
	case idScenes:
		w.verify(config{Address: readWinText(w.h[idAddress]), Password: readWinText(w.h[idPassword])}, false)
	case idArm:
		prefs, err := w.prefs()
		if err != nil {
			w.err(err)
			break
		}
		w.verify(prefs, true)
	case idAdd:
		name := readWinText(w.h[idCandidate])
		if strings.TrimSpace(name) == "" {
			focusWindow.Call(w.h[idCandidate])
			break
		}
		for i, n := range w.playlist {
			if n == name {
				w.showPlaylist(i)
				return
			}
		}
		w.playlist = append(w.playlist, name)
		winText(w.h[idCandidate], "")
		w.showPlaylist(len(w.playlist) - 1)
	case idRemove:
		i := w.selected()
		if i < 0 {
			break
		}
		w.playlist = append(w.playlist[:i], w.playlist[i+1:]...)
		w.showPlaylist(min(i, len(w.playlist)-1))
	case idUp, idDown:
		i := w.selected()
		if i < 0 {
			break
		}
		j := i + 1
		if id == idUp {
			j = i - 1
		}
		if j < 0 || j >= len(w.playlist) {
			break
		}
		w.playlist[i], w.playlist[j] = w.playlist[j], w.playlist[i]
		w.showPlaylist(j)
	case idLoad:
		w.chooseWAV()
	case idPlay:
		w.play()
	case idReset:
		fp, raw, err := defaultReference()
		if err != nil {
			w.err(err)
			break
		}
		w.stopPlayback()
		w.app.stop()
		w.app.mu.Lock()
		w.app.raw, w.app.ref = raw, fp
		w.app.state.Reference = fp.name
		w.app.state.CanPlay = len(raw) > 0
		w.app.mu.Unlock()
		winText(w.notice, "Default reference restored.")
	case idListen:
		prefs, err := w.prefs()
		if err != nil {
			w.err(err)
			break
		}
		w.app.launch(prefs, "", false)
		winText(w.notice, "Listen only. OBS won’t switch.")
	case idStop:
		w.cancelRequest()
		w.stopPlayback()
		w.app.stop()
		w.app.change(0, "Stopped", "Listening and pending switches stopped.")
		winText(w.notice, "")
	}
	w.refresh()
}

func winProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	w := ui
	if w != nil {
		switch msg {
		case wmDrawItem:
			if _, ok := help[int(wp)]; ok && lp != 0 {
				// LPARAM holds a native pointer for WM_DRAWITEM.
				d := *(**winDrawItem)(unsafe.Pointer(&lp))
				w.paintHint(d)
				return 1
			}
		case wmCommand:
			event, id := wp>>16&0xffff, int(wp&0xffff)
			if _, ok := help[id]; ok {
				switch event {
				case 6:
					w.focusHint(id) // BN_SETFOCUS
				case 7:
					w.hideHint() // BN_KILLFOCUS
				}
			}
			if event == 0 {
				w.command(id)
			}
			if id == idPlaylist {
				w.refreshEnabled()
			}
			if id == idCandidate && event == 2 {
				w.command(idAdd)
			}
			return 0
		case wmHScroll:
			if lp == w.slider {
				p, _, _ := send.Call(w.slider, 0x0400, 0, 0)
				winText(w.percent, fmt.Sprintf("%d%%", p))
			}
			return 0
		case wmTimer:
			for {
				select {
				case r := <-w.completed:
					w.finish(r)
				default:
					w.refresh()
					return 0
				}
			}
		case wmClose:
			w.cancelRequest()
			w.stopPlayback()
			w.app.stop()
			w.save()
			destroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			killTimer.Call(hwnd, 1)
			w.cancelRequest()
			w.stopPlayback()
			w.app.stop()
			w.app.closeOBS()
			if w.font != 0 {
				deleteGDI.Call(w.font)
			}
			postQuit.Call(0)
			return 0
		}
	}
	result, _, _ := defaultProc.Call(hwnd, uintptr(msg), wp, lp)
	return result
}

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ref, raw, err := defaultReference()
	if err != nil {
		showError(0, "FieldCam Buzzer Switcher", err.Error())
		return
	}
	// Use real OS DPI and native themed controls without browser scaling.
	if setDPIAware.Find() == nil {
		setDPIAware.Call()
	}
	dpi := uintptr(96)
	if getDPIForSystem.Find() == nil {
		if n, _, _ := getDPIForSystem.Call(); n != 0 {
			dpi = n
		}
	}
	init := struct{ size, flags uint32 }{8, 0x24} // progress and trackbar
	initControls.Call(uintptr(unsafe.Pointer(&init)))
	inst, _, _ := getModule.Call(0)
	icon, _, _ := loadIcon.Call(0, 32512)
	cursor, _, _ := loadCursor.Call(0, 32512)
	back, _, _ := getColorBrush.Call(15) // system window/control background
	className := winString("FieldCamBuzzerSwitcherNative")
	title := winString("FieldCam Buzzer Switcher")
	cl := winClass{proc: syscall.NewCallback(winProc), instance: inst, icon: icon, smallIcon: icon, cursor: cursor, background: back, name: className}
	cl.size = uint32(unsafe.Sizeof(cl))
	if atom, _, e := registerClass.Call(uintptr(unsafe.Pointer(&cl))); atom == 0 {
		showError(0, "FieldCam Buzzer Switcher", fmt.Sprintf("Could not register window: %v", e))
		return
	}
	w := &winUI{
		settings: loadWinSettings(),
		app:      &app{ref: ref, raw: raw, state: state{Mode: "Off", Status: "Stopped", Reference: ref.name, CanPlay: len(raw) > 0}},
		instance: inst, scale: int(dpi), h: make(map[int]uintptr), completed: make(chan winResult, 8),
	}
	ui = w
	rect := winRect{right: int32(w.px(554)), bottom: int32(w.px(566))}
	adjustWindowRect.Call(uintptr(unsafe.Pointer(&rect)), windowStyle, 0, wsExControlParent)
	work := winRect{}
	if ok, _, _ := systemParameters.Call(0x0030, 0, uintptr(unsafe.Pointer(&work)), 0); ok == 0 {
		x, _, _ := getSysMetrics.Call(0)
		y, _, _ := getSysMetrics.Call(1)
		work.right, work.bottom = int32(x), int32(y)
	}
	ww, wh := rect.right-rect.left, rect.bottom-rect.top
	x := max(work.left, work.left+(work.right-work.left-ww)/2)
	y := max(work.top, work.top+(work.bottom-work.top-wh)/2)
	win, _, e := createWindow.Call(wsExControlParent, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		windowStyle, uintptr(x), uintptr(y), uintptr(ww), uintptr(wh), 0, 0, inst, 0)
	if win == 0 {
		showError(0, "FieldCam Buzzer Switcher", fmt.Sprintf("Could not create window: %v", e))
		return
	}
	w.window = win
	w.construct()
	if w.initError != nil {
		showError(win, "FieldCam Buzzer Switcher", w.initError.Error())
		destroyWindow.Call(win)
		return
	}
	showWindow.Call(win, 1)
	focusWindow.Call(w.h[idAddress])
	updateWindow.Call(win)
	setTimer.Call(win, 1, 125, 0)
	var msg winMessage
	for {
		rc, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(rc) <= 0 {
			break
		}
		if msg.event == 0x0100 && msg.wparam == 27 && w.shownHint != 0 { // keyboard Escape
			w.hideHint()
			continue
		}
		if ok, _, _ := dialogMessage.Call(win, uintptr(unsafe.Pointer(&msg))); ok != 0 {
			continue
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

// Check the Win64 interop layouts at compile time.
var _ [80 - int(unsafe.Sizeof(winClass{}))]byte
var _ [int(unsafe.Sizeof(winClass{})) - 80]byte
var _ [152 - int(unsafe.Sizeof(winFile{}))]byte
var _ [int(unsafe.Sizeof(winFile{})) - 152]byte
var _ [48 - int(unsafe.Sizeof(winMessage{}))]byte
var _ [int(unsafe.Sizeof(winMessage{})) - 48]byte

var _ [64 - int(unsafe.Sizeof(winDrawItem{}))]byte
var _ [int(unsafe.Sizeof(winDrawItem{})) - 64]byte
