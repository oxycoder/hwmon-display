//go:build windows

package main

import (
	"encoding/xml"
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

// TestTrayIconRetry starts the tray while NIM_ADD is blocked, as at logon
// when the task launches hwmond before explorer.exe has a taskbar: startTray
// must still succeed (window up, no icon), and the retry timer must add the
// icon once the shell accepts it.
func TestTrayIconRetry(t *testing.T) {
	loginArgs = []string{"-tray"}
	setStatus("unit test")
	trayAddBlocked.Store(true)
	defer trayAddBlocked.Store(false)
	if err := startTray(); err != nil {
		t.Fatal(err)
	}
	h := trayHwnd.Load()
	if h == 0 {
		t.Fatal("no tray window")
	}
	if notify(nimModify, h) {
		t.Fatal("icon present although NIM_ADD was blocked")
	}
	trayAddBlocked.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for !notify(nimModify, h) {
		if time.Now().After(deadline) {
			t.Fatal("icon not added by the retry timer within 5s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !notify(nimDelete, h) {
		t.Fatal("NIM_DELETE failed")
	}
}

// TestAutostartRegistry registers and removes the login entry. Unelevated (the
// UAC relaunch is turned off) enabling must refuse and register nothing, not
// fall back to the unelevated Run key; elevated it registers the
// highest-privilege scheduled task. It changes the current user's startup list
// (restored afterwards), so it runs only with HWMOND_AUTOSTART_TEST=1.
func TestAutostartRegistry(t *testing.T) {
	if os.Getenv("HWMOND_AUTOSTART_TEST") != "1" {
		t.Skip("set HWMOND_AUTOSTART_TEST=1 to exercise the Run key / scheduled task")
	}
	uacPrompt = false
	defer func() { uacPrompt = true }()
	// Put back exactly what the user had: the Run value text and the task XML
	// (re-registering through setAutostart would point them at this test binary).
	origRun := runValueString()
	origTask, origTaskErr := runSchtasks("/Query", "/TN", taskName, "/XML")
	defer func() {
		if err := setAutostart(false); err != nil {
			t.Error("cleanup:", err)
		}
		if origRun != "" {
			if err := restoreRunValue(origRun); err != nil {
				t.Error("cleanup:", err)
			}
		}
		if origTaskErr == nil {
			if err := importTask(origTask); err != nil {
				t.Error("cleanup:", err)
			}
		}
	}()
	loginArgs = []string{"-tray", "-port", "COM3"}
	if err := setAutostart(false); err != nil { // start from a clean slate
		t.Fatal(err)
	}
	err := setAutostart(true)
	if !isAdmin() {
		if err == nil || !strings.Contains(err.Error(), "administrator") {
			t.Fatalf("unelevated enable: err = %v, want the needs-administrator refusal", err)
		}
		if autostartEnabled() || runKeyEnabled() || taskExists() {
			t.Fatalf("unelevated enable registered something: task=%v runKey=%v", taskExists(), runKeyEnabled())
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !autostartEnabled() {
		t.Fatal("entry not found after enabling")
	}
	if !taskExists() || runKeyEnabled() {
		t.Fatalf("elevated: task=%v runKey=%v, want the task only", taskExists(), runKeyEnabled())
	}
	out, err := runSchtasks("/Query", "/TN", taskName, "/XML")
	if err != nil || !strings.Contains(out, "<RunLevel>HighestAvailable</RunLevel>") || !strings.Contains(out, "-tray -port COM3") {
		t.Fatalf("task XML: %v\n%s", err, out)
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

// TestEmbeddedIcon checks that rsrc_windows_*.syso (icon/gen.go) linked an
// icon group with id 1 that LoadImage can resolve at tray and large sizes.
func TestEmbeddedIcon(t *testing.T) {
	hinst, _, _ := procGetModuleHandle.Call(0)
	for _, sz := range []uintptr{16, 32, 48, 256} {
		if h, _, _ := procLoadImage.Call(hinst, 1, 1 /*IMAGE_ICON*/, sz, sz, 0); h == 0 {
			t.Errorf("LoadImage(icon 1, %dpx) failed: resource not linked?", sz)
		}
	}
}

// TestTaskXML checks the scheduled-task definition parses as XML, escapes the
// path and user, and carries the settings the Run key could not express.
func TestTaskXML(t *testing.T) {
	x := taskXML(`C:\Tools & Co\hwmond.exe`, []string{"-tray", "-port", "COM 3"}, `PC\tung`)
	var task struct {
		Triggers struct {
			Logon struct {
				UserId string `xml:"UserId"`
			} `xml:"LogonTrigger"`
		}
		Principals struct {
			P struct {
				RunLevel  string
				LogonType string
			} `xml:"Principal"`
		}
		Settings struct {
			ExecutionTimeLimit      string
			MultipleInstancesPolicy string
		}
		Actions struct {
			Exec struct {
				Command   string
				Arguments string
			}
		}
	}
	// The declared UTF-16 is how schtasks gets the file; the Go decoder sees UTF-8.
	plain := strings.Replace(x, ` encoding="UTF-16"`, "", 1)
	if err := xml.Unmarshal([]byte(plain), &task); err != nil {
		t.Fatalf("xml: %v\n%s", err, x)
	}
	if task.Actions.Exec.Command != `C:\Tools & Co\hwmond.exe` || task.Actions.Exec.Arguments != `-tray -port "COM 3"` {
		t.Fatalf("exec = %q %q", task.Actions.Exec.Command, task.Actions.Exec.Arguments)
	}
	if task.Triggers.Logon.UserId != `PC\tung` || task.Principals.P.RunLevel != "HighestAvailable" || task.Principals.P.LogonType != "InteractiveToken" {
		t.Fatalf("principal: %+v %+v", task.Triggers.Logon, task.Principals.P)
	}
	if task.Settings.ExecutionTimeLimit != "PT0S" || task.Settings.MultipleInstancesPolicy != "IgnoreNew" {
		t.Fatalf("settings: %+v", task.Settings)
	}
	if b := utf16BOM("A"); len(b) != 4 || b[0] != 0xFF || b[1] != 0xFE || b[2] != 'A' || b[3] != 0 {
		t.Fatalf("utf16BOM = %x", b)
	}
}

// restoreRunValue writes cmd back as the hwmond Run value.
func restoreRunValue(cmd string) error {
	key, _ := syscall.UTF16PtrFromString(runKey)
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, key, 0, syscall.KEY_READ|0x0002, &h); err != nil {
		return err
	}
	defer syscall.RegCloseKey(h)
	return writeRunValue(h, cmd)
}

// TestRegValues enumerates a DEVICEMAP key that always exists (VIDEO), the
// same code listSerialPorts uses on SERIALCOMM, which is absent when no COM
// device is plugged in.
func TestRegValues(t *testing.T) {
	vals := regValues(`HARDWARE\DEVICEMAP\VIDEO`)
	if len(vals) == 0 {
		t.Fatal("no values under the DEVICEMAP VIDEO key")
	}
	t.Logf("%d values, first %q; serial ports: %v", len(vals), vals[0], listSerialPorts())
}
