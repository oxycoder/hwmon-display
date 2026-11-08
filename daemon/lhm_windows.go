//go:build windows

package main

// LibreHardwareMonitor web-server JSON (Options → Remote Web Server → Run,
// default port 8085). Optional: only consulted for readings WMI cannot supply
// (CPU temperature on boards without an ACPI thermal zone, fan RPM, GPU
// temperature, disk temperatures without admin rights, CPU package power,
// GPU power when the vendor library has none). HWMON_LHM_URL overrides the
// URL; "off" disables the fallback entirely.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var lhmURL = envOr("HWMON_LHM_URL", "http://localhost:8085/data.json")

// ---------- LibreHardwareMonitor JSON ------------------------------------------

type lhmNode struct {
	Text       string    `json:"Text"`
	Value      string    `json:"Value"`
	ImageURL   string    `json:"ImageURL"`
	HardwareId string    `json:"HardwareId"`
	Children   []lhmNode `json:"Children"`
}

// lhmReading is what the LHM tree yields; -1 (or 0 for fan) means "not found".
type lhmReading struct {
	cpuTemp, fan int
	cpuDie       bool // cpuTemp is the CPU's own die sensor (Tctl/Tdie, Package), not a board thermistor
	diskTemps    []int
	gpuName      string
	gpuTemp      int
	gpuLoad      int
	gpuMemUsed   int
	gpuMemTotal  int
	gpuPower     int // board power, W
	cpuPower     int // package power, W
	at           time.Time
}

var (
	lhmCache     lhmReading
	lhmClient    = &http.Client{Timeout: 800 * time.Millisecond}
	lhmRetryAt   time.Time
	lhmBackoff   = 30 * time.Second
	lhmAvailable bool
)

// lhm fetches LHM's JSON at most once per tick. When the server isn't there
// it returns an empty reading and only retries every lhmBackoff, so a machine
// without LHM pays one refused connection per 30 s rather than per second.
func lhm() lhmReading {
	if time.Since(lhmCache.at) < 900*time.Millisecond {
		return lhmCache
	}
	r := lhmReading{cpuTemp: -1, fan: 0, gpuTemp: -1, gpuLoad: -1, gpuMemUsed: -1, gpuMemTotal: -1, gpuPower: -1, cpuPower: -1, at: time.Now()}
	if strings.EqualFold(lhmURL, "off") || time.Now().Before(lhmRetryAt) {
		lhmCache = r
		return r
	}
	resp, err := lhmClient.Get(lhmURL)
	if err != nil {
		lhmAvailable = false
		lhmRetryAt = time.Now().Add(lhmBackoff)
		lhmCache = r
		return r
	}
	defer resp.Body.Close()
	var root lhmNode
	if json.NewDecoder(resp.Body).Decode(&root) == nil {
		lhmAvailable = true
		r.cpuTemp, r.cpuDie = findCPUTemp(&root)
		r.cpuPower = findCPUPower(&root)
		r.fan = findFan(&root)
		findDiskTemps(&root, &r.diskTemps)
		findGPU(&root, &r)
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

// findCPUTemp: the CPU temperature in °C and whether it is the CPU's own die
// sensor. -1 when LHM has none.
func findCPUTemp(root *lhmNode) (temp int, die bool) {
	// 1. The CPU's own die sensor, as LHM, the BIOS and Ryzen Master show it:
	//    AMD "Core (Tctl/Tdie)", Intel "CPU Package", then averages. Matched in
	//    preference order, so "Core (Tctl/Tdie)" beats "CCD1 (Tdie)".
	var nodes []*lhmNode
	cpuNodes(root, &nodes)
	model := strings.ToLower(cpuModel())
	for pass := 0; pass < 2; pass++ {
		for _, n := range nodes {
			name := strings.ToLower(n.Text)
			if pass == 0 && !(strings.Contains(model, name) || strings.Contains(name, firstWords(model, 3))) {
				continue
			}
			if v := tempOf(n, []string{"Tctl/Tdie", "Tctl", "Package", "Tdie", "Core Average", "CPU Core", "Core"}); v >= 0 {
				return v, true
			}
		}
	}

	// 2. Fallback: the motherboard Super I/O / EC sensor named "CPU". On AM4
	//    boards that is the CPUTIN socket thermistor, which lags and reads
	//    10-15 °C below Tctl, so it only serves when the CPU node is absent.
	if v := findSuperIOCPUTemp(root); v >= 0 {
		return v, false
	}
	return -1, false
}

// findCPUPower: the CPU package power in W from the CPU node's "Powers" group
// (AMD "Package", Intel "CPU Package"; else the cores' rail), or -1.
func findCPUPower(root *lhmNode) int {
	var nodes []*lhmNode
	cpuNodes(root, &nodes)
	model := strings.ToLower(cpuModel())
	for pass := 0; pass < 2; pass++ {
		for _, n := range nodes {
			name := strings.ToLower(n.Text)
			if pass == 0 && !(strings.Contains(model, name) || strings.Contains(name, firstWords(model, 3))) {
				continue
			}
			if v := powerOf(n, []string{"Package", "Cores", "Core"}); v >= 0 {
				return v
			}
		}
	}
	return -1
}

// powerOf: the node's "Powers" group → the first preference that names a
// sensor with a value, else the largest one. -1 without a group or values.
func powerOf(n *lhmNode, prefs []string) int {
	for i := range n.Children {
		if n.Children[i].Text != "Powers" {
			continue
		}
		sensors := n.Children[i].Children
		for _, p := range prefs {
			for _, s := range sensors {
				if strings.Contains(s.Text, p) {
					if v := numOf(s.Value); v >= 0 {
						return v
					}
				}
			}
		}
		best := -1
		for _, s := range sensors {
			if v := numOf(s.Value); v > best {
				best = v
			}
		}
		return best
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

// tempOf: the node's "Temperatures" group → the first preference (in priority
// order) that names a sensor with a value, else the hottest sensor.
func tempOf(n *lhmNode, prefs []string) int {
	for i := range n.Children {
		if n.Children[i].Text != "Temperatures" {
			continue
		}
		sensors := n.Children[i].Children
		for _, p := range prefs {
			for _, s := range sensors {
				if strings.Contains(s.Text, p) {
					if v := numOf(s.Value); v >= 0 {
						return v
					}
				}
			}
		}
		best := -1
		for _, s := range sensors {
			if v := numOf(s.Value); v > best {
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
	// Whole-board power first: AMD/NVIDIA "GPU Package", Intel "GPU Power";
	// AMD's PPT limit sensor and the core rail only when nothing else is there.
	out.gpuPower = powerOf(gpuNode, []string{"GPU Package", "GPU Power", "GPU PPT", "GPU Core"})
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
		return int(val*1024 + 0.5)
	}
	return int(val + 0.5)
}
