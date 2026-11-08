//go:build windows

package main

// Sensor readings on Windows, native first: WMI (root\WMI ACPI thermal zone,
// root\CIMV2 GPU performance counters and video controller), the storage
// temperature IOCTL on each physical drive (falling back to the Storage
// namespace's S.M.A.R.T. counters, which need admin rights), the GPU vendor's
// driver library for GPU temperature (gpuvendor_windows.go) and the
// display-class registry key for VRAM size. Nothing here needs a helper
// application. Whatever the platform cannot provide (CPU temperature when the
// firmware exposes no thermal zone, which is most desktop boards; fan RPM) is
// filled from LibreHardwareMonitor if its web server happens to be running;
// otherwise the field stays -1 and the display shows "--".

import (
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// reading is one merged snapshot. -1 (fan: 0) means unknown.
type reading struct {
	cpuTemp, fan int
	cpuDie       bool // cpuTemp is the CPU's die sensor (Tctl/Tdie, Package), not a board thermistor
	diskTemps    []int
	gpuName      string
	gpuTemp      int
	gpuLoad      int
	gpuMemUsed   int
	gpuMemTotal  int
	src          string // which sources contributed, for the tray/status line
}

func cpuTemp() int { return sample().cpuTemp }
func fanRPM() int  { return sample().fan }

// gpuInfo: "GPU=name|GPU_TEMP=61|GPU_LOAD=37|VRAM_USED_MB=7500|VRAM_MB=16384";
// "" when no usable GPU is present.
func gpuInfo() string {
	r := sample()
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

func diskTemps() string {
	var ss []string
	for _, t := range sample().diskTemps {
		ss = append(ss, strconv.Itoa(t))
	}
	return strings.Join(ss, ",")
}

// ---------- sampling loop -----------------------------------------------------

// The GPU performance-counter provider takes a few hundred ms to enumerate
// every process's engines, so sampling runs on its own goroutine and the
// serial tick just reads the latest snapshot.
var (
	sampleMu   sync.Mutex
	sampleCur  reading
	sampleOnce sync.Once
)

func sample() reading {
	sampleOnce.Do(func() {
		sampleCur = collect()
		go func() {
			for {
				start := time.Now()
				r := collect()
				sampleMu.Lock()
				sampleCur = r
				sampleMu.Unlock()
				if d := time.Second - time.Since(start); d > 0 {
					time.Sleep(d)
				}
			}
		}()
	})
	sampleMu.Lock()
	defer sampleMu.Unlock()
	return sampleCur
}

// collect merges the native reading with LHM for whatever is still missing.
// CPU temperature is the exception: a die sensor (Tctl/Tdie, Package) from
// either side beats a board thermistor or ACPI zone from the other, so LHM's
// die reading replaces a native socket reading whenever LHM is running.
func collect() reading {
	r := wmiSample()
	srcs := []string{"wmi"}
	l := lhm()
	if lhmAvailable {
		srcs = append(srcs, "lhm")
	}
	r.cpuTemp, r.cpuDie = pickCPUTemp(r.cpuTemp, r.cpuDie, l)
	if r.fan == 0 || r.gpuTemp < 0 || len(r.diskTemps) == 0 || r.gpuName == "" {
		if r.fan == 0 {
			r.fan = l.fan
		}
		if r.gpuTemp < 0 {
			r.gpuTemp = l.gpuTemp
		}
		if len(r.diskTemps) == 0 {
			r.diskTemps = l.diskTemps
		}
		if r.gpuName == "" && l.gpuName != "" {
			r.gpuName, r.gpuLoad, r.gpuMemUsed, r.gpuMemTotal = l.gpuName, l.gpuLoad, l.gpuMemUsed, l.gpuMemTotal
		} else {
			if r.gpuLoad < 0 {
				r.gpuLoad = l.gpuLoad
			}
			if r.gpuMemUsed < 0 {
				r.gpuMemUsed = l.gpuMemUsed
			}
			if r.gpuMemTotal <= 0 {
				r.gpuMemTotal = l.gpuMemTotal
			}
		}
	}
	r.src = strings.Join(srcs, "+")
	return r
}

// pickCPUTemp: a die sensor first (native Tctl, then LHM's), else the native
// board reading (ACPI zone / Super I/O), else whatever LHM found, else -1.
func pickCPUTemp(native int, nativeDie bool, l lhmReading) (int, bool) {
	if nativeDie && native >= 0 {
		return native, true
	}
	if l.cpuDie && l.cpuTemp >= 0 {
		return l.cpuTemp, true
	}
	if native >= 0 {
		return native, false
	}
	return l.cpuTemp, false
}

// ---------- native (WMI + registry) -------------------------------------------

const (
	nsCIMV2   = `root\CIMV2`
	nsWMI     = `root\WMI`
	nsStorage = `root\Microsoft\Windows\Storage`
)

// wmiSample reads everything the OS exposes without a driver or helper.
func wmiSample() reading {
	g := gpuStatic()
	r := reading{cpuTemp: thermalZoneTemp(), fan: 0, gpuTemp: -1, gpuLoad: -1, gpuMemUsed: -1,
		gpuName: g.name, gpuMemTotal: g.memTotalMB, diskTemps: storageTemps()}
	if g.name != "" {
		r.gpuLoad, r.gpuMemUsed = gpuPerf()
		r.gpuTemp = gpuVendorTemp()
	}
	// The ASUS path can reach the CPU's own Tctl, which beats an ACPI zone.
	if t, die := asusCPUTemp(); die || (t >= 0 && r.cpuTemp < 0) {
		r.cpuTemp, r.cpuDie = t, die
	}
	r.fan = asusFanRPM()
	return r
}

// thermalZoneTemp: hottest ACPI thermal zone in °C, or -1. Desktop boards
// often expose none ("Not supported"); laptops usually do.
func thermalZoneTemp() int {
	best := -1
	rows, _ := wmiQuery(nsWMI, "SELECT CurrentTemperature FROM MSAcpi_ThermalZoneTemperature", "CurrentTemperature")
	for _, row := range rows {
		if v := row["CurrentTemperature"]; v.ok {
			best = max(best, deciKelvinToC(v.Int()))
		}
	}
	if best >= 0 {
		return best
	}
	rows, _ = wmiQuery(nsCIMV2, "SELECT HighPrecisionTemperature, Temperature FROM Win32_PerfFormattedData_Counters_ThermalZoneInformation",
		"HighPrecisionTemperature", "Temperature")
	for _, row := range rows {
		if v := row["HighPrecisionTemperature"]; v.ok && v.Int() > 0 {
			best = max(best, deciKelvinToC(v.Int()))
		} else if v := row["Temperature"]; v.ok && v.Int() > 0 {
			best = max(best, deciKelvinToC(v.Int()*10))
		}
	}
	return best
}

// deciKelvinToC converts tenths of a kelvin; implausible values become -1.
func deciKelvinToC(dk int64) int {
	c := int(float64(dk)/10 - 273.15 + 0.5)
	if c <= 0 || c >= 150 {
		return -1
	}
	return c
}

// storageTemps: temperature per physical disk in PhysicalDrive/DeviceId order.
// First the storage temperature IOCTL, which NVMe drives answer for any user;
// then, when that yields nothing, the Storage namespace's S.M.A.R.T. counters,
// which need administrator rights.
func storageTemps() []int {
	var out []int
	for n := 0; n < 32; n++ {
		t, exists := driveTempIOCTL(n)
		if !exists {
			continue
		}
		if t > 0 {
			out = append(out, t)
		}
	}
	if len(out) > 0 {
		return out
	}
	return storageTempsWMI()
}

var procDeviceIoControl = kernel32.NewProc("DeviceIoControl")

// driveTempIOCTL asks \\.\PhysicalDriveN for StorageDeviceTemperatureProperty.
// Opening with no access rights needs no privilege. exists is false when there
// is no such drive; temp is -1 when the drive or its driver has no answer.
func driveTempIOCTL(n int) (temp int, exists bool) {
	p, _ := syscall.UTF16PtrFromString(`\\.\PhysicalDrive` + strconv.Itoa(n))
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return -1, err != syscall.ERROR_FILE_NOT_FOUND && err != syscall.ERROR_PATH_NOT_FOUND
	}
	defer syscall.CloseHandle(h)
	const ioctlStorageQueryProperty = 0x2D1400
	const storageDeviceTemperatureProperty = 52
	query := [3]uint32{storageDeviceTemperatureProperty, 0 /*PropertyStandardQuery*/, 0}
	var out [4096]byte
	var got uint32
	r, _, _ := procDeviceIoControl.Call(uintptr(h), ioctlStorageQueryProperty,
		uintptr(unsafe.Pointer(&query[0])), uintptr(unsafe.Sizeof(query)),
		uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)), uintptr(unsafe.Pointer(&got)), 0)
	if r == 0 || got < 16 {
		return -1, true
	}
	return parseStorageTemperature(out[:got]), true
}

// parseStorageTemperature reads a STORAGE_TEMPERATURE_DATA_DESCRIPTOR:
// Version, Size, CriticalTemperature, WarningTemperature, InfoCount, 12
// reserved bytes (24-byte header), then 16-byte STORAGE_TEMPERATURE_INFO
// entries whose Temperature is at +2. Returns the first plausible sensor,
// normally the composite one; -1 if none.
func parseStorageTemperature(b []byte) int {
	const hdr, entry = 24, 16
	if len(b) < hdr {
		return -1
	}
	count := int(binary.LittleEndian.Uint32(b[12:]))
	for i := 0; i < count && hdr+(i+1)*entry <= len(b); i++ {
		t := int(int16(binary.LittleEndian.Uint16(b[hdr+i*entry+2:])))
		if t > 0 && t < 150 {
			return t
		}
	}
	return -1
}

// storageTempsWMI: the Storage provider's reliability counters, reachable only
// through each disk's association (a plain SELECT on the class returns
// nothing) and only with administrator rights.
func storageTempsWMI() []int {
	disks, err := wmiQuery(nsStorage, "SELECT * FROM MSFT_PhysicalDisk", "DeviceId", "__RELPATH")
	if err != nil {
		return nil
	}
	type dt struct{ id, t int }
	var ds []dt
	for _, d := range disks {
		rel := d["__RELPATH"].Str()
		if rel == "" {
			continue
		}
		rows, _ := wmiQuery(nsStorage, "ASSOCIATORS OF {"+rel+"} WHERE ResultClass=MSFT_StorageReliabilityCounter", "Temperature")
		for _, row := range rows {
			if t := row["Temperature"]; t.ok && t.Int() > 0 && t.Int() < 150 {
				id, _ := strconv.Atoi(strings.TrimSpace(d["DeviceId"].Str()))
				ds = append(ds, dt{id, int(t.Int())})
			}
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].id < ds[j].id })
	out := make([]int, len(ds))
	for i, d := range ds {
		out[i] = d.t
	}
	return out
}

// ---------- GPU ---------------------------------------------------------------

type gpuDesc struct {
	name       string
	memTotalMB int
}

var (
	gpuOnce sync.Once
	gpuDet  gpuDesc
)

// gpuStatic finds the display adapter once: Win32_VideoController for the
// name, the display class key's qwMemorySize for the real dedicated VRAM
// (AdapterRAM is a uint32 and caps at 4 GB).
func gpuStatic() gpuDesc {
	gpuOnce.Do(func() {
		rows, _ := wmiQuery(nsCIMV2, "SELECT Name, AdapterRAM, ConfigManagerErrorCode FROM Win32_VideoController",
			"Name", "AdapterRAM", "ConfigManagerErrorCode")
		gpuDet = pickGPU(rows)
		if gpuDet.name != "" {
			if mb := registryVRAM(gpuDet.name); mb > 0 {
				gpuDet.memTotalMB = mb
			}
		}
	})
	return gpuDet
}

// pickGPU: the first working, non-virtual adapter; ties broken by AdapterRAM.
func pickGPU(rows []wmiRow) gpuDesc {
	var best gpuDesc
	bestRAM := int64(-1)
	for _, row := range rows {
		name := strings.TrimSpace(row["Name"].Str())
		if name == "" || isVirtualGPU(name) {
			continue
		}
		if e := row["ConfigManagerErrorCode"]; e.ok && e.Int() != 0 {
			continue
		}
		ram := row["AdapterRAM"].Int()
		if ram < 0 { // CIM uint32 arrives as a signed VT_I4
			ram = int64(uint32(ram))
		}
		if ram > bestRAM {
			best, bestRAM = gpuDesc{name: name, memTotalMB: int(ram / (1024 * 1024))}, ram
		}
	}
	return best
}

func isVirtualGPU(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"microsoft basic", "virtual", "remote", "parsec", "displaylink", "meta ", "idd"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// registryVRAM reads HardwareInformation.qwMemorySize (bytes) from the display
// adapter class instance whose DriverDesc matches name; 0 when absent.
func registryVRAM(name string) int {
	const cls = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
	for _, sub := range regSubkeys(cls) {
		if len(sub) != 4 { // instances are "0000".."9999"; skip "Properties"
			continue
		}
		k := cls + `\` + sub
		if !strings.EqualFold(regString(k, "DriverDesc", ""), name) {
			continue
		}
		if v, ok := regUint64(k, "HardwareInformation.qwMemorySize"); ok && v > 0 {
			return int(v / (1024 * 1024))
		}
	}
	return 0
}

// regUint64 reads a REG_QWORD / REG_DWORD / 4- or 8-byte REG_BINARY value.
func regUint64(path, name string) (uint64, bool) {
	p, _ := syscall.UTF16PtrFromString(path)
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &h) != nil {
		return 0, false
	}
	defer syscall.RegCloseKey(h)
	n, _ := syscall.UTF16PtrFromString(name)
	var typ, size uint32
	if syscall.RegQueryValueEx(h, n, nil, &typ, nil, &size) != nil || size == 0 || size > 8 {
		return 0, false
	}
	buf := make([]byte, 8)
	if syscall.RegQueryValueEx(h, n, nil, &typ, &buf[0], &size) != nil {
		return 0, false
	}
	return *(*uint64)(unsafe.Pointer(&buf[0])), true
}

// gpuPerf: (load %, dedicated VRAM in use MB) from the GPU performance
// counters, the same source as Task Manager. The adapter with the most
// dedicated memory in use is taken as the active one; load is the busiest
// engine type across all of that adapter's processes. -1 when unavailable.
func gpuPerf() (load, usedMB int) {
	load, usedMB = -1, -1
	mem, _ := wmiQuery(nsCIMV2, "SELECT Name, DedicatedUsage FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory",
		"Name", "DedicatedUsage")
	luid, used := pickAdapterMemory(mem)
	if used >= 0 {
		usedMB = int(used / (1024 * 1024))
	}
	eng, _ := wmiQuery(nsCIMV2, "SELECT Name, UtilizationPercentage FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUEngine",
		"Name", "UtilizationPercentage")
	load = gpuLoadFrom(eng, luid)
	return
}

// pickAdapterMemory returns the LUID ("luid_0x..._0x...") and DedicatedUsage
// bytes of the adapter using the most dedicated memory; ("", -1) if none.
func pickAdapterMemory(rows []wmiRow) (string, int64) {
	luid, best := "", int64(-1)
	for _, row := range rows {
		if u := row["DedicatedUsage"].Int(); u > best {
			best = u
			luid = row["Name"].Str()
			if i := strings.Index(luid, "_phys"); i > 0 {
				luid = luid[:i]
			}
		}
	}
	return luid, best
}

// gpuLoadFrom sums UtilizationPercentage per engine type for the given LUID
// (all adapters when luid is "") and returns the busiest type, capped at 100.
func gpuLoadFrom(rows []wmiRow, luid string) int {
	if len(rows) == 0 {
		return -1
	}
	perType := map[string]int64{}
	for _, row := range rows {
		name := row["Name"].Str()
		if luid != "" && !strings.Contains(name, luid) {
			continue
		}
		et := name
		if i := strings.LastIndex(name, "_engtype_"); i >= 0 {
			et = name[i:]
		}
		perType[et] += row["UtilizationPercentage"].Int()
	}
	best := int64(-1)
	for _, v := range perType {
		best = max(best, v)
	}
	if best < 0 {
		return -1
	}
	return int(min(best, 100))
}
