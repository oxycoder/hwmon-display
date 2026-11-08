package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTextFrame(t *testing.T) {
	got, err := textFrame(textPage{Title: "Build | status", Lines: []string{"#ok:main passing", "", "  ", "release=FAILED", "", ""}, Size: 0, Color: "dim", Align: "center"})
	if err != nil {
		t.Fatal(err)
	}
	want := "SRC=TEXT|TITLE=Build   status|L1=#ok:main passing|L2=|L3=|L4=release FAILED|SIZE=2|COLOR=dim|ALIGN=center"
	if got != want {
		t.Errorf("textFrame =\n %s\nwant\n %s", got, want)
	}
	if got, _ := textFrame(textPage{Lines: []string{"x"}, Size: 3}); got != "SRC=TEXT|L1=x|SIZE=3" {
		t.Errorf("minimal = %s", got)
	}
	long := strings.Repeat("abcdefghij", 7) // 70 chars: cut to the display's 63-byte field
	if got, _ := textFrame(textPage{Lines: []string{long}}); !strings.Contains(got, "|L1="+long[:63]+"|") {
		t.Errorf("long line not cut: %s", got)
	}
	bad := []textPage{
		{Size: 5}, {Size: -1}, {Color: "purple"}, {Align: "right"},
		{Lines: make([]string, maxTextLines+1)},
	}
	for i := range bad {
		if i == 4 {
			for j := range bad[i].Lines {
				bad[i].Lines[j] = "x"
			}
		}
		if _, err := textFrame(bad[i]); err == nil {
			t.Errorf("textFrame(%+v) accepted", bad[i])
		}
	}
	// 12 lines of 63 chars blow the 511-byte frame limit.
	full := textPage{Lines: make([]string, maxTextLines)}
	for i := range full.Lines {
		full.Lines[i] = strings.Repeat("y", 63)
	}
	if _, err := textFrame(full); err == nil || !strings.Contains(err.Error(), "511") {
		t.Errorf("oversize frame: err = %v", err)
	}
}

func TestRawFrame(t *testing.T) {
	if got, err := rawFrame("  SRC=CMD|STANDBY=1 \r\n"); err != nil || got != "SRC=CMD|STANDBY=1" {
		t.Errorf("rawFrame = %q, %v", got, err)
	}
	for _, s := range []string{"", "LOAD=1", "SRC=OS\nSRC=OS", "SRC=OS|HOST=café", "SRC=" + strings.Repeat("x", 600)} {
		if _, err := rawFrame(s); err == nil {
			t.Errorf("rawFrame(%q) accepted", s)
		}
	}
}

func TestScreenModes(t *testing.T) {
	s := &screen{mode: "monitor"}
	mon := func() string { return "SRC=OS|LOAD=1" }
	if got := s.line(mon); got != "SRC=OS|LOAD=1" {
		t.Errorf("monitor: %s", got)
	}
	if err := s.set("text", textPage{Lines: []string{"hi"}}, ""); err != nil {
		t.Fatal(err)
	}
	if got := s.line(mon); got != "SRC=TEXT|L1=hi|SIZE=2" {
		t.Errorf("text: %s", got)
	}
	if err := s.set("raw", textPage{}, "SRC=UEFI|CPU=x\n"); err != nil {
		t.Fatal(err)
	}
	if got := s.line(mon); got != "SRC=UEFI|CPU=x" {
		t.Errorf("raw: %s", got)
	}
	// a bad request must not change anything
	if err := s.set("raw", textPage{}, "nope"); err == nil {
		t.Error("bad raw accepted")
	}
	if err := s.set("text", textPage{Size: 9}, ""); err == nil {
		t.Error("bad text accepted")
	}
	if err := s.set("off", textPage{}, ""); err == nil {
		t.Error("bad mode accepted")
	}
	if mode, _, raw := s.snapshot(); mode != "raw" || raw != "SRC=UEFI|CPU=x" {
		t.Errorf("state changed by a rejected request: %s %q", mode, raw)
	}
	if err := s.set("monitor", textPage{}, ""); err != nil || s.line(mon) != "SRC=OS|LOAD=1" {
		t.Error("back to monitor failed")
	}
}

func TestParseFrame(t *testing.T) {
	m, order := parseFrame("SRC=DEV|FW=1.2|MAC=00AA|FW=dup|junk|=novalue")
	if m["SRC"] != "DEV" || m["FW"] != "dup" || m["MAC"] != "00AA" || len(m) != 3 {
		t.Errorf("fields = %v", m)
	}
	if strings.Join(order, ",") != "SRC,FW,MAC" {
		t.Errorf("order = %v", order)
	}
}

// TestDeviceInfoRoundTrip plays the display: hwmond writes SRC=CMD|INFO=1 to
// the "port" (a pipe), the fake firmware answers with a SRC=DEV line on the
// return pipe, and readLoop + requestDeviceInfo deliver it.
func TestDeviceInfoRoundTrip(t *testing.T) {
	toDev, hostOut, err := os.Pipe() // host writes hostOut, device reads toDev
	if err != nil {
		t.Fatal(err)
	}
	fromDev, devOut, err := os.Pipe() // device writes devOut, host reads fromDev
	if err != nil {
		t.Fatal(err)
	}
	defer func() { curPort.Store(nil); hostOut.Close(); toDev.Close(); devOut.Close(); fromDev.Close() }()
	curPort.Store(hostOut)
	go readLoop(fromDev)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := toDev.Read(buf)
			if err != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "SRC=CMD|INFO=1") {
				devOut.WriteString("noise line without prefix\r\nSRC=DEV|FW=1.2|BOARD=ES3C28P|HEAP_KB=280|UP_S=42\r\n")
			}
		}
	}()
	if err := requestDeviceInfo(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	d := deviceSnapshot()
	if d.Fields["BOARD"] != "ES3C28P" || d.Fields["UP_S"] != "42" || d.AgeS < 0 {
		t.Errorf("device = %+v", d)
	}
	if strings.Join(d.Order, ",") != "SRC,FW,BOARD,HEAP_KB,UP_S" {
		t.Errorf("order = %v", d.Order)
	}
	if err := setBacklight(40); err != nil {
		t.Fatal(err)
	}
	if backlight.Load() != 40 {
		t.Error("backlight not remembered")
	}
	if err := setBacklight(101); err == nil {
		t.Error("101 % accepted")
	}
	if last, _ := lastSent.Load().(string); last != "SRC=CMD|BL=40" {
		t.Errorf("last sent = %q", last)
	}
}

func TestDeviceHistory(t *testing.T) {
	history.mu.Lock()
	history.s = nil
	history.mu.Unlock()
	handleRxLine("SRC=DEV|FW=1.2|TEMP=41|HEAP_KB=140|HEAP_MIN_KB=120|RX_BYTES=1000|FRAMES=5|UP_S=50|CPU_MHZ=240|STANDBY=0")
	handleRxLine("SRC=DEV|FW=1.2|HEAP_KB=138|HEAP_MIN_KB=118|RX_BYTES=1300|FRAMES=6|UP_S=60|CPU_MHZ=80|STANDBY=1") // CYD-style: no TEMP
	s := history.snapshot()
	if len(s) != 2 {
		t.Fatalf("history len = %d", len(s))
	}
	if s[0].Temp != 41 || s[0].HeapKB != 140 || s[0].RxBytes != 1000 || s[0].Standby {
		t.Errorf("sample 0 = %+v", s[0])
	}
	if s[1].Temp != -1 || s[1].CPUMHz != 80 || !s[1].Standby || s[1].T < s[0].T {
		t.Errorf("sample 1 = %+v", s[1])
	}
	for i := 0; i < historyMax+5; i++ {
		history.add(devSample{T: int64(i)})
	}
	if s := history.snapshot(); len(s) != historyMax || s[0].T != 5 {
		t.Errorf("ring: len %d first %d", len(s), s[0].T)
	}
	srv := httptest.NewServer(panelHandler())
	defer srv.Close()
	r, _ := http.Get(srv.URL + "/api/history")
	var h struct {
		EveryS  int         `json:"every_s"`
		Samples []devSample `json:"samples"`
	}
	json.NewDecoder(r.Body).Decode(&h)
	if h.EveryS != 10 || len(h.Samples) != historyMax {
		t.Errorf("history api = %d, %d samples", h.EveryS, len(h.Samples))
	}
}

func TestPanelAPI(t *testing.T) {
	curPort.Store(nil)
	scr.set("monitor", textPage{}, "")
	srv := httptest.NewServer(panelHandler())
	defer srv.Close()
	post := func(path, body string, hdr map[string]string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		r.Body.Close()
		return r, m
	}

	r, err := http.Get(srv.URL + "/")
	if err != nil || r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %v %v", r.StatusCode, err)
	}
	r, _ = http.Get(srv.URL + "/api/state")
	var st stateResp
	json.NewDecoder(r.Body).Decode(&st)
	if st.Version != daemonVersion || st.Mode != "monitor" || st.Connected || st.Device.AgeS != -1 && st.Device.Fields == nil {
		t.Errorf("state = %+v", st)
	}

	// text mode: accepted even with no display (sent on the next tick / connect)
	r, m := post("/api/screen", `{"mode":"text","text":{"title":"Hi","lines":["one","two"],"size":3}}`, nil)
	if r.StatusCode != 200 || m["mode"] != "text" {
		t.Errorf("screen text = %d %v", r.StatusCode, m)
	}
	if got := scr.line(func() string { return "" }); got != "SRC=TEXT|TITLE=Hi|L1=one|L2=two|SIZE=3" {
		t.Errorf("line after POST = %s", got)
	}
	if r, m = post("/api/screen", `{"mode":"text","text":{"lines":["x"],"size":7}}`, nil); r.StatusCode != 400 || m["error"] == nil {
		t.Errorf("bad size = %d %v", r.StatusCode, m)
	}
	if r, _ = post("/api/screen", `{"mode":"raw","raw":"SRC=CMD|STANDBY=1"}`, nil); r.StatusCode != 200 {
		t.Errorf("raw = %d", r.StatusCode)
	}
	if r, _ = post("/api/screen", `{"mode":"monitor"}`, nil); r.StatusCode != 200 {
		t.Errorf("monitor = %d", r.StatusCode)
	}
	// one-shot send with no display connected
	if r, m = post("/api/send", `{"line":"SRC=CMD|INFO=1"}`, nil); r.StatusCode != 503 || m["error"] == nil {
		t.Errorf("send without port = %d %v", r.StatusCode, m)
	}
	if r, _ = post("/api/send", `{"line":"hello"}`, nil); r.StatusCode != 400 {
		t.Errorf("send junk = %d", r.StatusCode)
	}
	if r, _ = post("/api/device/backlight", `{"percent":150}`, nil); r.StatusCode != 400 {
		t.Errorf("backlight 150 = %d", r.StatusCode)
	}
	if r, _ = post("/api/device/refresh", ``, nil); r.StatusCode != 504 {
		t.Errorf("refresh without port = %d", r.StatusCode)
	}

	// cross-site protection: wrong content type, foreign origin
	req, _ := http.NewRequest("POST", srv.URL+"/api/screen", strings.NewReader(`mode=raw`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 415 {
		t.Errorf("form post = %d", r.StatusCode)
	}
	if r, _ = post("/api/screen", `{"mode":"monitor"}`, map[string]string{"Origin": "http://evil.example"}); r.StatusCode != 403 {
		t.Errorf("foreign origin = %d", r.StatusCode)
	}
	if r, _ = post("/api/screen", `{"mode":"monitor"}`, map[string]string{"Origin": srv.URL}); r.StatusCode != 200 {
		t.Errorf("same origin = %d", r.StatusCode)
	}
}

func TestStartPanelLoopbackOnly(t *testing.T) {
	if err := startPanel("0.0.0.0:0"); err == nil {
		t.Error("non-loopback address accepted")
	}
	if err := startPanel(""); err != nil {
		t.Error("disabled panel:", err)
	}
	if err := startPanel("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if u := panelURL(); !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Errorf("panelURL = %q", u)
	}
	r, err := http.Get(panelURL() + "api/state")
	if err != nil || r.StatusCode != 200 {
		t.Errorf("GET state via listener: %v %v", r, err)
	}
}

func TestAutostartArgsWeb(t *testing.T) {
	if got := strings.Join(autostartArgs("auto", time.Second, defaultWeb), " "); got != "-tray" {
		t.Errorf("defaults = %q", got)
	}
	if got := strings.Join(autostartArgs("COM3", 2*time.Second, "127.0.0.1:9000"), " "); got != "-tray -port COM3 -interval 2s -web 127.0.0.1:9000" {
		t.Errorf("custom = %q", got)
	}
}
