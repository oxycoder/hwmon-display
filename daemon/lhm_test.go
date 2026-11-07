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
	if got := findCPUTemp(&root); got != 68 { // not the "Virtual" node's 25
		t.Errorf("cpuTemp = %d, want 68", got)
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
