package main

import (
	"os"
	"reflect"
	"testing"
)

func TestWithPort(t *testing.T) {
	cases := []struct {
		in   []string
		port string
		want []string
	}{
		{[]string{"-tray"}, "COM7", []string{"-tray", "-port", "COM7"}},
		{[]string{"-tray", "-port", "COM3", "-web", "x"}, "COM7", []string{"-tray", "-web", "x", "-port", "COM7"}},
		{[]string{"-tray", "-port", "COM3"}, "auto", []string{"-tray"}},
		{[]string{"-port", "COM3"}, "auto", []string{}},
	}
	for _, c := range cases {
		if got := withPort(c.in, c.port); !reflect.DeepEqual(got, c.want) {
			t.Errorf("withPort(%v, %q) = %v, want %v", c.in, c.port, got, c.want)
		}
	}
}

func TestSortPorts(t *testing.T) {
	got := sortPorts([]string{"COM10", "COM3", "COM3", "COM1", "/dev/ttyUSB0", "/dev/ttyACM1", "/dev/ttyACM0"})
	want := []string{"/dev/ttyACM0", "/dev/ttyACM1", "/dev/ttyUSB0", "COM1", "COM3", "COM10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortPorts = %v, want %v", got, want)
	}
}

// TestSelectPort checks the run-time switch: the selection, the login args
// and the open port (closed so the main loop reconnects) all follow it, and
// the menu keeps a selected port that the OS no longer lists.
func TestSelectPort(t *testing.T) {
	defer func() { wantPort.Store("auto"); loginArgs = nil; curPort.Store(nil); portSwitched.Store(false) }()
	wantPort.Store("auto")
	loginArgs = []string{"-tray"}
	f, err := os.CreateTemp(t.TempDir(), "port")
	if err != nil {
		t.Fatal(err)
	}
	curPort.Store(f)

	selectPort("NOSUCHPORT9")
	if selectedPort() != "NOSUCHPORT9" {
		t.Fatalf("selectedPort = %q", selectedPort())
	}
	if want := []string{"-tray", "-port", "NOSUCHPORT9"}; !reflect.DeepEqual(loginArgs, want) {
		t.Errorf("loginArgs = %v, want %v", loginArgs, want)
	}
	if curPort.Load() != nil {
		t.Error("current port still set after the switch")
	}
	if !portSwitched.Load() {
		t.Error("portSwitched not raised")
	}
	if _, err := f.WriteString("x"); err == nil {
		t.Error("old port still writable: the main loop would not reconnect")
	}

	ch := portChoices()
	if ch[0].Name != "auto" {
		t.Errorf("first choice %+v, want auto", ch[0])
	}
	found := false
	for _, c := range ch {
		found = found || c.Name == "NOSUCHPORT9"
	}
	if !found {
		t.Errorf("selected port missing from choices %+v", ch)
	}

	// Selecting the same port again is a no-op (nothing to close, no flag).
	portSwitched.Store(false)
	selectPort("NOSUCHPORT9")
	if portSwitched.Load() {
		t.Error("re-selecting the current port raised portSwitched")
	}
	t.Logf("serial ports here: %v", listSerialPorts())
}
