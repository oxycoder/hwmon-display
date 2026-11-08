//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestTrayIcon creates the real tray icon, updates its tooltip and removes it.
func TestTrayIcon(t *testing.T) {
	loginArgs = []string{"-tray"}
	setStatus("unit test")
	if err := startTray(); err != nil {
		t.Fatal(err)
	}
	h := trayHwnd.Load()
	if h == 0 {
		t.Fatal("no tray window")
	}
	setStatus("display on COM3")
	time.Sleep(200 * time.Millisecond) // let the tray thread handle the refresh
	if !notify(nimModify, h) {
		t.Fatal("NIM_MODIFY failed")
	}
	if !notify(nimDelete, h) {
		t.Fatal("NIM_DELETE failed")
	}
}

// TestAutostartRegistry writes and removes the HKCU Run entry. It changes the
// current user's startup list (restored afterwards), so it runs only with
// HWMOND_AUTOSTART_TEST=1.
func TestAutostartRegistry(t *testing.T) {
	if os.Getenv("HWMOND_AUTOSTART_TEST") != "1" {
		t.Skip("set HWMOND_AUTOSTART_TEST=1 to exercise the Run key")
	}
	was := autostartEnabled()
	defer setAutostart(was)
	loginArgs = []string{"-tray", "-port", "COM3"}
	if err := setAutostart(true); err != nil {
		t.Fatal(err)
	}
	if !autostartEnabled() {
		t.Fatal("entry not found after enabling")
	}
	exe, _ := exePath()
	if v := runValueString(); !strings.Contains(v, exe) || !strings.HasSuffix(v, "-tray -port COM3") {
		t.Fatalf("Run value = %q", v)
	}
	if err := setAutostart(false); err != nil {
		t.Fatal(err)
	}
	if autostartEnabled() {
		t.Fatal("entry still present after disabling")
	}
	if err := setAutostart(false); err != nil { // idempotent
		t.Fatal(err)
	}
}

func runValueString() string {
	key, _ := syscall.UTF16PtrFromString(runKey)
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, key, 0, syscall.KEY_READ, &h) != nil {
		return ""
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString(runValue)
	var typ, size uint32
	if syscall.RegQueryValueEx(h, name, nil, &typ, nil, &size) != nil {
		return ""
	}
	buf := make([]uint16, size/2+1)
	syscall.RegQueryValueEx(h, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size)
	return strings.TrimRight(syscall.UTF16ToString(buf), "\x00")
}
