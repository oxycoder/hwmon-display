//go:build windows

package main

// CPU temperature, fan RPM and CPU package power on ASUS boards without any
// kernel driver of our own: ASUS firmware exposes an ACPI-WMI class "ASUSHW" (root\WMI, GUID
// 466747a0-70ec-11de-8a39-0800200c9a66) whose asio_hw_fun21 method reads one
// register of the Nuvoton NCT67xx Super I/O hardware monitor; the ACPI code in
// the BIOS does the port I/O. It is the same interface the Linux
// asus-wmi-sensors driver family uses. Windows grants access to the class only
// to administrators, so this path lights up when hwmond runs elevated.
//
// Register map (NCT6779D and later, bank<<8 | index), as in the Linux nct6775
// and LibreHardwareMonitor drivers:
//   0x073  CPUTIN, °C           (ASUS's "CPU" sensor: the socket thermistor)
//   0x4A0  SMBUSMASTER 0, °C    (AMD TSI over SMBus on AM4/AM5; fallback)
//
// Neither register is the CPU's die sensor. On a Crosshair VIII Hero with a
// Ryzen 9 3900X, CPUTIN read 46.5 °C and SMBUSMASTER 0 read 50 °C while
// Tctl/Tdie was 63 °C: the socket reading lags and sits 10-15 °C below the
// die. The same class also exposes PCI configuration reads and writes
// (asio_hw_fun11/12), which is enough to read Tctl from the CPU itself; see
// amdTctl below. The Super I/O register is only the fallback.
//   0x4C0.. fan tachometers, 16-bit RPM big-endian: SYS, CPU, AUX0, AUX1, AUX2, AUX3, AUX4

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	sioVendorLo, sioVendorHi = 0x004F, 0x804F // Nuvoton: 0xA3 / 0x5C
	sioCPUTIN, sioSMBMaster0 = 0x073, 0x4A0
	sioFanCPU                = 0x4C2
)

var sioFanRegs = []uint16{0x4C2, 0x4C0, 0x4C4, 0x4C6, 0x4C8, 0x4CA, 0x4CE} // CPU fan first

var (
	asusOnce sync.Once
	asusPath string // __RELPATH of the ASUSHW instance; "" when unavailable
	asusSIO  bool   // a Nuvoton monitor answers behind asio_hw_fun21
)

// asusHW finds the ASUSHW instance once and checks whether a Nuvoton monitor
// answers behind it. Logs one line explaining why it is unavailable.
func asusHW() string {
	asusOnce.Do(func() {
		rows, err := wmiQuery(nsWMI, "SELECT * FROM ASUSHW", "__RELPATH")
		if err != nil || len(rows) == 0 || rows[0]["__RELPATH"].Str() == "" {
			if err != nil && !isAdmin() {
				fmt.Fprintln(os.Stderr, "asus-wmi: ASUSHW not accessible without administrator rights; CPU temp/fan need LibreHardwareMonitor or an elevated hwmond")
			}
			return
		}
		asusPath = rows[0]["__RELPATH"].Str()
		lo, hi := sioRead(asusPath, sioVendorLo), sioRead(asusPath, sioVendorHi)
		asusSIO = lo == 0xA3 && hi == 0x5C
		if !asusSIO {
			fmt.Fprintf(os.Stderr, "asus-wmi: unknown Super I/O vendor %02X%02X, not reading fans or CPUTIN from it\n", hi, lo)
		}
	})
	return asusPath
}

// sioRead reads one hardware-monitor register (bank<<8 | index); -1 on failure.
func sioRead(path string, reg uint16) int {
	row, err := wmiExecMethod(nsWMI, path, "asio_hw_fun21",
		map[string]int64{"Bank": int64(reg >> 8), "Index": int64(reg & 0xFF)}, "Data")
	// The ACPI-WMI mapper does not populate ReturnValue for these methods;
	// only check it when it is present.
	if err != nil || !row["Data"].ok || (row["ReturnValue"].ok && row["ReturnValue"].Int() == 0) {
		return -1
	}
	return int(row["Data"].Int())
}

// asusCPUTemp: CPU temperature in °C and whether it is the die sensor. The
// CPU's own Tctl when the firmware lets us reach it, else the Super I/O
// socket reading, else -1.
func asusCPUTemp() (temp int, die bool) {
	p := asusHW()
	if p == "" {
		return -1, false
	}
	if v := amdTctl(p); plausibleTemp(v) {
		return v, true
	}
	if asusSIO {
		for _, reg := range []uint16{sioCPUTIN, sioSMBMaster0} {
			if v := sioRead(p, reg); plausibleTemp(v) {
				return v, false
			}
		}
	}
	return -1, false
}

// ---------- AMD Tctl through PCI configuration space ----------------------------
//
// asio_hw_fun11 / asio_hw_fun12 read and write PCI configuration space (Width
// in bytes). On AMD Zen CPUs the thermal controller's THM_TCON_CUR_TMP register
// sits in the System Management Network at 0x00059800, reached through the
// index/data pair at offsets 0x60/0x64 of the root complex (bus 0, device 0,
// function 0): write the address to 0x60, read the value from 0x64. Bits 31:21
// hold the temperature in 0.125 °C steps and bit 19 flags a 49 °C offset.
// LibreHardwareMonitor and Ryzen Master run the same sequence through their
// kernel drivers; here the firmware does the port I/O. Everyone serialises the
// two-step access with the named mutex Global\Access_PCI, so we take it too.

const (
	pciVendorAMD  = 0x1022
	smnIndexReg   = 0x60
	smnDataReg    = 0x64
	smnThmCurTemp = 0x00059800
)

var (
	smnOnce sync.Once
	smnOK   bool
)

// amdTctl: Tctl in °C from the CPU's thermal controller, or -1 when this is
// not an AMD root complex or the read fails.
func amdTctl(path string) int {
	smnOnce.Do(func() {
		v, ok := pciRead(path, 0, 4)
		smnOK = ok && v&0xFFFF == pciVendorAMD
		if ok && !smnOK {
			fmt.Fprintf(os.Stderr, "asus-wmi: root complex vendor %04X is not AMD, no SMN Tctl; using the Super I/O CPU sensor\n", v&0xFFFF)
		}
	})
	if !smnOK {
		return -1
	}
	// A Win32 mutex belongs to the OS thread that acquired it; keep the
	// goroutine on one thread so the release comes from the same thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if !pciMutex.lock() {
		return -1
	}
	defer pciMutex.unlock()
	if !pciWrite(path, smnIndexReg, 4, smnThmCurTemp) {
		return -1
	}
	v, ok := pciRead(path, smnDataReg, 4)
	if !ok || v == 0xFFFFFFFF {
		return -1
	}
	return tctlOf(v)
}

// tctlOf decodes THM_TCON_CUR_TMP: CUR_TEMP in bits 31:21 (0.125 °C units),
// minus 49 °C when CUR_TEMP_RANGE_SEL (bit 19) is set. Rounded to whole °C.
func tctlOf(v uint32) int {
	t := float64(v>>21) * 0.125
	if v&(1<<19) != 0 {
		t -= 49
	}
	return int(t + 0.5)
}

// pciRead reads width bytes at reg of PCI 0:0.0.
func pciRead(path string, reg uint16, width int) (uint32, bool) {
	row, err := wmiExecMethod(nsWMI, path, "asio_hw_fun11",
		map[string]int64{"Bus": 0, "Dev": 0, "Func": 0, "Reg": int64(reg), "Width": int64(width)}, "dwData")
	if err != nil || !row["dwData"].ok {
		return 0, false
	}
	return uint32(row["dwData"].Int()), true // UInt32 arrives as signed VT_I4
}

// pciWrite writes width bytes of v at reg of PCI 0:0.0.
func pciWrite(path string, reg uint16, width int, v uint32) bool {
	_, err := wmiExecMethod(nsWMI, path, "asio_hw_fun12",
		map[string]int64{"Bus": 0, "Dev": 0, "Func": 0, "Reg": int64(reg), "Width": int64(width), "dwData": int64(v)})
	return err == nil
}

// namedMutex wraps a Win32 named mutex. lock waits at most 20 ms so a hung
// peer never stalls the sampler; an abandoned mutex still counts as acquired.
type namedMutex struct {
	name string
	once sync.Once
	h    uintptr
}

var pciMutex = &namedMutex{name: `Global\Access_PCI`}

var (
	procCreateMutexW        = kernel32.NewProc("CreateMutexW")
	procReleaseMutex        = kernel32.NewProc("ReleaseMutex")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
)

func (m *namedMutex) lock() bool {
	m.once.Do(func() {
		name, _ := syscall.UTF16PtrFromString(m.name)
		m.h, _, _ = procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	})
	if m.h == 0 {
		return true // no mutex to honour; proceed unserialised
	}
	const waitObject0, waitAbandoned = 0, 0x80
	r, _, _ := procWaitForSingleObject.Call(m.h, 20)
	return r == waitObject0 || r == waitAbandoned
}

func (m *namedMutex) unlock() {
	if m.h != 0 {
		procReleaseMutex.Call(m.h)
	}
}

// ---------- CPU package power through the RAPL energy MSRs ----------------------
//
// asio_hw_fun15 reads a model-specific register (Index in, DataHi/DataLo out).
// Both vendors keep a running package energy counter in an MSR, in units of
// 2^-ESU joules where ESU comes from the power-unit MSR: AMD Zen
// 0xC0010299 / 0xC001029B (the "Package" power LibreHardwareMonitor and the
// Linux amd_energy driver show), Intel 0x606 / 0x611. Power is the counter
// delta over the sampling interval; the counter is 32 bits wide and wraps
// roughly every ten minutes at 100 W, which uint32 subtraction unwraps.

const (
	msrAMDPowerUnit   = 0xC0010299
	msrAMDPkgEnergy   = 0xC001029B
	msrIntelPowerUnit = 0x606
	msrIntelPkgEnergy = 0x611
)

var (
	raplOnce      sync.Once
	raplJoules    float64 // joules per counter step; 0 when unavailable
	raplEnergyMSR uint32
	raplPrevRaw   uint32
	raplPrevAt    time.Time
	raplHave      bool
)

// msrRead reads one MSR through the firmware; false when the method fails.
func msrRead(path string, index uint32) (uint64, bool) {
	row, err := wmiExecMethod(nsWMI, path, "asio_hw_fun15", map[string]int64{"Index": int64(index)}, "DataHi", "DataLo")
	if err != nil || !row["DataHi"].ok || !row["DataLo"].ok {
		return 0, false
	}
	return uint64(uint32(row["DataHi"].Int()))<<32 | uint64(uint32(row["DataLo"].Int())), true
}

// asusCPUPower: CPU package power in W from the RAPL counters, or -1. The
// first call only primes the counter.
func asusCPUPower() int {
	p := asusHW()
	if p == "" {
		return -1
	}
	raplOnce.Do(func() {
		unitMSR, energyMSR := uint32(msrAMDPowerUnit), uint32(msrAMDPkgEnergy)
		vendor := regString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "VendorIdentifier", "")
		switch {
		case strings.Contains(vendor, "Intel"):
			unitMSR, energyMSR = msrIntelPowerUnit, msrIntelPkgEnergy
		case strings.Contains(vendor, "AMD"):
		default:
			fmt.Fprintf(os.Stderr, "asus-wmi: CPU vendor %q has no known energy MSRs; CPU power needs LibreHardwareMonitor\n", vendor)
			return
		}
		unit, ok := msrRead(p, unitMSR)
		raplJoules = raplJoulesPerStep(unit)
		if !ok || raplJoules == 0 {
			fmt.Fprintln(os.Stderr, "asus-wmi: energy unit MSR unreadable; CPU power needs LibreHardwareMonitor")
			raplJoules = 0
			return
		}
		raplEnergyMSR = energyMSR
		// Prime the counter so even the very first frame (-once) carries a value.
		if raw, ok := msrRead(p, energyMSR); ok {
			raplPrevRaw, raplPrevAt, raplHave = uint32(raw), time.Now(), true
			time.Sleep(250 * time.Millisecond)
		}
	})
	if raplJoules == 0 {
		return -1
	}
	raw, ok := msrRead(p, raplEnergyMSR)
	if !ok {
		return -1
	}
	now := time.Now()
	prevRaw, prevAt, had := raplPrevRaw, raplPrevAt, raplHave
	raplPrevRaw, raplPrevAt, raplHave = uint32(raw), now, true
	if !had {
		return -1
	}
	return raplWatts(prevRaw, uint32(raw), raplJoules, now.Sub(prevAt).Seconds())
}

// raplJoulesPerStep decodes the power-unit MSR: ESU in bits 12:8 gives
// 2^-ESU J per energy counter step. 0 for an implausible register.
func raplJoulesPerStep(unit uint64) float64 {
	esu := (unit >> 8) & 0x1F
	if esu == 0 {
		return 0
	}
	return 1 / float64(uint64(1)<<esu)
}

// raplWatts: average power between two 32-bit energy counter readings taken
// dt seconds apart, unwrapping one rollover; -1 when the interval is too short
// or the result is implausible.
func raplWatts(prev, cur uint32, joules, dt float64) int {
	if dt < 0.05 {
		return -1
	}
	w := float64(cur-prev) * joules / dt
	if w <= 0 || w > 2000 {
		return -1
	}
	return int(w + 0.5)
}

// asusFanRPM: the CPU fan's RPM, else the first other spinning fan, else 0.
func asusFanRPM() int {
	p := asusHW()
	if p == "" || !asusSIO {
		return 0
	}
	for _, reg := range sioFanRegs {
		if rpm := fanRPMOf(sioRead(p, reg), sioRead(p, reg+1)); rpm > 0 {
			return rpm
		}
	}
	return 0
}

var procIsUserAnAdmin = syscall.NewLazyDLL("shell32.dll").NewProc("IsUserAnAdmin")

// isAdmin reports whether the process token is elevated.
func isAdmin() bool {
	r, _, _ := procIsUserAnAdmin.Call()
	return r != 0
}

// fanRPMOf decodes a 16-bit tachometer (high byte first). An unconnected
// header reads 0xFFxx; anything that high, or negative bytes from a failed
// read, is "no fan".
func fanRPMOf(hi, lo int) int {
	if hi < 0 || lo < 0 || hi >= 0xFF {
		return 0
	}
	rpm := hi<<8 | lo
	if rpm < 100 || rpm > 20000 {
		return 0
	}
	return rpm
}
