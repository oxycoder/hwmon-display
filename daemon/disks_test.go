package main

import "testing"

func TestDiskListLimits(t *testing.T) {
	var d diskList
	const gb = 1000 * 1000 * 1000
	if !d.add("/", 620*gb, 931*gb) || d.String() != "/:66:931" {
		t.Fatalf("root: %q", d.String())
	}
	// delimiters stripped, label cut to 6 chars
	d.add("My USB: stick,2", 5*gb, 64*gb)
	if d.String() != "/:66:931,MyUSBs:7:64" {
		t.Fatalf("label: %q", d.String())
	}
	d.add("Windows", 700*gb, 931*gb)
	d.add("home", 400*gb, 1863*gb)
	if !d.add("fifth", 1, 10*gb) == false || len(d.parts) != 4 {
		t.Fatalf("expected 4 disks max, got %d: %q", len(d.parts), d.String())
	}
	if n := len(d.String()); n > maxDiskValue {
		t.Fatalf("DISK value %d bytes > %d", n, maxDiskValue)
	}
	var e diskList
	if e.add("x", 1, 0) {
		t.Fatal("zero-size volume accepted")
	}
}

func TestDiskListByteCap(t *testing.T) {
	var d diskList
	const gb = 1000 * 1000 * 1000
	for i := 0; i < 4; i++ {
		d.add("abcdef", 99999*gb, 99999*gb) // "abcdef:100:99999" = 16 bytes each
	}
	if n := len(d.String()); n > maxDiskValue {
		t.Fatalf("DISK value %d bytes > %d: %q", n, maxDiskValue, d.String())
	}
}
