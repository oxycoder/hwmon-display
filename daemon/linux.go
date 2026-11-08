//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const defaultPort = "/dev/ttyACM0"

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

// disks: "/:62:931,/home:40:1863" = mount : used% : total GB for the main filesystems.
func disks() string {
	var parts []string
	for _, m := range []string{"/", "/home", "/data"} {
		var st syscall.Statfs_t
		if syscall.Statfs(m, &st) != nil || st.Blocks == 0 {
			continue
		}
		total := uint64(st.Bsize) * st.Blocks
		avail := uint64(st.Bsize) * st.Bavail
		if m != "/" && sameDevice(m) {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d:%d", m, 100*(total-avail)/total, total/(1000*1000*1000)))
	}
	return strings.Join(parts, ",")
}

func sameDevice(m string) bool {
	var a, b syscall.Stat_t
	return syscall.Stat("/", &a) == nil && syscall.Stat(m, &b) == nil && a.Dev == b.Dev
}

func diskTemps() string {
	var ss []string
	dirs, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, d := range dirs {
		switch readTrim(d + "/name") {
		case "nvme", "drivetemp":
			if v := readInt(d + "/temp1_input"); v > 0 {
				ss = append(ss, strconv.Itoa(v/1000))
			}
		}
	}
	return strings.Join(ss, ",")
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

// gpuInfo: GPU name/temp/load/VRAM via sysfs (amdgpu). Nvidia needs nvidia-smi; not wired yet.
func gpuInfo() string {
	dirs, _ := filepath.Glob("/sys/class/drm/card[0-9]/device")
	for _, d := range dirs {
		if readTrim(d+"/vendor") != "0x1002" {
			continue
		}
		parts := []string{"GPU=" + clean(amdGPUName(d))}
		if hw, _ := filepath.Glob(d + "/hwmon/hwmon*"); len(hw) > 0 {
			if t := readInt(hw[0] + "/temp1_input"); t > 0 {
				parts = append(parts, "GPU_TEMP="+strconv.Itoa(t/1000))
			}
		}
		if v := readTrim(d + "/gpu_busy_percent"); v != "" {
			parts = append(parts, "GPU_LOAD="+v)
		}
		used, total := readInt(d+"/mem_info_vram_used"), readInt(d+"/mem_info_vram_total")
		if total > 0 {
			parts = append(parts, "VRAM_USED_MB="+strconv.Itoa(used>>20), "VRAM_MB="+strconv.Itoa(total>>20))
		}
		return strings.Join(parts, "|")
	}
	return ""
}

func amdGPUName(dev string) string {
	if n := readTrim(dev + "/product_name"); n != "" {
		return n
	}
	return "AMD GPU " + strings.TrimPrefix(readTrim(dev+"/device"), "0x")
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

// ---------- serial port: 115200 8N1 raw, via termios ioctl (no cgo) ----------

// findPorts lists /dev/tty* nodes whose USB parent is a known display MCU:
// /sys/class/tty/ttyACM0/device -> the CDC interface; its parent holds idVendor/idProduct.
func findPorts() []string {
	var ports []string
	ttys, _ := filepath.Glob("/sys/class/tty/tty*/device")
	for _, dev := range ttys {
		real, err := filepath.EvalSymlinks(dev) // .../usb1/1-3/1-3:1.0 (interface)
		if err != nil {
			continue
		}
		for up, i := filepath.Dir(real), 0; i < 4 && up != "/"; up, i = filepath.Dir(up), i+1 {
			if vid := readTrim(up + "/idVendor"); vid != "" {
				if knownUSB(vid, readTrim(up+"/idProduct")) {
					ports = append(ports, "/dev/"+filepath.Base(filepath.Dir(dev)))
				}
				break
			}
		}
	}
	return ports
}

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
	t.Iflag = 0                                                   // raw input: no IXON/ICRNL/etc.
	t.Oflag = 0                                                   // raw output: no ONLCR
	t.Lflag = 0                                                   // no ICANON/ECHO
	t.Cflag = baud | syscall.CS8 | syscall.CREAD | syscall.CLOCAL // 8N1, no flow control
	t.Ispeed, t.Ospeed = baud, baud
	t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 0, 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		f.Close()
		return nil, fmt.Errorf("tcsets: %v", e)
	}
	return f, nil
}
