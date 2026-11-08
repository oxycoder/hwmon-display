// Control panel: a small local web UI + JSON API (default http://127.0.0.1:8086)
// to look at the display device and decide what the screen shows.
//
//   - screen modes: "monitor" (the OS hardware page, default), "text" (a free
//     text page the user writes: SRC=TEXT frames) or "raw" (any protocol line,
//     repeated every tick so the display never times out)
//   - one-shot sends of arbitrary protocol lines (e.g. SRC=CMD|STANDBY=1)
//   - device information: the ESP32 answers "SRC=CMD|INFO=1" with a
//     "SRC=DEV|FW=..|CHIP=..|HEAP_KB=..|..." line, which a reader goroutine on
//     the serial port collects; the firmware also sends one on USB connect
//   - backlight brightness (SRC=CMD|BL=0..100), re-applied after a re-plug
//
// The server binds to loopback only. State-changing requests must carry a JSON
// body with Content-Type application/json and no foreign Origin, which keeps
// other web pages in the browser from driving the display.
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed panel.html
var panelHTML []byte

const daemonVersion = "1.2"

// maxFrame is the longest protocol line the display accepts (DC_MAX_LINE - 1,
// without the newline); maxValue the longest field value (dc_field.val).
const (
	maxFrame     = 511
	maxValue     = 63
	maxTextLines = 12
)

// ---------- what the screen shows ----------------------------------------------

type textPage struct {
	Title string   `json:"title"`
	Lines []string `json:"lines"`
	Size  int      `json:"size"`
	Color string   `json:"color"`
	Align string   `json:"align"`
}

var textColors = map[string]bool{"": true, "text": true, "accent": true, "ok": true, "warn": true, "hot": true, "dim": true}

// textFrame encodes a text page as one SRC=TEXT line (no newline), within the
// display's field and line limits. Blank lines are kept as gaps; trailing
// blank lines are dropped.
func textFrame(t textPage) (string, error) {
	if t.Size == 0 {
		t.Size = 2
	}
	if t.Size < 1 || t.Size > 4 {
		return "", fmt.Errorf("size must be 1..4")
	}
	if !textColors[t.Color] {
		return "", fmt.Errorf("unknown colour %q", t.Color)
	}
	if t.Align != "" && t.Align != "left" && t.Align != "center" {
		return "", fmt.Errorf("align must be left or center")
	}
	lines := append([]string(nil), t.Lines...)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxTextLines {
		return "", fmt.Errorf("at most %d lines", maxTextLines)
	}
	parts := []string{"SRC=TEXT"}
	if title := cut(clean(t.Title), maxValue); title != "" {
		parts = append(parts, "TITLE="+title)
	}
	for i, l := range lines {
		parts = append(parts, fmt.Sprintf("L%d=%s", i+1, cut(clean(l), maxValue)))
	}
	parts = append(parts, fmt.Sprintf("SIZE=%d", t.Size))
	if t.Color != "" {
		parts = append(parts, "COLOR="+t.Color)
	}
	if t.Align == "center" {
		parts = append(parts, "ALIGN=center")
	}
	line := strings.Join(parts, "|")
	if len(line) > maxFrame {
		return "", fmt.Errorf("frame is %d bytes, the display takes %d", len(line), maxFrame)
	}
	return line, nil
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// rawFrame validates a user-typed protocol line: one line, starts with SRC=,
// printable ASCII, within the display's limit. Returns it without a newline.
func rawFrame(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "SRC=") {
		return "", fmt.Errorf("a frame starts with SRC=")
	}
	if strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("one line only")
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("printable ASCII only (the display's font has nothing else)")
		}
	}
	if len(s) > maxFrame {
		return "", fmt.Errorf("frame is %d bytes, the display takes %d", len(s), maxFrame)
	}
	return s, nil
}

// screen is the current content choice; the main loop asks it for every frame.
type screen struct {
	mu   sync.Mutex
	mode string // monitor | text | raw
	text textPage
	raw  string // validated SRC=... line for raw mode
}

var scr = &screen{mode: "monitor", text: textPage{Size: 2}}

// set switches the mode; text/raw are validated first so a bad request leaves
// the screen as it was.
func (s *screen) set(mode string, t textPage, raw string) error {
	switch mode {
	case "monitor":
	case "text":
		if _, err := textFrame(t); err != nil {
			return err
		}
	case "raw":
		r, err := rawFrame(raw)
		if err != nil {
			return err
		}
		raw = r
	default:
		return fmt.Errorf("mode must be monitor, text or raw")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
	if mode == "text" {
		s.text = t
	}
	if mode == "raw" {
		s.raw = raw
	}
	return nil
}

// line returns the frame to send now (no newline). monitor builds the OS page
// and is only called in monitor mode, so sensors are left alone otherwise.
func (s *screen) line(monitor func() string) string {
	s.mu.Lock()
	mode, t, raw := s.mode, s.text, s.raw
	s.mu.Unlock()
	switch mode {
	case "text":
		if l, err := textFrame(t); err == nil {
			return l
		}
	case "raw":
		if raw != "" {
			return raw
		}
	}
	return monitor()
}

func (s *screen) snapshot() (mode string, t textPage, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode, s.text, s.raw
}

// ---------- traffic bookkeeping ------------------------------------------------

var (
	framesSent atomic.Uint64
	lastSent   atomic.Value // string: the last frame written (no newline)
	startedAt  = time.Now()
	backlight  atomic.Int32 // last brightness asked for, -1 = never
)

func init() { backlight.Store(-1) }

func noteSent(line string) {
	framesSent.Add(1)
	lastSent.Store(strings.TrimRight(line, "\n"))
}

// sendNow writes one protocol line to the display right away, outside the main
// loop's tick (mode changes, one-shot sends, commands). The main loop sees and
// handles any port failure on its next write.
func sendNow(line string) error {
	f := curPort.Load()
	if f == nil {
		return errors.New("display not connected")
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	noteSent(line)
	return nil
}

// ---------- device information from the display -------------------------------

type deviceInfo struct {
	mu     sync.Mutex
	fields map[string]string
	order  []string // keys in the order the firmware sent them
	at     time.Time
	seq    uint64
}

var device deviceInfo

// readLoop collects lines the display sends back on f (only "SRC=DEV|..."
// is meaningful; anything else is ignored). The port is opened with a short
// read timeout, which os.File reports as io.EOF, so EOF only ends the loop
// once f is no longer the current port.
func readLoop(f *os.File) {
	buf := make([]byte, 512)
	var acc []byte
	for {
		n, err := f.Read(buf)
		for _, b := range buf[:n] {
			if b == '\n' {
				handleRxLine(strings.TrimRight(string(acc), "\r"))
				acc = acc[:0]
			} else if len(acc) < 1024 {
				acc = append(acc, b)
			}
		}
		if err != nil {
			if err == io.EOF && curPort.Load() == f {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		if n == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func handleRxLine(line string) {
	if !strings.HasPrefix(line, "SRC=DEV|") {
		return
	}
	fields, order := parseFrame(line)
	now := time.Now()
	device.mu.Lock()
	device.fields, device.order, device.at = fields, order, now
	device.seq++
	device.mu.Unlock()
	history.add(sampleOf(fields, now))
}

// ---------- device history (for the panel's charts) ----------------------------

// devSample is what the charts plot from each SRC=DEV line. Missing numbers
// are -1 (the firmware may lack the field, e.g. TEMP on a chip without a sensor).
type devSample struct {
	T         int64 `json:"t"`           // unix ms
	Temp      int   `json:"temp"`        // chip temperature, °C
	HeapKB    int   `json:"heap_kb"`     // free heap
	HeapMinKB int   `json:"heap_min_kb"` // lowest free heap since boot
	RxBytes   int64 `json:"rx_bytes"`    // counters since boot; the panel derives rates
	Frames    int64 `json:"frames"`
	UpS       int64 `json:"up_s"`
	CPUMHz    int   `json:"cpu_mhz"`
	Standby   bool  `json:"standby"`
}

func sampleOf(f map[string]string, at time.Time) devSample {
	n := func(k string) int64 {
		v, ok := f[k]
		if !ok {
			return -1
		}
		var x int64
		if _, err := fmt.Sscan(v, &x); err != nil {
			return -1
		}
		return x
	}
	return devSample{
		T: at.UnixMilli(), Temp: int(n("TEMP")), HeapKB: int(n("HEAP_KB")), HeapMinKB: int(n("HEAP_MIN_KB")),
		RxBytes: n("RX_BYTES"), Frames: n("FRAMES"), UpS: n("UP_S"), CPUMHz: int(n("CPU_MHZ")), Standby: f["STANDBY"] == "1",
	}
}

// devicePollEvery is how often hwmond asks the display for a SRC=DEV line while
// one is connected; historyMax samples are kept (2 h at 10 s).
const (
	devicePollEvery = 10 * time.Second
	historyMax      = 720
)

type devHistory struct {
	mu sync.Mutex
	s  []devSample
}

var history devHistory

func (h *devHistory) add(x devSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.s = append(h.s, x)
	if len(h.s) > historyMax {
		h.s = h.s[len(h.s)-historyMax:]
	}
}

func (h *devHistory) snapshot() []devSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]devSample{}, h.s...)
}

// devicePoller requests device info every devicePollEvery while a real port is
// open, so the charts have a series even when nobody presses Refresh.
func devicePoller() {
	for {
		if f := curPort.Load(); f != nil && f != os.Stdout {
			sendNow("SRC=CMD|INFO=1")
		}
		time.Sleep(devicePollEvery)
	}
}

// parseFrame splits "K=V|K=V" into a map plus key order.
func parseFrame(line string) (map[string]string, []string) {
	m := map[string]string{}
	var order []string
	for _, p := range strings.Split(line, "|") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			continue
		}
		if _, dup := m[k]; !dup {
			order = append(order, k)
		}
		m[k] = v
	}
	return m, order
}

// requestDeviceInfo asks the display for a fresh SRC=DEV line and waits up to
// timeout for it. Old firmware never answers: the error says so and the last
// known info (if any) stays available through deviceSnapshot.
func requestDeviceInfo(timeout time.Duration) error {
	device.mu.Lock()
	seq := device.seq
	device.mu.Unlock()
	if err := sendNow("SRC=CMD|INFO=1"); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		device.mu.Lock()
		got := device.seq > seq
		device.mu.Unlock()
		if got {
			return nil
		}
	}
	return errors.New("no answer from the display (firmware without device info?)")
}

type deviceJSON struct {
	Fields map[string]string `json:"fields"`
	Order  []string          `json:"order"`
	AgeS   int               `json:"age_s"` // -1 = never received
}

func deviceSnapshot() deviceJSON {
	device.mu.Lock()
	defer device.mu.Unlock()
	d := deviceJSON{Fields: device.fields, Order: device.order, AgeS: -1}
	if !device.at.IsZero() {
		d.AgeS = int(time.Since(device.at).Seconds())
	}
	return d
}

// setBacklight sends SRC=CMD|BL=n and remembers n so a re-plugged display gets
// it again (the firmware forgets it on reset).
func setBacklight(pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("brightness must be 0..100")
	}
	backlight.Store(int32(pct))
	return sendNow(fmt.Sprintf("SRC=CMD|BL=%d", pct))
}

// onConnect runs after the main loop opened a port: start collecting replies
// and restore the brightness the user chose.
func onConnect(f *os.File) {
	if f == os.Stdout {
		return
	}
	go readLoop(f)
	if bl := backlight.Load(); bl >= 0 {
		f.WriteString(fmt.Sprintf("SRC=CMD|BL=%d\n", bl))
	}
}

// ---------- HTTP ----------------------------------------------------------------

var panelAddr atomic.Value // string: "host:port" the panel listens on, "" = off

func panelURL() string {
	a, _ := panelAddr.Load().(string)
	if a == "" {
		return ""
	}
	return "http://" + a + "/"
}

// startPanel serves the UI + API on addr (loopback only); "" disables it.
func startPanel(addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-web %q: %v", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("-web %q: the control panel only listens on loopback (127.0.0.1 / localhost)", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("-web %q: %v", addr, err)
	}
	panelAddr.Store(ln.Addr().String())
	go http.Serve(ln, panelHandler())
	go devicePoller()
	fmt.Fprintln(os.Stderr, "control panel on", panelURL())
	return nil
}

func panelHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(panelHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, stateJSON())
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		s := history.snapshot()
		if s == nil {
			s = []devSample{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"every_s": int(devicePollEvery.Seconds()), "samples": s})
	})
	mux.HandleFunc("POST /api/screen", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode string   `json:"mode"`
			Text textPage `json:"text"`
			Raw  string   `json:"raw"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if err := scr.set(req.Mode, req.Text, req.Raw); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Mode != "monitor" { // show it now rather than on the next tick
			sendNow(scr.line(func() string { return "" }))
		}
		writeJSON(w, http.StatusOK, stateJSON())
	})
	mux.HandleFunc("POST /api/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Line string `json:"line"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		line, err := rawFrame(req.Line)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := sendNow(line); err != nil {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"sent": line})
	})
	mux.HandleFunc("POST /api/device/refresh", func(w http.ResponseWriter, r *http.Request) {
		var req struct{}
		if !readJSON(w, r, &req) {
			return
		}
		err := requestDeviceInfo(1500 * time.Millisecond)
		d := deviceSnapshot()
		if err != nil {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": err.Error(), "device": d})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"device": d})
	})
	mux.HandleFunc("POST /api/device/backlight", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Percent int `json:"percent"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if err := setBacklight(req.Percent); err != nil {
			code := http.StatusBadRequest
			if req.Percent >= 0 && req.Percent <= 100 {
				code = http.StatusServiceUnavailable
			}
			writeErr(w, code, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"percent": req.Percent})
	})
	return mux
}

type stateResp struct {
	Version    string     `json:"version"`
	Status     string     `json:"status"`
	Connected  bool       `json:"connected"`
	Port       string     `json:"port"`
	UptimeS    int        `json:"uptime_s"`
	FramesSent uint64     `json:"frames_sent"`
	LastFrame  string     `json:"last_frame"`
	Mode       string     `json:"mode"`
	Text       textPage   `json:"text"`
	Raw        string     `json:"raw"`
	Backlight  int        `json:"backlight"` // -1 = not set from here
	Autostart  bool       `json:"autostart"`
	Device     deviceJSON `json:"device"`
	OS         string     `json:"os"`
}

func stateJSON() stateResp {
	mode, t, raw := scr.snapshot()
	last, _ := lastSent.Load().(string)
	var port string
	if f := curPort.Load(); f != nil {
		port = f.Name()
	}
	return stateResp{
		Version:    daemonVersion,
		Status:     getStatus(),
		Connected:  port != "",
		Port:       port,
		UptimeS:    int(time.Since(startedAt).Seconds()),
		FramesSent: framesSent.Load(),
		LastFrame:  last,
		Mode:       mode,
		Text:       t,
		Raw:        raw,
		Backlight:  int(backlight.Load()),
		Autostart:  autostartEnabled(),
		Device:     deviceSnapshot(),
		OS:         runtime.GOOS,
	}
}

// readJSON decodes a state-changing request. It insists on a JSON content
// type and a same-origin (or absent) Origin header: a cross-site page cannot
// send either without a CORS preflight, which this server never answers.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, errors.New("send application/json"))
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
		writeErr(w, http.StatusForbidden, errors.New("cross-origin request refused"))
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad JSON: %v", err))
		return false
	}
	return true
}

func sameOrigin(origin, host string) bool {
	return origin == "http://"+host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// openPanel opens the control panel in the default browser (tray menu / click).
func openPanel() {
	url := panelURL()
	if url == "" {
		fmt.Fprintln(os.Stderr, "control panel is disabled (-web \"\")")
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "open browser:", err, "-", url)
	}
}
