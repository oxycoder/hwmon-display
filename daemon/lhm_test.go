//go:build windows

package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestLHMParse(t *testing.T) {
	b, err := os.ReadFile("testdata/lhm.json")
	if err != nil {
		t.Fatal(err)
	}
	var root lhmNode
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	if got, die := findCPUTemp(&root); got != 68 || !die { // not the "Virtual" node's 25
		t.Errorf("cpuTemp = %d, %v, want 68, true", got, die)
	}
	if got := findFan(&root); got != 1180 { // "CPU Fan", not "Chassis Fan #1"
		t.Errorf("fan = %d, want 1180", got)
	}
	var dt []int
	findDiskTemps(&root, &dt)
	if len(dt) != 1 || dt[0] != 38 {
		t.Errorf("diskTemps = %v, want [38]", dt)
	}
}

// temps builds a hardware node with one "Temperatures" group.
func temps(text, hwid, img string, sensors ...string) lhmNode {
	g := lhmNode{Text: "Temperatures"}
	for i := 0; i+1 < len(sensors); i += 2 {
		g.Children = append(g.Children, lhmNode{Text: sensors[i], Value: sensors[i+1]})
	}
	return lhmNode{Text: text, HardwareId: hwid, ImageURL: img, Children: []lhmNode{g}}
}

// TestLHMCPUTempPrefersDie mirrors a ROG Crosshair VIII Hero + Ryzen 9 3900X:
// the Super I/O "CPU" thermistor (46.5) comes first in the tree and reads far
// below Tctl (63). The die sensor must win, and "Core (Tctl/Tdie)" must beat
// "CCD1 (Tdie)" even when listed after it.
func TestLHMCPUTempPrefersDie(t *testing.T) {
	board := lhmNode{Text: "ASUS ROG CROSSHAIR VIII HERO (WI-FI)", HardwareId: "/motherboard", ImageURL: "images_icon/mainboard.png",
		Children: []lhmNode{
			temps("Nuvoton NCT6798D", "/lpc/nct6798d/0", "images_icon/chip.png", "CPU", "46.5 °C", "Motherboard", "40.0 °C", "Virtual", "61.0 °C"),
			temps("Embedded Controller", "/lpc/ec", "images_icon/chip.png", "Chipset", "61.0 °C", "CPU", "51.0 °C"),
		}}
	cpu := temps("AMD Ryzen 9 3900X", "/amdcpu/0", "images_icon/cpu.png",
		"CCD1 (Tdie)", "68.8 °C", "CCD2 (Tdie)", "56.5 °C", "Core (Tctl/Tdie)", "63.0 °C", "CCDs Average (Tdie)", "62.6 °C")
	root := lhmNode{Text: "Sensor", Children: []lhmNode{{Text: "TUNG", Children: []lhmNode{board, cpu}}}}

	if got, die := findCPUTemp(&root); got != 63 || !die {
		t.Errorf("cpuTemp = %d, %v, want 63, true (Tctl/Tdie)", got, die)
	}

	// Without a CPU node, the board's "CPU" sensor is all there is, flagged as not-die.
	root.Children[0].Children = []lhmNode{board}
	if got, die := findCPUTemp(&root); got != 47 || die {
		t.Errorf("cpuTemp = %d, %v, want 47, false (Super I/O fallback)", got, die)
	}

	// Intel naming.
	intel := temps("Intel Core i7-12700K", "/intelcpu/0", "images_icon/cpu.png",
		"CPU Core #1", "40.0 °C", "CPU Core #2", "42.0 °C", "CPU Package", "45.0 °C", "Core Average", "41.0 °C")
	root.Children[0].Children = []lhmNode{board, intel}
	if got, die := findCPUTemp(&root); got != 45 || !die {
		t.Errorf("cpuTemp = %d, %v, want 45, true (CPU Package)", got, die)
	}
}

// powers builds a hardware node with one "Powers" group.
func powers(text, hwid, img string, sensors ...string) lhmNode {
	g := lhmNode{Text: "Powers"}
	for i := 0; i+1 < len(sensors); i += 2 {
		g.Children = append(g.Children, lhmNode{Text: sensors[i], Value: sensors[i+1]})
	}
	return lhmNode{Text: text, HardwareId: hwid, ImageURL: img, Children: []lhmNode{g}}
}

// TestLHMPower: CPU package power from the CPU node (AMD "Package", Intel
// "CPU Package", never the per-core rails), GPU board power from the GPU
// node's "GPU Package" rather than the core rail or the PPT limit.
func TestLHMPower(t *testing.T) {
	amd := powers("AMD Ryzen 9 3900X", "/amdcpu/0", "images_icon/cpu.png",
		"Core #1 (SMU)", "12.3 W", "Package", "88.4 W", "Core (SMU)", "61.0 W")
	gpu := powers("AMD Radeon RX 5600 XT", "/gpu-amd/0", "images_icon/ati.png",
		"GPU Core", "95.2 W", "GPU PPT", "160.0 W", "GPU Package", "142.6 W")
	root := lhmNode{Text: "Sensor", Children: []lhmNode{{Text: "TUNG", Children: []lhmNode{amd, gpu}}}}

	if got := findCPUPower(&root); got != 88 {
		t.Errorf("cpuPower = %d, want 88 (Package)", got)
	}
	var r lhmReading
	findGPU(&root, &r)
	if r.gpuPower != 143 {
		t.Errorf("gpuPower = %d, want 143 (GPU Package)", r.gpuPower)
	}

	intel := powers("Intel Core i7-12700K", "/intelcpu/0", "images_icon/cpu.png",
		"CPU Package", "65.5 W", "CPU Cores", "50.1 W", "CPU Graphics", "1.2 W", "CPU Memory", "4.0 W")
	nv := powers("NVIDIA GeForce RTX 4070", "/gpu-nvidia/0", "images_icon/nvidia.png", "GPU Package", "199.7 W")
	root.Children[0].Children = []lhmNode{intel, nv}
	if got := findCPUPower(&root); got != 66 {
		t.Errorf("cpuPower = %d, want 66 (CPU Package)", got)
	}
	r = lhmReading{}
	findGPU(&root, &r)
	if r.gpuPower != 200 {
		t.Errorf("gpuPower = %d, want 200", r.gpuPower)
	}

	// No Powers group anywhere: -1, not 0.
	root.Children[0].Children = []lhmNode{temps("Intel Core i7-12700K", "/intelcpu/0", "images_icon/cpu.png", "CPU Package", "45.0 °C")}
	if got := findCPUPower(&root); got != -1 {
		t.Errorf("cpuPower without a Powers group = %d, want -1", got)
	}
}

func TestPickCPUTemp(t *testing.T) {
	cases := []struct {
		native    int
		nativeDie bool
		l         lhmReading
		want      int
		wantDie   bool
	}{
		{63, true, lhmReading{cpuTemp: 64, cpuDie: true}, 63, true},    // native Tctl needs no LHM
		{51, false, lhmReading{cpuTemp: 63, cpuDie: true}, 63, true},   // LHM die beats the socket thermistor
		{51, false, lhmReading{cpuTemp: 47, cpuDie: false}, 51, false}, // both board sensors: keep the native one
		{51, false, lhmReading{cpuTemp: -1}, 51, false},                // no LHM
		{-1, false, lhmReading{cpuTemp: 47, cpuDie: false}, 47, false}, // nothing native: any LHM reading
		{-1, false, lhmReading{cpuTemp: -1}, -1, false},
	}
	for _, c := range cases {
		if got, die := pickCPUTemp(c.native, c.nativeDie, c.l); got != c.want || die != c.wantDie {
			t.Errorf("pickCPUTemp(%d, %v, %+v) = %d, %v, want %d, %v", c.native, c.nativeDie, c.l, got, die, c.want, c.wantDie)
		}
	}
}

// TestRAPL: the Zen 2 power-unit MSR reads 0xA1003 (ESU 16 -> 15.26 µJ per
// step); 6,553,600 steps in one second is 100 W; a counter rollover unwraps.
func TestRAPL(t *testing.T) {
	j := raplJoulesPerStep(0xA1003)
	if j != 1.0/65536 {
		t.Fatalf("joules per step = %g, want 1/65536", j)
	}
	if got := raplJoulesPerStep(0); got != 0 {
		t.Errorf("ESU 0 -> %g, want 0 (unavailable)", got)
	}
	cases := []struct {
		prev, cur uint32
		dt        float64
		want      int
	}{
		{1000, 1000 + 6553600, 1.0, 100},
		{1000, 1000 + 6553600, 2.0, 50},
		{0xFFFFFF00, 0x00000100 + 6553600 - 0x100, 1.0, 100}, // wrapped
		{1000, 1000, 1.0, -1},                                // no energy consumed: implausible
		{1000, 2000, 0.01, -1},                               // interval too short
		{1000, 1000 + 6553600*30, 1.0, -1},                   // 3 kW: implausible
	}
	for _, c := range cases {
		if got := raplWatts(c.prev, c.cur, j, c.dt); got != c.want {
			t.Errorf("raplWatts(%d, %d, %.2fs) = %d, want %d", c.prev, c.cur, c.dt, got, c.want)
		}
	}
}

// TestTctlOf decodes THM_TCON_CUR_TMP values captured on a Ryzen 9 3900X
// through the ASUS firmware's PCI config read (0x6FDB0F6F = 62.75 °C).
func TestTctlOf(t *testing.T) {
	cases := map[uint32]int{
		0x6FDB0F6F:                63,  // 0x37E * 0.125 = 111.75, CUR_TEMP_RANGE_SEL set: minus 49
		0x6FDB0F6F &^ (1 << 19):   112, // same raw value with the range bit clear
		0x00000000:                0,
		(0x7FF << 21) | (1 << 19): 207, // ceiling
	}
	for v, want := range cases {
		if got := tctlOf(v); got != want {
			t.Errorf("tctlOf(%08X) = %d, want %d", v, got, want)
		}
	}
}
