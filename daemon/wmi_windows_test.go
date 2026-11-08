//go:build windows

package main

import (
	"strconv"
	"testing"
	"time"
)

func TestWMIQuery(t *testing.T) {
	rows, err := wmiQuery(`root\CIMV2`, "SELECT Caption, BuildNumber FROM Win32_OperatingSystem", "Caption", "BuildNumber")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["Caption"].Str() == "" || rows[0]["BuildNumber"].Int() < 10000 {
		t.Fatalf("rows = %+v", rows)
	}
	t.Logf("os: %s build %d", rows[0]["Caption"].Str(), rows[0]["BuildNumber"].Int())

	// Unknown class must report an error, not hang or crash.
	if _, err := wmiQuery(`root\CIMV2`, "SELECT X FROM No_Such_Class", "X"); err == nil {
		t.Error("expected error for unknown class")
	}
	// Bad namespace likewise.
	if _, err := wmiQuery(`root\NoSuchNamespace`, "SELECT X FROM Y", "X"); err == nil {
		t.Error("expected error for unknown namespace")
	}
}

func TestWMISensors(t *testing.T) {
	start := time.Now()
	s := wmiSample()
	t.Logf("wmi sample in %v: %+v", time.Since(start), s)
	start = time.Now()
	r := sample()
	t.Logf("merged sample in %v: cpuTemp=%d fan=%d gpu=%q gpuTemp=%d gpuLoad=%d vram=%d/%d disks=%v",
		time.Since(start), r.cpuTemp, r.fan, r.gpuName, r.gpuTemp, r.gpuLoad, r.gpuMemUsed, r.gpuMemTotal, r.diskTemps)
	if g := gpuInfo(); g != "" {
		t.Logf("gpuInfo: %s", g)
	}
}

func row(kv ...any) wmiRow {
	r := wmiRow{}
	for i := 0; i+1 < len(kv); i += 2 {
		switch v := kv[i+1].(type) {
		case string: // like the BSTR path: numbers in text are also parsed
			n, _ := strconv.ParseInt(v, 10, 64)
			r[kv[i].(string)] = wmiValue{s: v, n: n, ok: true}
		case int:
			r[kv[i].(string)] = intVal(int64(v))
		case int64:
			r[kv[i].(string)] = intVal(v)
		}
	}
	return r
}

func TestPickGPU(t *testing.T) {
	// AdapterRAM is CIM uint32 but arrives as VT_I4: 4 GB wraps negative.
	rows := []wmiRow{
		row("Name", "Microsoft Basic Display Adapter", "AdapterRAM", 0, "ConfigManagerErrorCode", 0),
		row("Name", "AMD Radeon RX 5600 XT", "AdapterRAM", -1048576, "ConfigManagerErrorCode", 0),
		row("Name", "NVIDIA GeForce RTX 4090", "AdapterRAM", 2147483647, "ConfigManagerErrorCode", 43),
	}
	g := pickGPU(rows)
	if g.name != "AMD Radeon RX 5600 XT" || g.memTotalMB != 4095 {
		t.Errorf("pickGPU = %+v", g)
	}
	if pickGPU(nil).name != "" {
		t.Error("expected no GPU for no rows")
	}
}

func TestGPULoadFrom(t *testing.T) {
	rows := []wmiRow{
		row("Name", "pid_1_luid_0x00000000_0x0001_phys_0_eng_0_engtype_3D", "UtilizationPercentage", 30),
		row("Name", "pid_2_luid_0x00000000_0x0001_phys_0_eng_0_engtype_3D", "UtilizationPercentage", 25),
		row("Name", "pid_2_luid_0x00000000_0x0001_phys_0_eng_1_engtype_VideoDecode", "UtilizationPercentage", 10),
		row("Name", "pid_3_luid_0x00000000_0x0002_phys_0_eng_0_engtype_3D", "UtilizationPercentage", 90),
	}
	if got := gpuLoadFrom(rows, "luid_0x00000000_0x0001"); got != 55 { // 3D engines summed, decode ignored
		t.Errorf("load(adapter 1) = %d, want 55", got)
	}
	if got := gpuLoadFrom(rows, ""); got != 100 { // all adapters, capped
		t.Errorf("load(all) = %d, want 100", got)
	}
	if got := gpuLoadFrom(rows, "luid_0x00000000_0x0009"); got != -1 {
		t.Errorf("load(absent adapter) = %d, want -1", got)
	}
	if got := gpuLoadFrom(nil, ""); got != -1 {
		t.Errorf("load(no rows) = %d, want -1", got)
	}
	mem := []wmiRow{
		row("Name", "luid_0x00000000_0x0002_phys_0", "DedicatedUsage", "0"),
		row("Name", "luid_0x00000000_0x0001_phys_0", "DedicatedUsage", "1454686208"), // uint64 comes as text
	}
	luid, used := pickAdapterMemory(mem)
	if luid != "luid_0x00000000_0x0001" || used != 1454686208 {
		t.Errorf("pickAdapterMemory = %q, %d", luid, used)
	}
}

func TestDeciKelvinToC(t *testing.T) {
	for dk, want := range map[int64]int{3182: 45, 2732: -1, 0: -1, 4731: -1} {
		if got := deciKelvinToC(dk); got != want {
			t.Errorf("deciKelvinToC(%d) = %d, want %d", dk, got, want)
		}
	}
}

func TestParseStorageTemperature(t *testing.T) {
	// As returned by a Samsung 970 EVO Plus: Version=40 Size=72 Critical=85
	// Warning=85 InfoCount=3, reserved, then sensors 52/52/58 °C.
	b := []byte{0x28, 0, 0, 0, 0x48, 0, 0, 0, 0x55, 0, 0x55, 0, 3, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0x34, 0, 0x55, 0, 0, 0x80, 1, 1, 1, 0, 0, 0, 0, 0,
		1, 0, 0x34, 0, 0xee, 0xfe, 0, 0x80, 1, 1, 1, 0, 0, 0, 0, 0,
		2, 0, 0x3a, 0, 0xee, 0xfe, 0, 0x80, 1, 1, 1, 0, 0, 0, 0, 0}
	if got := parseStorageTemperature(b); got != 52 {
		t.Errorf("temp = %d, want 52", got)
	}
	b[26] = 0 // composite sensor unknown → next one
	if got := parseStorageTemperature(b); got != 52 {
		t.Errorf("temp = %d, want 52 from sensor 1", got)
	}
	b[42] = 0
	if got := parseStorageTemperature(b); got != 58 {
		t.Errorf("temp = %d, want 58 from sensor 2", got)
	}
	if got := parseStorageTemperature(b[:8]); got != -1 {
		t.Errorf("short = %d, want -1", got)
	}
}

func TestFanRPMOf(t *testing.T) {
	for _, c := range []struct{ hi, lo, want int }{
		{4, 215, 1239}, {3, 184, 952}, {255, 31, 0}, {255, 255, 0}, {0, 0, 0}, {-1, 5, 0}, {0, 50, 0},
	} {
		if got := fanRPMOf(c.hi, c.lo); got != c.want {
			t.Errorf("fanRPMOf(%d,%d) = %d, want %d", c.hi, c.lo, got, c.want)
		}
	}
}

// TestASUSHW only reports: the class needs administrator rights.
func TestASUSHW(t *testing.T) {
	p := asusHW()
	t.Logf("ASUSHW path=%q admin=%v", p, isAdmin())
	if p == "" {
		t.Skip("ASUSHW unavailable (not an ASUS board, or not elevated)")
	}
	// Low byte first: after a bank-0x80 read the chip keeps answering the high byte at 0x4F.
	lo, hi := sioRead(p, sioVendorLo), sioRead(p, sioVendorHi)
	temp, die := asusCPUTemp()
	t.Logf("vendor=%02X%02X chip=%02X cpuTemp=%d die=%v tctl=%d cputin=%d fan=%d",
		hi, lo, sioRead(p, 0x058), temp, die, amdTctl(p), sioRead(p, sioCPUTIN), asusFanRPM())
	if temp < 0 {
		t.Error("expected a CPU temperature")
	}
	if v, ok := pciRead(p, 0, 4); ok && v&0xFFFF == pciVendorAMD {
		// Repeated reads from different goroutines (hence OS threads) must
		// all succeed: the Global\Access_PCI mutex is thread-owned.
		for i := 0; i < 8; i++ {
			done := make(chan int)
			go func() { done <- amdTctl(p) }()
			if got := <-done; !plausibleTemp(got) {
				t.Errorf("Tctl read %d: got %d (SMN read or mutex failed)", i, got)
			}
		}
		if !die {
			t.Error("AMD root complex but asusCPUTemp did not return Tctl")
		}
	}
}
