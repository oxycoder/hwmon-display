//go:build windows

// Login autostart on Windows. The CPU temperature and package power come
// through the ASUS firmware's ASUSHW methods, which Windows opens to
// administrators only, and an HKCU Run entry always launches unelevated, so a
// Run-key autostart shows "--" for both. The real route is a Task Scheduler
// logon task with RunLevel HighestAvailable: it starts hwmond elevated at
// login without a UAC prompt. Creating such a task itself needs an elevated
// process, so a non-elevated `-autostart on` (or the tray toggle) relaunches
// hwmond through the UAC "runas" verb to do it; if the user declines, the
// Run key is written instead and a warning explains what will be missing.

package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const taskName = "hwmond"

// uacPrompt lets tests turn off the self-elevation (which would otherwise pop
// a UAC dialog) and exercise the Run-key fallback directly.
var uacPrompt = true

// setAutostart registers (or removes) hwmond at login. Elevated: a
// HighestAvailable logon task, replacing any older Run entry. Not elevated:
// relaunch elevated to create the task; on refusal fall back to the Run key.
func setAutostart(enable bool) error {
	if !enable {
		errTask := deleteTask()
		errKey := setRunKey(false)
		if errTask != nil {
			return errTask
		}
		return errKey
	}
	if isAdmin() {
		if err := createTask(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "autostart: Task Scheduler logon task %q registered with highest privileges\n", taskName)
		return setRunKey(false) // one launcher only; the task supersedes the key
	}
	if uacPrompt {
		err := relaunchElevated(append([]string{"-autostart", "on"}, loginArgs...))
		if err == nil {
			return nil
		}
		fmt.Fprintln(os.Stderr, "autostart: elevation declined or failed:", err)
	}
	if err := setRunKey(true); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "autostart: registered without administrator rights (Run key); CPU temperature and CPU power will show \"--\" at login. Run `hwmond -autostart on` from an elevated prompt to register a highest-privilege logon task instead.")
	return nil
}

// autostartEnabled reports whether either launcher is registered.
func autostartEnabled() bool {
	return taskExists() || runKeyEnabled()
}

// ---------- Task Scheduler via schtasks.exe -------------------------------

func schtasks() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "schtasks.exe")
}

// runSchtasks runs schtasks with a hidden window and returns its combined output.
func runSchtasks(args ...string) (string, error) {
	cmd := exec.Command(schtasks(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func taskExists() bool {
	_, err := runSchtasks("/Query", "/TN", taskName)
	return err == nil
}

func deleteTask() error {
	if !taskExists() {
		return nil
	}
	if out, err := runSchtasks("/Delete", "/TN", taskName, "/F"); err != nil {
		return fmt.Errorf("schtasks /Delete: %v: %s", err, out)
	}
	return nil
}

// createTask registers (replacing) the logon task from a task XML written
// to a temp file; the XML form is the only way schtasks sets "no time limit"
// (the command line default kills the task after 72 hours) and the battery
// settings.
func createTask() error {
	exe, err := exePath()
	if err != nil {
		return err
	}
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("current user: %v", err)
	}
	return importTask(taskXML(exe, loginArgs, u.Username))
}

// importTask registers (replacing) the task from its XML definition.
func importTask(x string) error {
	f, err := os.CreateTemp("", "hwmond-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(utf16BOM(x)); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if out, err := runSchtasks("/Create", "/TN", taskName, "/XML", f.Name(), "/F"); err != nil {
		return fmt.Errorf("schtasks /Create: %v: %s", err, out)
	}
	return nil
}

// taskXML is the Task Scheduler 1.2 definition: run at this user's logon
// with the highest available privileges, interactively, forever, on battery
// too, one instance at a time.
func taskXML(exe string, args []string, userName string) string {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var quoted []string
	for _, a := range args {
		quoted = append(quoted, quoteArg(a))
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>hwmon-display daemon: starts at login with administrator rights so the firmware CPU sensors (temperature, package power) are readable.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + esc(userName) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(userName) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(exe) + `</Command>
      <Arguments>` + esc(strings.Join(quoted, " ")) + `</Arguments>
      <WorkingDirectory>` + esc(filepath.Dir(exe)) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

// utf16BOM encodes s as UTF-16LE with a byte-order mark, the encoding
// schtasks /XML expects.
func utf16BOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		b[2+2*i], b[3+2*i] = byte(c), byte(c>>8)
	}
	return b
}

// ---------- UAC self-elevation: ShellExecuteEx "runas" -----------------------

var procShellExecuteEx = shell32.NewProc("ShellExecuteExW")

type shellExecuteInfo struct {
	size        uint32
	mask        uint32
	hwnd        uintptr
	verb        *uint16
	file        *uint16
	parameters  *uint16
	directory   *uint16
	show        int32
	instApp     uintptr
	idList      uintptr
	class       *uint16
	keyClass    uintptr
	hotKey      uint32
	iconMonitor uintptr
	process     syscall.Handle
}

// relaunchElevated runs this binary again with args through the UAC prompt
// and waits for it, returning its exit status (or the user's refusal).
func relaunchElevated(args []string) error {
	exe, err := exePath()
	if err != nil {
		return err
	}
	var quoted []string
	for _, a := range args {
		quoted = append(quoted, quoteArg(a))
	}
	const seeMaskNoCloseProcess = 0x40
	const swHide = 0
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess, show: swHide}
	info.size = uint32(unsafe.Sizeof(info))
	info.verb, _ = syscall.UTF16PtrFromString("runas")
	info.file, _ = syscall.UTF16PtrFromString(exe)
	info.parameters, _ = syscall.UTF16PtrFromString(strings.Join(quoted, " "))
	info.directory, _ = syscall.UTF16PtrFromString(filepath.Dir(exe))
	if r, _, e := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return fmt.Errorf("ShellExecuteEx: %v", e)
	}
	if info.process == 0 {
		return fmt.Errorf("ShellExecuteEx: no process handle")
	}
	defer syscall.CloseHandle(info.process)
	if _, err := syscall.WaitForSingleObject(info.process, syscall.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := syscall.GetExitCodeProcess(info.process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("elevated hwmond exited with status %d", code)
	}
	return nil
}
