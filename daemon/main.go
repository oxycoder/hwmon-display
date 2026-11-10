// hwmond — OS-phase sender for the boot/OS hardware display.
//
// Reads CPU temp / fan / load / RAM from the OS and writes one
// "KEY=VAL|KEY=VAL|...\n" line per second to a serial port, using the same
// protocol as hwmon.efi so the display doesn't care who's talking.
//
//	hwmond -port /dev/ttyACM0          # Linux, USB display
//	hwmond -port /dev/ttyS0            # Linux, COM1
//	hwmond -port COM5                  # Windows (WMI/driver libraries; run elevated on ASUS boards for CPU temp + fan)
//	hwmond -port - -once               # print one line to stdout
//	hwmond -tray                       # also show a tray icon (status, control panel, serial port picker, start-on-login toggle, quit)
//	hwmond -web 127.0.0.1:8086         # control panel address (default); -web "" turns it off
//	hwmond -autostart on               # start at login with the current flags (+ -tray); "off" removes it
//	                                   # (Windows: elevated Task Scheduler logon task via a UAC prompt, so CPU temp/power work)
//
// Per-OS sensor + serial code lives in linux.go / windows.go; the tray icon and
// the login-autostart entry in tray_linux.go / tray_windows.go + autostart_windows.go;
// the control panel (device info, custom screen content, raw commands) in panel.go.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	port := flag.String("port", "auto", "serial device (COM5, /dev/ttyACM0...), - for stdout, or auto (find the USB display)")
	interval := flag.Duration("interval", time.Second, "send interval")
	once := flag.Bool("once", false, "send one frame and exit")
	tray := flag.Bool("tray", true, "show a system tray icon (status, serial port picker, start-on-login toggle, quit)")
	web := flag.String("web", defaultWeb, "control panel address (web UI + JSON API: device info, custom screen content); \"\" disables it")
	autostart := flag.String("autostart", "", "on|off: register/unregister hwmond to start at login (with the current -port/-interval/-web and -tray), then exit")
	flag.Parse()

	loginArgs = autostartArgs(*port, *interval, *web)
	wantPort.Store(*port)
	if *autostart != "" {
		if *autostart == "on" && *port == "-" {
			// A login instance has no stdout; it would exit on the first frame.
			fmt.Fprintln(os.Stderr, "-autostart on: -port - (frames to stdout) cannot be the login configuration")
			os.Exit(1)
		}
		if err := configureAutostart(*autostart); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if !*once {
		if err := startPanel(*web); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	if *tray {
		if err := startTray(); err != nil {
			fmt.Fprintln(os.Stderr, "tray:", err)
		}
	}

	if !*once {
		standbyOnExit()
	}
	cpu := newCPULoad()
	static := staticInfo()
	monitor := func() string { return static + "|" + dynamicInfo(cpu) }
	for {
		w := connect(*once)
		curPort.Store(w)
		onConnect(w)
		for {
			if w == os.Stdout && portSwitched.Swap(false) {
				break // the tray picked a real port while frames went to stdout
			}
			line := scr.line(monitor) + "\n"
			if _, err := w.WriteString(line); err != nil {
				if portSwitched.Swap(false) {
					break // selectPort closed the port on purpose: reconnect to the new one
				}
				fmt.Fprintln(os.Stderr, "write:", err)
				if *once || w == os.Stdout {
					os.Exit(1)
				}
				curPort.Store((*os.File)(nil))
				w.Close() // display unplugged / re-enumerated: go back to detection
				setStatus("display disconnected, searching...")
				break
			}
			noteSent(line)
			if *once {
				return
			}
			time.Sleep(*interval)
		}
	}
}

// connect opens the selected port (selectedPort: the -port flag until the tray
// changes it), or with "auto" searches for a USB display (ESP32-S3 303a:1001,
// Espressif TinyUSB 303a:4001, RP2040 2e8a:000a) and keeps retrying until one
// appears. The selection is re-read on every attempt so a tray change while
// waiting takes effect at once. Exits on failure only in -once mode.
func connect(once bool) *os.File {
	prev := ""
	for {
		port := selectedPort()
		first := port != prev // first try of this selection: say what we are waiting for
		prev = port
		if port == "-" {
			return os.Stdout
		}
		cands := []string{port}
		if port == "auto" {
			cands = findPorts()
		}
		var lastErr error
		for _, c := range cands {
			f, err := openSerial(c)
			if err == nil {
				fmt.Fprintln(os.Stderr, "display on", c)
				setStatus("display on " + c)
				return f
			}
			lastErr = err
			fmt.Fprintln(os.Stderr, "open serial:", err)
		}
		if once {
			fmt.Fprintln(os.Stderr, "no display found")
			os.Exit(1)
		}
		if first {
			if port == "auto" {
				fmt.Fprintln(os.Stderr, "waiting for a USB display...")
				setStatus("waiting for a USB display...")
			} else {
				setStatus(fmt.Sprintf("cannot open %s (%v), retrying...", port, lastErr))
			}
		}
		for i := 0; i < 20 && selectedPort() == port; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// ---------- serial port selection -------------------------------------------

// wantPort is the port frames should go to: "auto", "-" or a device name. It
// starts as the -port flag; the tray menu changes it at run time.
var wantPort atomic.Value

// portSwitched tells the main loop that its write failed because selectPort
// closed the port on purpose, not because the display went away.
var portSwitched atomic.Bool

func selectedPort() string {
	if s, _ := wantPort.Load().(string); s != "" {
		return s
	}
	return "auto"
}

// selectPort switches the daemon to another serial port (or back to "auto").
// The current port is closed so the main loop reconnects right away, and the
// login-autostart arguments follow the choice so a later "Start on login"
// registers it. Safe to call from the tray thread.
func selectPort(name string) {
	if name == "" || name == selectedPort() {
		return
	}
	wantPort.Store(name)
	loginArgs = withPort(loginArgs, name)
	fmt.Fprintln(os.Stderr, "serial port:", name)
	if f := curPort.Swap((*os.File)(nil)); f != nil {
		portSwitched.Store(true)
		if f != os.Stdout {
			f.Close() // the main loop's next write fails and it reconnects
		}
	}
	setStatus("switching to " + name + "...")
}

// withPort returns args with the -port flag replaced by port ("auto" = none).
func withPort(args []string, port string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		if args[i] == "-port" && i+1 < len(args) {
			i++
			continue
		}
		out = append(out, args[i])
	}
	if port != "auto" {
		out = append(out, "-port", port)
	}
	return out
}

// portChoice is one entry of the tray's "Serial port" submenu.
type portChoice struct {
	Name  string // what selectPort takes: "auto" or a device
	Label string // menu text
}

// portChoices lists "Auto" plus every serial port on the machine, known
// display MCUs marked, the current selection kept even when it is not
// enumerated right now (unplugged, or a path the OS does not list).
func portChoices() []portChoice {
	out := []portChoice{{"auto", "Auto (USB display)"}}
	known := map[string]bool{}
	for _, p := range findPorts() {
		known[p] = true
	}
	cur, seen := selectedPort(), false
	for _, p := range listSerialPorts() {
		label := p
		if known[p] {
			label += "  (USB display)"
		}
		out = append(out, portChoice{p, label})
		seen = seen || p == cur
	}
	if !seen && cur != "auto" && cur != "-" {
		out = append(out, portChoice{cur, cur + "  (not found)"})
	}
	return out
}

// ---------- display standby -------------------------------------------------

// curPort is the serial port frames currently go to (nil while searching).
var curPort atomic.Pointer[os.File]

// standbyLine is the control frame that puts the display to sleep right away.
// The firmware also sleeps on its own after a minute of silence; this just
// skips the wait when hwmond quits or the OS shuts down. The next data frame
// wakes the display.
const standbyLine = "SRC=CMD|STANDBY=1\n"

// sendStandby writes the standby frame to the display, if one is connected.
// Safe to call more than once and from any goroutine.
func sendStandby() {
	f := curPort.Swap((*os.File)(nil))
	if f == nil || f == os.Stdout {
		return
	}
	f.WriteString(standbyLine)
	time.Sleep(100 * time.Millisecond) // let the USB/UART drain before the process dies
	f.Close()
}

// standbyOnExit arranges for sendStandby to run when hwmond is told to stop:
// SIGINT/SIGTERM (Ctrl-C, systemd stop, session end on Linux), plus the
// per-OS shutdown hooks in hookShutdown (Windows console control events and
// WM_ENDSESSION on the tray window). The tray's Quit calls it directly.
func standbyOnExit() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		sendStandby()
		os.Exit(0)
	}()
	hookShutdown()
}

// usbIDs lists VID:PID pairs (lower-case hex) that are known display MCUs.
var usbIDs = []string{"303a:1001", "303a:4001", "2e8a:000a"}

func knownUSB(vid, pid string) bool {
	id := strings.ToLower(vid) + ":" + strings.ToLower(pid)
	for _, k := range usbIDs {
		if id == k {
			return true
		}
	}
	return false
}

// ---------- tray status + login autostart (shared) ----------------------------

// status is the one-line state shown as the tray tooltip / first menu entry.
var status atomic.Value

func setStatus(s string) { status.Store(s); trayStatusChanged() }

func getStatus() string {
	if s, _ := status.Load().(string); s != "" {
		return s
	}
	return "starting..."
}

// loginArgs are the flags the login-autostart entry launches hwmond with.
var loginArgs []string

func autostartArgs(port string, interval time.Duration, web string) []string {
	args := []string{"-tray"}
	if port != "auto" {
		args = append(args, "-port", port)
	}
	if interval != time.Second {
		args = append(args, "-interval", interval.String())
	}
	if web != defaultWeb {
		args = append(args, "-web", web)
	}
	return args
}

// defaultWeb is where the control panel listens (loopback only; see panel.go).
const defaultWeb = "127.0.0.1:8086"

// exePath is the absolute path of this binary (symlinks resolved), for the autostart entry.
func exePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

// configureAutostart handles -autostart on|off.
func configureAutostart(mode string) error {
	switch mode {
	case "on", "off":
	default:
		return fmt.Errorf("-autostart: want on or off, got %q", mode)
	}
	if err := setAutostart(mode == "on"); err != nil {
		return fmt.Errorf("autostart %s: %v", mode, err)
	}
	if mode == "on" {
		exe, _ := exePath()
		fmt.Fprintf(os.Stderr, "hwmond will start at login: %s %s\n", exe, strings.Join(loginArgs, " "))
	} else {
		fmt.Fprintln(os.Stderr, "hwmond removed from login autostart")
	}
	return nil
}

// toggleAutostart flips the login entry; used by the tray menu.
func toggleAutostart() {
	if err := setAutostart(!autostartEnabled()); err != nil {
		fmt.Fprintln(os.Stderr, "autostart:", err)
		reportAutostartError(err) // the tray instance has no visible console
	}
}

// ---------- static (read once) --------------------------------------------

func staticInfo() string {
	host, _ := os.Hostname()
	parts := []string{
		"SRC=OS",
		"HOST=" + clean(host),
		"CPU=" + clean(cpuModel()),
		"THREADS=" + strconv.Itoa(runtime.NumCPU()),
		"OS=" + clean(osName()),
	}
	return strings.Join(parts, "|")
}

// ---------- dynamic (every tick) -------------------------------------------

func dynamicInfo(c *cpuLoad) string {
	parts := []string{
		"LOAD=" + strconv.Itoa(c.percent()),
		"TEMP=" + strconv.Itoa(cpuTemp()),
		"FAN=" + strconv.Itoa(fanRPM()),
	}
	if w := cpuPower(); w >= 0 {
		parts = append(parts, "CPU_W="+strconv.Itoa(w))
	}
	if used, total, ok := memMB(); ok {
		parts = append(parts, "RAM_USED_MB="+strconv.Itoa(used), "RAM_MB="+strconv.Itoa(total))
	}
	if d := disks(); d != "" {
		parts = append(parts, "DISK="+d)
	}
	if t := diskTemps(); t != "" {
		parts = append(parts, "DISK_T="+t)
	}
	if g := gpuInfo(); g != "" {
		parts = append(parts, g)
	}
	return strings.Join(parts, "|")
}

// ---------- disk list -------------------------------------------------------

// The display shows at most 4 disks with labels of up to 6 chars, and keeps
// each protocol value in a 64-byte buffer, so the DISK value stays <= 63 bytes.
const (
	maxDisks     = 4
	maxDiskLabel = 6
	maxDiskValue = 63
)

// diskList accumulates "label:used%:GB" entries within the display's limits.
type diskList struct{ parts []string }

// add appends a volume; false when the list is full. Labels lose protocol
// delimiters and are cut to the display's label width.
func (d *diskList) add(label string, usedBytes, totalBytes uint64) bool {
	if len(d.parts) >= maxDisks || totalBytes == 0 {
		return false
	}
	label = strings.NewReplacer(":", "", ",", "", " ", "").Replace(clean(label))
	if label == "" {
		label = "disk"
	}
	if len(label) > maxDiskLabel {
		label = label[:maxDiskLabel]
	}
	e := fmt.Sprintf("%s:%d:%d", label, 100*usedBytes/totalBytes, totalBytes/(1000*1000*1000))
	if cur := d.String(); len(cur)+len(e)+1 > maxDiskValue && cur != "" || len(e) > maxDiskValue {
		return false
	}
	d.parts = append(d.parts, e)
	return true
}

func (d *diskList) String() string { return strings.Join(d.parts, ",") }

// cpuLoad tracks CPU busy % between ticks; per-OS implementation.
type cpuLoad struct{ prevIdle, prevTotal uint64 }

func newCPULoad() *cpuLoad { c := &cpuLoad{}; c.percent(); return c }

// ---------- helpers ---------------------------------------------------------

func readTrim(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
func readInt(p string) int     { v, _ := strconv.Atoi(readTrim(p)); return v }

// clean strips protocol delimiters and non-ASCII from a value.
func clean(s string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == '|' || r == '=' || r == '\n' || r == '\r':
			sb.WriteByte(' ')
		case r < 0x20 || r > 0x7e:
			sb.WriteByte('?')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// sortPorts orders port names naturally (COM3 before COM10, ttyACM0 before
// ttyUSB0) and drops duplicates.
func sortPorts(ports []string) []string {
	sort.SliceStable(ports, func(i, j int) bool {
		a, an := splitNum(ports[i])
		b, bn := splitNum(ports[j])
		if a != b {
			return a < b
		}
		return an < bn
	})
	out := ports[:0]
	for i, p := range ports {
		if i == 0 || p != ports[i-1] {
			out = append(out, p)
		}
	}
	return out
}

// splitNum splits "COM12" into ("COM", 12); a name without a trailing number gets -1.
func splitNum(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, -1
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}
