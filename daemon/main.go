// hwmond — OS-phase sender for the boot/OS hardware display.
//
// Reads CPU temp / fan / load / RAM from the OS and writes one
// "KEY=VAL|KEY=VAL|...\n" line per second to a serial port, using the same
// protocol as hwmon.efi so the display doesn't care who's talking.
//
//	hwmond -port /dev/ttyACM0          # Linux, USB display
//	hwmond -port /dev/ttyS0            # Linux, COM1
//	hwmond -port COM5                  # Windows (temps/fans via LibreHardwareMonitor's web server)
//	hwmond -port - -once               # print one line to stdout
//
// Per-OS sensor + serial code lives in linux.go / windows.go.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func main() {
	port := flag.String("port", defaultPort, "serial device (COM5, /dev/ttyACM0...), or - for stdout")
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
	if d := disks(); d != "" {
		parts = append(parts, "DISK="+d)
	}
	if t := diskTemps(); t != "" {
		parts = append(parts, "DISK_T="+t)
	}
	return strings.Join(parts, "|")
}

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
