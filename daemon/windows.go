//go:build windows

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const defaultPort = "COM5"

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes     = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatx  = kernel32.NewProc("GlobalMemoryStatusEx")
	procSetCommState       = kernel32.NewProc("SetCommState")
	procGetCommState       = kernel32.NewProc("GetCommState")
	procSetCommTimeouts    = kernel32.NewProc("SetCommTimeouts")
	procEscapeCommFunc     = kernel32.NewProc("EscapeCommFunction")
	procGetLogicalDrives   = kernel32.NewProc("GetLogicalDrives")
	procGetDriveType       = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
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

// findPorts lists COM ports of known display MCUs from the USB enum tree:
// HKLM\SYSTEM\CurrentControlSet\Enum\USB\VID_303A&PID_1001&MI_00\<inst>\Device Parameters\PortName.
// The registry remembers unplugged devices too, so callers must try to open each one.
func findPorts() []string {
	const usb = `SYSTEM\CurrentControlSet\Enum\USB`
	var ports []string
	for _, dev := range regSubkeys(usb) {
		d := strings.ToUpper(dev)
		if !strings.HasPrefix(d, "VID_") || len(d) < 17 || d[8:13] != "&PID_" {
			continue
		}
		if !knownUSB(d[4:8], d[13:17]) {
			continue
		}
		for _, inst := range regSubkeys(usb + `\` + dev) {
			if p := regString(usb+`\`+dev+`\`+inst+`\Device Parameters`, "PortName", ""); strings.HasPrefix(p, "COM") {
				ports = append(ports, p)
			}
		}
	}
	return ports
}

func regSubkeys(path string) []string {
	var h syscall.Handle
	p, _ := syscall.UTF16PtrFromString(path)
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &h) != nil {
		return nil
	}
	defer syscall.RegCloseKey(h)
	var out []string
	for i := uint32(0); ; i++ {
		buf := make([]uint16, 256)
		n := uint32(len(buf))
		if syscall.RegEnumKeyEx(h, i, &buf[0], &n, nil, nil, nil, nil) != nil {
			break
		}
		out = append(out, syscall.UTF16ToString(buf[:n]))
	}
	return out
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

// disks: "C:62:931,D:40:1863,E:5:64" = letter : used% : total GB for fixed and
// removable (USB stick / external HDD) drives with media present. Empty card
// readers fail GetDiskFreeSpaceEx and are skipped; network drives are ignored.
func disks() string {
	r, _, _ := procGetLogicalDrives.Call()
	var d diskList
	for i := 0; i < 26; i++ {
		if r&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := syscall.UTF16PtrFromString(root)
		const driveRemovable, driveFixed = 2, 3
		if t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p))); t != driveFixed && t != driveRemovable {
			continue
		}
		var free, total, totalFree uint64
		if ok, _, _ := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)),
			uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree))); ok == 0 || total == 0 {
			continue
		}
		if !d.add(string(rune('A'+i)), total-totalFree, total) {
			break
		}
	}
	return d.String()
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
