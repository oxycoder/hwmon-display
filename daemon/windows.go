//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const defaultPort = "COM5"

// Temperatures and fan speeds are not readable on Windows without a kernel
// driver, so they come from LibreHardwareMonitor: Options → Remote Web Server
// → Run (default port 8085). Without it, TEMP/FAN render as "--".
var lhmURL = envOr("HWMON_LHM_URL", "http://localhost:8085/data.json")

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes    = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatx = kernel32.NewProc("GlobalMemoryStatusEx")
	procSetCommState      = kernel32.NewProc("SetCommState")
	procGetCommState      = kernel32.NewProc("GetCommState")
	procSetCommTimeouts   = kernel32.NewProc("SetCommTimeouts")
	procEscapeCommFunc    = kernel32.NewProc("EscapeCommFunction")
)

// ---------- static -----------------------------------------------------------

func cpuModel() string {
	return regString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString", "?")
}

func osName() string {
	k := `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	name := regString(k, "ProductName", "Windows")
	ver := regString(k, "DisplayVersion", "")
	build := regString(k, "CurrentBuildNumber", "")
	// Windows 11 still reports "Windows 10 ..." in ProductName; fix by build number.
	if b, _ := strconv.Atoi(build); b >= 22000 {
		name = strings.Replace(name, "Windows 10", "Windows 11", 1)
	}
	return strings.TrimSpace(name + " " + ver)
}

func regString(path, name, def string) string {
	p, _ := syscall.UTF16PtrFromString(path)
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &h) != nil {
		return def
	}
	defer syscall.RegCloseKey(h)
	n, _ := syscall.UTF16PtrFromString(name)
	var typ, size uint32
	if syscall.RegQueryValueEx(h, n, nil, &typ, nil, &size) != nil || size == 0 {
		return def
	}
	buf := make([]uint16, size/2+1)
	if syscall.RegQueryValueEx(h, n, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil {
		return def
	}
	return strings.TrimRight(syscall.UTF16ToString(buf), "\x00")
}

// ---------- dynamic ----------------------------------------------------------

func (c *cpuLoad) percent() int {
	var idle, kern, user syscall.Filetime
	r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return -1
	}
	ft := func(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	i, total := ft(idle), ft(kern)+ft(user) // kernel time includes idle
	dTotal, dIdle := total-c.prevTotal, i-c.prevIdle
	c.prevTotal, c.prevIdle = total, i
	if dTotal == 0 {
		return 0
	}
	return int(100 * (dTotal - dIdle) / dTotal)
}

func memMB() (used, total int, ok bool) {
	var m struct {
		Length                                             uint32
		MemoryLoad                                         uint32
		TotalPhys, AvailPhys, TotalPageFile, AvailPageFile uint64
		TotalVirtual, AvailVirtual, AvailExtendedVirtual   uint64
	}
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, 0, false
	}
	mb := func(b uint64) int { return int(b / (1024 * 1024)) }
	return mb(m.TotalPhys - m.AvailPhys), mb(m.TotalPhys), true
}

func cpuTemp() int { return lhm().cpuTemp }
func fanRPM() int  { return lhm().fan }

// ---------- LibreHardwareMonitor JSON ------------------------------------------

type lhmNode struct {
	Text     string    `json:"Text"`
	Value    string    `json:"Value"`
	ImageURL string    `json:"ImageURL"`
	Children []lhmNode `json:"Children"`
}

type lhmReading struct {
	cpuTemp, fan int
	at           time.Time
}

var lhmCache lhmReading
var lhmClient = &http.Client{Timeout: 800 * time.Millisecond}

// lhm fetches at most once per tick; returns -1/0 when LHM isn't running.
func lhm() lhmReading {
	if time.Since(lhmCache.at) < 900*time.Millisecond {
		return lhmCache
	}
	r := lhmReading{cpuTemp: -1, fan: 0, at: time.Now()}
	resp, err := lhmClient.Get(lhmURL)
	if err == nil {
		defer resp.Body.Close()
		var root lhmNode
		if json.NewDecoder(resp.Body).Decode(&root) == nil {
			r.cpuTemp = findCPUTemp(&root)
			r.fan = findFan(&root)
		}
	}
	lhmCache = r
	return r
}

func numOf(v string) int { // "45.3 °C" / "1,180 RPM" -> 45 / 1180
	f := strings.Fields(strings.ReplaceAll(v, ",", ""))
	if len(f) == 0 {
		return -1
	}
	x, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return -1
	}
	return int(x + 0.5)
}

// findCPUTemp: node whose ImageURL is the CPU icon → "Temperatures" → best sensor.
func findCPUTemp(n *lhmNode) int {
	if strings.Contains(n.ImageURL, "cpu.png") {
		for i := range n.Children {
			if n.Children[i].Text == "Temperatures" {
				best := -1
				for _, s := range n.Children[i].Children {
					v := numOf(s.Value)
					t := s.Text
					if strings.Contains(t, "Package") || strings.Contains(t, "Tctl") || strings.Contains(t, "Tdie") || t == "CPU Core" {
						return v
					}
					if v > best {
						best = v
					}
				}
				return best
			}
		}
	}
	for i := range n.Children {
		if v := findCPUTemp(&n.Children[i]); v >= 0 {
			return v
		}
	}
	return -1
}

// findFan: first non-zero fan under any "Fans" group (motherboard first in LHM's tree).
func findFan(n *lhmNode) int {
	if n.Text == "Fans" {
		for _, s := range n.Children {
			if v := numOf(s.Value); v > 0 {
				return v
			}
		}
	}
	for i := range n.Children {
		if v := findFan(&n.Children[i]); v > 0 {
			return v
		}
	}
	return 0
}

// ---------- serial port -------------------------------------------------------

type dcb struct {
	DCBlength, BaudRate uint32
	Flags               uint32
	WReserved           uint16
	XonLim, XoffLim     uint16
	ByteSize, Parity    byte
	StopBits            byte
	XonChar, XoffChar   int8
	ErrorChar, EofChar  int8
	EvtChar             int8
	WReserved1          uint16
}

func openSerial(name string) (*os.File, error) {
	if !strings.HasPrefix(name, `\\.\`) {
		name = `\\.\` + name // required for COM10+
	}
	p, _ := syscall.UTF16PtrFromString(name)
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	var d dcb
	d.DCBlength = uint32(unsafe.Sizeof(d))
	procGetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d)))
	d.BaudRate = 115200
	d.ByteSize, d.Parity, d.StopBits = 8, 0, 0 // 8N1
	// fBinary | fDtrControl=ENABLE | fRtsControl=ENABLE; no flow control, no parity check
	d.Flags = 1 | (1 << 4) | (1 << 12)
	if r, _, e := procSetCommState.Call(uintptr(h), uintptr(unsafe.Pointer(&d))); r == 0 {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("SetCommState: %v", e)
	}
	// write timeout 1 s so a dead device doesn't block forever
	var to = struct{ RI, RTM, RTC, WTM, WTC uint32 }{0xFFFFFFFF, 0, 0, 10, 1000}
	procSetCommTimeouts.Call(uintptr(h), uintptr(unsafe.Pointer(&to)))
	procEscapeCommFunc.Call(uintptr(h), 5) // SETDTR
	return os.NewFile(uintptr(h), name), nil
}
