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
//	hwmond -tray                       # also show a tray icon (status, start-on-login toggle, quit)
//	hwmond -autostart on               # start at login with the current flags (+ -tray); "off" removes it
//
// Per-OS sensor + serial code lives in linux.go / windows.go; the tray icon and
// the login-autostart entry in tray_linux.go / tray_windows.go.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	tray := flag.Bool("tray", true, "show a system tray icon (status, start-on-login toggle, quit)")
	autostart := flag.String("autostart", "", "on|off: register/unregister hwmond to start at login (with the current -port/-interval and -tray), then exit")
	flag.Parse()

	loginArgs = autostartArgs(*port, *interval)
	if *autostart != "" {
		if err := configureAutostart(*autostart); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
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
	for {
		w := connect(*port, *once)
		curPort.Store(w)
		for {
			line := static + "|" + dynamicInfo(cpu) + "\n"
			if _, err := w.WriteString(line); err != nil {
				fmt.Fprintln(os.Stderr, "write:", err)
				if *once || w == os.Stdout {
					os.Exit(1)
				}
				curPort.Store((*os.File)(nil))
				w.Close() // display unplugged / re-enumerated: go back to detection
				setStatus("display disconnected, searching...")
				break
			}
			if *once {
				return
			}
			time.Sleep(*interval)
		}
	}
}

// connect opens the requested port, or with "auto" searches for a USB display
// (ESP32-S3 303a:1001, Espressif TinyUSB 303a:4001, RP2040 2e8a:000a) and keeps
// retrying until one appears. Exits on failure only in -once mode.
func connect(port string, once bool) *os.File {
	if port == "-" {
		return os.Stdout
	}
	for attempt := 0; ; attempt++ {
		cands := []string{port}
		if port == "auto" {
			cands = findPorts()
		}
		for _, c := range cands {
			f, err := openSerial(c)
			if err == nil {
				fmt.Fprintln(os.Stderr, "display on", c)
				setStatus("display on " + c)
				return f
			}
			fmt.Fprintln(os.Stderr, "open serial:", err)
		}
		if once {
			fmt.Fprintln(os.Stderr, "no display found")
			os.Exit(1)
		}
		if attempt == 0 {
			fmt.Fprintln(os.Stderr, "waiting for a USB display...")
			setStatus("waiting for a USB display...")
		}
		time.Sleep(2 * time.Second)
	}
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

func autostartArgs(port string, interval time.Duration) []string {
	args := []string{"-tray"}
	if port != "auto" {
		args = append(args, "-port", port)
	}
	if interval != time.Second {
		args = append(args, "-interval", interval.String())
	}
	return args
}

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
