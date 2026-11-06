// hwmond — OS-phase sender for the boot/OS hardware display.
//
// Reads CPU temp / fan / load / RAM from the Linux kernel (hwmon, /proc) and
// writes one "KEY=VAL|KEY=VAL|...\n" line per second to a serial port,
// using the same protocol as hwmon.efi so the display doesn't care who's talking.
//
//	hwmond -port /dev/ttyS0            # real COM1
//	hwmond -port /dev/pts/7            # QEMU/socat pty for testing
//	hwmond -port - -once               # print one line to stdout
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func main() {
	port := flag.String("port", "/dev/ttyS0", "serial device, or - for stdout")
	interval := flag.Duration("interval", time.Second, "send interval")
	once := flag.Bool("once", false, "send one frame and exit")
	flag.Parse()

	var w *os.File
	if *port == "-" {
		w = os.Stdout
	} else {
		f, err := openSerial(*port)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open serial:", err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}

	cpu := newCPULoad()
	static := staticInfo()
	for {
		line := static + "|" + dynamicInfo(cpu) + "\n"
		if _, err := w.WriteString(line); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		if *once {
			return
		}
		time.Sleep(*interval)
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

func cpuModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "?"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return "?"
}

func osName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return "Linux"
}

// ---------- dynamic (every tick) -------------------------------------------

func dynamicInfo(c *cpuLoad) string {
	parts := []string{
		"LOAD=" + strconv.Itoa(c.percent()),
		"TEMP=" + strconv.Itoa(cpuTemp()),
		"FAN=" + strconv.Itoa(fanRPM()),
	}
	if used, total, ok := memMB(); ok {
		parts = append(parts, "RAM_USED_MB="+strconv.Itoa(used), "RAM_MB="+strconv.Itoa(total))
	}
	return strings.Join(parts, "|")
}

// cpuTemp returns the package/die temperature in °C, or -1 if nothing found.
// Prefers coretemp (Intel) / k10temp (AMD) / zenpower; falls back to any temp1.
func cpuTemp() int {
	best := -1
	dirs, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, d := range dirs {
		name := readTrim(d + "/name")
		switch name {
		case "coretemp", "k10temp", "zenpower", "cpu_thermal", "acpitz":
		default:
			continue
		}
		// k10temp labels: Tctl/Tdie; coretemp: "Package id 0"
		inputs, _ := filepath.Glob(d + "/temp*_input")
		for _, in := range inputs {
			label := readTrim(strings.TrimSuffix(in, "_input") + "_label")
			v := readInt(in) / 1000
			if label == "Tdie" || label == "Tctl" || strings.HasPrefix(label, "Package") {
				return v
			}
			if best < 0 || v > best {
				best = v
			}
		}
	}
	if best < 0 { // last resort: thermal zones
		if z, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp"); len(z) > 0 {
			best = readInt(z[0]) / 1000
		}
	}
	return best
}

// fanRPM returns the first non-zero fan reading (typically CPU fan), or 0.
func fanRPM() int {
	fans, _ := filepath.Glob("/sys/class/hwmon/hwmon*/fan*_input")
	for _, f := range fans {
		if v := readInt(f); v > 0 {
			return v
		}
	}
	return 0
}

func memMB() (used, total int, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var tot, avail int
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			tot, _ = strconv.Atoi(f[1])
		case "MemAvailable:":
			avail, _ = strconv.Atoi(f[1])
		}
	}
	if tot == 0 {
		return 0, 0, false
	}
	return (tot - avail) / 1024, tot / 1024, true
}

type cpuLoad struct{ prevIdle, prevTotal uint64 }

func newCPULoad() *cpuLoad { c := &cpuLoad{}; c.percent(); return c }

func (c *cpuLoad) percent() int {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return -1
	}
	f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0]) // "cpu  user nice system idle iowait irq softirq steal"
	if len(f) < 8 || f[0] != "cpu" {
		return -1
	}
	var total, idle uint64
	for i := 1; i < len(f); i++ {
		v, _ := strconv.ParseUint(f[i], 10, 64)
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	dTotal, dIdle := total-c.prevTotal, idle-c.prevIdle
	c.prevTotal, c.prevIdle = total, idle
	if dTotal == 0 {
		return 0
	}
	return int(100 * (dTotal - dIdle) / dTotal)
}

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

// ---------- serial port: 115200 8N1 raw, via termios ioctl (no cgo) ----------

func openSerial(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		// Not a tty (e.g. a regular file or pipe) — fine for testing, just write to it.
		return f, nil
	}
	const baud = syscall.B115200
	t.Iflag = 0 // raw input: no IXON/ICRNL/etc.
	t.Oflag = 0 // raw output: no ONLCR
	t.Lflag = 0 // no ICANON/ECHO
	t.Cflag = baud | syscall.CS8 | syscall.CREAD | syscall.CLOCAL // 8N1, no flow control
	t.Ispeed, t.Ospeed = baud, baud
	t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 0, 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		f.Close()
		return nil, fmt.Errorf("tcsets: %v", e)
	}
	return f, nil
}
