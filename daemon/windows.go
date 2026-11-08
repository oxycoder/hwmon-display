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
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatx    = kernel32.NewProc("GlobalMemoryStatusEx")
	procSetCommState         = kernel32.NewProc("SetCommState")
	procGetCommState         = kernel32.NewProc("GetCommState")
	procSetCommTimeouts      = kernel32.NewProc("SetCommTimeouts")
	procEscapeCommFunc       = kernel32.NewProc("EscapeCommFunction")
	procGetLogicalDrives     = kernel32.NewProc("GetLogicalDrives")
	procGetDriveType         = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceEx   = kernel32.NewProc("GetDiskFreeSpaceExW")
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

func cpuTemp() int { return lhm().cpuTemp }
func fanRPM() int  { return lhm().fan }

// gpuInfo: "GPU=name|GPU_TEMP=61|GPU_LOAD=37|VRAM_USED_MB=7500|VRAM_MB=16384"
// from LibreHardwareMonitor; "" when no GPU node is present.
func gpuInfo() string {
	r := lhm()
	if r.gpuName == "" {
		return ""
	}
	parts := []string{"GPU=" + clean(shortGPUName(r.gpuName))}
	if r.gpuTemp >= 0 {
		parts = append(parts, "GPU_TEMP="+strconv.Itoa(r.gpuTemp))
	}
	if r.gpuLoad >= 0 {
		parts = append(parts, "GPU_LOAD="+strconv.Itoa(r.gpuLoad))
	}
	if r.gpuMemUsed >= 0 && r.gpuMemTotal > 0 {
		parts = append(parts, "VRAM_USED_MB="+strconv.Itoa(r.gpuMemUsed), "VRAM_MB="+strconv.Itoa(r.gpuMemTotal))
	}
	return strings.Join(parts, "|")
}

// shortGPUName drops vendor boilerplate so the name fits one LCD line.
func shortGPUName(n string) string {
	for _, cut := range []string{"NVIDIA ", "AMD ", "Intel(R) ", "Intel ", "(R)", "(TM)", " Graphics Family"} {
		n = strings.ReplaceAll(n, cut, "")
	}
	return strings.Join(strings.Fields(n), " ")
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

func diskTemps() string {
	var ss []string
	for _, t := range lhm().diskTemps {
		ss = append(ss, strconv.Itoa(t))
	}
	return strings.Join(ss, ",")
}

// ---------- LibreHardwareMonitor JSON ------------------------------------------

type lhmNode struct {
	Text     string    `json:"Text"`
	Value    string    `json:"Value"`
	ImageURL string    `json:"ImageURL"`
	HardwareId string  `json:"HardwareId"`
	Children []lhmNode `json:"Children"`
}

type lhmReading struct {
	cpuTemp, fan int
	diskTemps    []int
	gpuName      string
	gpuTemp      int
	gpuLoad      int
	gpuMemUsed   int
	gpuMemTotal  int
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
			findDiskTemps(&root, &r.diskTemps)
			findGPU(&root, &r)
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

// cpuNodes collects every hardware node with the CPU icon, skipping LHM's
// "Virtual" pseudo-devices, with the one matching the registry CPU name first.
func cpuNodes(n *lhmNode, out *[]*lhmNode) {
	if strings.Contains(n.ImageURL, "cpu.png") && !strings.Contains(strings.ToLower(n.Text), "virtual") {
		*out = append(*out, n)
	}
	for i := range n.Children {
		cpuNodes(&n.Children[i], out)
	}
}

func findCPUTemp(root *lhmNode) int {
	// 1. Prioritize motherboard Super I/O / LPC sensor named "CPU" (e.g. 44°C)
	if v := findSuperIOCPUTemp(root); v >= 0 {
		return v
	}

	// 2. Fallback to CPU package sensors
	var nodes []*lhmNode
	cpuNodes(root, &nodes)
	model := strings.ToLower(cpuModel())
	for pass := 0; pass < 2; pass++ {
		for _, n := range nodes {
			name := strings.ToLower(n.Text)
			if pass == 0 && !(strings.Contains(model, name) || strings.Contains(name, firstWords(model, 3))) {
				continue
			}
			if v := tempOf(n, []string{"Package", "Tctl", "Tdie", "CPU Core", "Core Average"}); v >= 0 {
				return v
			}
		}
	}
	return -1
}

func findSuperIOCPUTemp(n *lhmNode) int {
	if strings.Contains(n.HardwareId, "/lpc") {
		for i := range n.Children {
			if n.Children[i].Text == "Temperatures" {
				for _, s := range n.Children[i].Children {
					if s.Text == "CPU" {
						return numOf(s.Value)
					}
				}
			}
		}
	}
	for i := range n.Children {
		if v := findSuperIOCPUTemp(&n.Children[i]); v >= 0 {
			return v
		}
	}
	return -1
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

// tempOf: the node's "Temperatures" group → first sensor matching prefs, else the max.
func tempOf(n *lhmNode, prefs []string) int {
	for i := range n.Children {
		if n.Children[i].Text != "Temperatures" {
			continue
		}
		best := -1
		for _, s := range n.Children[i].Children {
			v := numOf(s.Value)
			for _, p := range prefs {
				if strings.Contains(s.Text, p) {
					return v
				}
			}
			if v > best {
				best = v
			}
		}
		return best
	}
	return -1
}

// findFan: a fan labelled "CPU" if there is one, else the first spinning fan.
func findFan(root *lhmNode) int {
	var fans []lhmNode
	collectFans(root, &fans)
	for _, f := range fans {
		if strings.Contains(strings.ToUpper(f.Text), "CPU") && numOf(f.Value) > 0 {
			return numOf(f.Value)
		}
	}
	for _, f := range fans {
		if v := numOf(f.Value); v > 0 {
			return v
		}
	}
	return 0
}

func collectFans(n *lhmNode, out *[]lhmNode) {
	if n.Text == "Fans" {
		*out = append(*out, n.Children...)
	}
	for i := range n.Children {
		collectFans(&n.Children[i], out)
	}
}

// findDiskTemps: temperatures of storage devices (hdd icon), in tree order.
func findDiskTemps(n *lhmNode, out *[]int) {
	if strings.Contains(n.ImageURL, "hdd.png") {
		if v := tempOf(n, []string{"Temperature"}); v >= 0 {
			*out = append(*out, v)
		}
	}
	for i := range n.Children {
		findDiskTemps(&n.Children[i], out)
	}
}

// ---------- GPU Parsing Support ----------------------------------------------

func findGPU(root *lhmNode, out *lhmReading) {
	var gpuNode *lhmNode
	walkGPU(root, &gpuNode)
	if gpuNode == nil {
		return
	}
	out.gpuName = gpuNode.Text
	out.gpuTemp = tempOf(gpuNode, []string{"GPU Core", "Core", "Hot Spot"})
	out.gpuLoad = loadOf(gpuNode, []string{"GPU Core", "D3D 3D"})
	out.gpuMemUsed, out.gpuMemTotal = gpuMemOf(gpuNode)
}

func walkGPU(n *lhmNode, found **lhmNode) {
	if strings.HasPrefix(n.HardwareId, "/gpu") || strings.Contains(n.ImageURL, "ati.png") || strings.Contains(n.ImageURL, "nvidia.png") || strings.Contains(n.ImageURL, "intel.png") {
		*found = n
		return
	}
	for i := range n.Children {
		walkGPU(&n.Children[i], found)
		if *found != nil {
			return
		}
	}
}

func loadOf(n *lhmNode, prefs []string) int {
	for i := range n.Children {
		if n.Children[i].Text == "Load" {
			best := -1
			for _, s := range n.Children[i].Children {
				v := numOf(s.Value)
				for _, p := range prefs {
					if strings.Contains(s.Text, p) {
						return v
					}
				}
				if v > best {
					best = v
				}
			}
			return best
		}
	}
	return -1
}

func gpuMemOf(n *lhmNode) (used, total int) {
	used, total = -1, -1
	for i := range n.Children {
		if n.Children[i].Text == "Data" || n.Children[i].Text == "SmallData" {
			for _, s := range n.Children[i].Children {
				if strings.Contains(s.Text, "GPU Memory Used") || strings.Contains(s.Text, "Dedicated Memory Used") {
					used = numMB(s.Value)
				}
				if strings.Contains(s.Text, "GPU Memory Total") || strings.Contains(s.Text, "Dedicated Memory Total") {
					total = numMB(s.Value)
				}
			}
		}
	}
	return
}

func numMB(v string) int {
	f := strings.Fields(strings.ReplaceAll(v, ",", ""))
	if len(f) < 2 {
		return numOf(v)
	}
	val, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return -1
	}
	unit := strings.ToUpper(f[1])
	if strings.HasPrefix(unit, "GB") {
		return int(val * 1024 + 0.5)
	}
	return int(val + 0.5)
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