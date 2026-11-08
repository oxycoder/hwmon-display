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
	"time"
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

// disks: "/:62:931,home:40:1863,USB:5:64,Win:71:931" = label : used% : total GB
// for every mounted block-device filesystem: internal partitions, USB sticks /
// external drives (udisks mounts them under /media or /run/media) and Windows
// partitions (ntfs / ntfs3 / ntfs-3g shows up as fuseblk / exfat / vfat).
// Root is listed first, the rest in mount order; the display shows four at most.
func disks() string {
	var d diskList
	seen := map[uint64]bool{}
	for _, m := range mountedVolumes() {
		var st syscall.Statfs_t
		if syscall.Statfs(m, &st) != nil || st.Blocks == 0 {
			continue
		}
		var s syscall.Stat_t
		if syscall.Stat(m, &s) != nil || seen[s.Dev] { // bind mounts / btrfs subvolumes
			continue
		}
		seen[s.Dev] = true
		total := uint64(st.Bsize) * st.Blocks
		avail := uint64(st.Bsize) * st.Bavail
		if !d.add(diskLabel(m), total-avail, total) {
			break
		}
	}
	return d.String()
}

// diskLabel: "/" stays "/" (the display prints it as "root"); anything else is the
// last path element, e.g. /media/tt/Windows -> "Windows", /home -> "home".
func diskLabel(mount string) string {
	if mount == "/" {
		return "/"
	}
	return filepath.Base(mount)
}

// diskFS lists the filesystem types worth showing as "a disk".
var diskFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true,
	"ntfs": true, "ntfs3": true, "fuseblk": true, // Windows partitions (fuseblk = ntfs-3g)
	"exfat": true, "vfat": true, "msdos": true, // USB sticks / SD cards
	"hfsplus": true, "udf": true, "jfs": true, "reiserfs": true, "zfs": true,
}

// mountedVolumes returns the mount points of real block devices from /proc/mounts,
// root first, skipping pseudo filesystems, loop/snap mounts and the EFI partition.
func mountedVolumes() []string {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return []string{"/"}
	}
	return parseMounts(string(b))
}

// parseMounts filters /proc/mounts text down to the volumes worth showing.
func parseMounts(mounts string) []string {
	var out []string
	for _, l := range strings.Split(mounts, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || !diskFS[f[2]] {
			continue
		}
		dev, m := f[0], unescapeMount(f[1])
		if !strings.HasPrefix(dev, "/dev/") && f[2] != "zfs" {
			continue
		}
		if strings.HasPrefix(dev, "/dev/loop") || strings.HasPrefix(dev, "/dev/zram") {
			continue
		}
		if m == "/" {
			out = append([]string{"/"}, out...)
			continue
		}
		skip := false
		for _, p := range []string{"/boot", "/snap/", "/var/lib/docker", "/var/lib/containers", "/var/snap", "/proc", "/sys", "/dev", "/run/"} {
			if strings.HasPrefix(m, p) && !strings.HasPrefix(m, "/run/media") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []string{"/"}
	}
	return out
}

// unescapeMount decodes /proc/mounts octal escapes ("\040" = space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				sb.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
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
		if w := amdGPUPower(d); w >= 0 {
			parts = append(parts, "GPU_W="+strconv.Itoa(w))
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

// amdGPUPower: board power in W from the amdgpu hwmon node (power1_average on
// dGPUs, power1_input on APUs; both in µW), or -1.
func amdGPUPower(dev string) int {
	hw, _ := filepath.Glob(dev + "/hwmon/hwmon*")
	if len(hw) == 0 {
		return -1
	}
	for _, f := range []string{"/power1_average", "/power1_input"} {
		if uw := readInt(hw[0] + f); uw > 0 {
			return (uw + 500000) / 1000000
		}
	}
	return -1
}

// ---------- CPU package power ------------------------------------------------
//
// RAPL through powercap: /sys/class/powercap/intel-rapl:N/energy_uj is a
// running energy counter per package (the intel_rapl driver also covers AMD
// Zen), so power is the energy delta over the sampling interval. Readable by
// root only since kernel 5.10 (CVE-2020-8694); when the files are not readable
// the zenpower hwmon driver's power inputs (µW, core + SoC) are the fallback.

type energySample struct {
	uj uint64
	at time.Time
}

var raplPrev map[string]energySample

// cpuPower returns the CPU package power in W, or -1 when unavailable. The
// first call only primes the counters and returns -1.
func cpuPower() int {
	zones, _ := filepath.Glob("/sys/class/powercap/intel-rapl:[0-9]*")
	now := time.Now()
	if raplPrev == nil {
		raplPrev = map[string]energySample{}
	}
	watts, ok := -1.0, false
	for _, z := range zones {
		if !strings.HasPrefix(readTrim(z+"/name"), "package") {
			continue
		}
		b, err := os.ReadFile(z + "/energy_uj")
		if err != nil {
			continue
		}
		uj, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			continue
		}
		prev, had := raplPrev[z]
		raplPrev[z] = energySample{uj, now}
		if !had {
			continue
		}
		if w, good := raplWatts(prev, energySample{uj, now}, uint64(readInt(z+"/max_energy_range_uj"))); good {
			if !ok {
				watts, ok = 0, true
			}
			watts += w
		}
	}
	if ok {
		return int(watts + 0.5)
	}
	return zenpowerWatts()
}

// raplWatts turns two energy_uj samples into watts, unwrapping one counter
// rollover; false when the interval is too short or the delta is implausible.
func raplWatts(prev, cur energySample, maxRange uint64) (float64, bool) {
	dt := cur.at.Sub(prev.at).Seconds()
	if dt < 0.05 {
		return 0, false
	}
	var duj uint64
	if cur.uj >= prev.uj {
		duj = cur.uj - prev.uj
	} else if maxRange > 0 {
		duj = maxRange - prev.uj + cur.uj
	} else {
		return 0, false
	}
	w := float64(duj) / 1e6 / dt
	if w < 0 || w > 2000 {
		return 0, false
	}
	return w, true
}

// zenpowerWatts sums the zenpower driver's power inputs (µW), or -1.
func zenpowerWatts() int {
	dirs, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, d := range dirs {
		if readTrim(d+"/name") != "zenpower" {
			continue
		}
		inputs, _ := filepath.Glob(d + "/power*_input")
		sum := 0
		for _, in := range inputs {
			sum += readInt(in)
		}
		if sum > 0 {
			return (sum + 500000) / 1000000
		}
	}
	return -1
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
	t.Cc[syscall.VMIN], t.Cc[syscall.VTIME] = 0, 1 // reads return on the first byte or after 100 ms (device replies, panel.go)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		f.Close()
		return nil, fmt.Errorf("tcsets: %v", e)
	}
	return f, nil
}
