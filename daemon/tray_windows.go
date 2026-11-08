//go:build windows

// Tray icon (Shell_NotifyIcon on a message-only window, no cgo) and login
// autostart via HKCU\Software\Microsoft\Windows\CurrentVersion\Run.

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	user32                = syscall.NewLazyDLL("user32.dll")
	shell32               = syscall.NewLazyDLL("shell32.dll")
	advapi32              = syscall.NewLazyDLL("advapi32.dll")
	procRegisterClassEx   = user32.NewProc("RegisterClassExW")
	procCreateWindowEx    = user32.NewProc("CreateWindowExW")
	procDefWindowProc     = user32.NewProc("DefWindowProcW")
	procGetMessage        = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessage   = user32.NewProc("DispatchMessageW")
	procPostMessage       = user32.NewProc("PostMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procRegisterWindowMsg = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenu        = user32.NewProc("AppendMenuW")
	procTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	procDestroyMenu       = user32.NewProc("DestroyMenu")
	procSetForegroundWnd  = user32.NewProc("SetForegroundWindow")
	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procCreateIcon        = user32.NewProc("CreateIcon")
	procLoadIcon          = user32.NewProc("LoadIconW")
	procShellNotifyIcon   = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle   = kernel32.NewProc("GetModuleHandleW")
	procFreeConsole       = kernel32.NewProc("FreeConsole")
	procGetConsoleProcs   = kernel32.NewProc("GetConsoleProcessList")
	procRegCreateKeyEx    = advapi32.NewProc("RegCreateKeyExW")
	procRegSetValueEx     = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValue    = advapi32.NewProc("RegDeleteValueW")
)

const (
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmEndSession  = 0x0016 // wParam != 0: the session really is ending
	wmPowerBcast  = 0x0218 // WM_POWERBROADCAST
	pbtSuspend    = 0x0004 // PBT_APMSUSPEND: the PC is going to sleep now
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmApp         = 0x8000
	wmTrayEvent   = wmApp + 1 // Shell_NotifyIcon callback
	wmTrayRefresh = wmApp + 2 // status changed: update tooltip

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfSeparator = 0x0800
	tpmReturn   = 0x0100
	tpmRightBtn = 0x0002
	tpmBottom   = 0x0020

	idStatus    = 1
	idAutostart = 2
	idQuit      = 3
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

var (
	trayHwnd       atomic.Uintptr
	trayIcon       uintptr
	taskbarCreated uintptr // broadcast when explorer.exe restarts: re-add the icon
	trayWndProcCB  = syscall.NewCallback(trayWndProc)
)

// startTray creates the icon on its own OS thread and returns once it is
// visible (or creation failed). The message loop runs for the process lifetime.
func startTray() error {
	hideOwnConsole()
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		hinst, _, _ := procGetModuleHandle.Call(0)
		cls, _ := syscall.UTF16PtrFromString("hwmondTray")
		wc := wndClassEx{LpfnWndProc: trayWndProcCB, HInstance: hinst, LpszClassName: cls}
		wc.CbSize = uint32(unsafe.Sizeof(wc))
		if r, _, e := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			done <- fmt.Errorf("RegisterClassEx: %v", e)
			return
		}
		tc, _ := syscall.UTF16PtrFromString("TaskbarCreated")
		taskbarCreated, _, _ = procRegisterWindowMsg.Call(uintptr(unsafe.Pointer(tc)))
		// A hidden top-level window (parent 0, no WS_VISIBLE) rather than a
		// HWND_MESSAGE one: message-only windows never get the WM_ENDSESSION
		// broadcast, and that is how a tray-only process learns the OS is
		// shutting down so it can put the display into standby.
		hwnd, _, e := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)),
			0, 0, 0, 0, 0, 0, 0, hinst, 0)
		if hwnd == 0 {
			done <- fmt.Errorf("CreateWindowEx: %v", e)
			return
		}
		trayIcon = makeIcon(hinst)
		if !notify(nimAdd, hwnd) {
			done <- fmt.Errorf("Shell_NotifyIcon failed")
			return
		}
		trayHwnd.Store(hwnd)
		done <- nil
		var m msg
		for {
			r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	return <-done
}

// trayStatusChanged asks the tray thread to refresh the tooltip.
func trayStatusChanged() {
	if h := trayHwnd.Load(); h != 0 {
		procPostMessage.Call(h, wmTrayRefresh, 0, 0)
	}
}

// notify adds/updates/removes the icon; the tooltip is the current status.
func notify(action uintptr, hwnd uintptr) bool {
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmTrayEvent
	nid.HIcon = trayIcon
	tip, _ := syscall.UTF16FromString("hwmond: " + getStatus())
	copy(nid.SzTip[:len(nid.SzTip)-1], tip)
	r, _, _ := procShellNotifyIcon.Call(action, uintptr(unsafe.Pointer(&nid)))
	return r != 0
}

func trayWndProc(hwnd uintptr, m uint32, wParam, lParam uintptr) uintptr {
	switch {
	case m == wmTrayEvent && (lParam == wmRButtonUp || lParam == wmLButtonUp):
		showMenu(hwnd)
		return 0
	case m == wmTrayRefresh:
		notify(nimModify, hwnd)
		return 0
	case m == wmClose:
		trayQuit(hwnd)
		return 0
	case m == wmEndSession && wParam != 0:
		sendStandby() // shutdown / logoff: the display would otherwise wait out its silence timeout
		return 0
	case m == wmPowerBcast && wParam == pbtSuspend:
		sendStandby() // sleep / hibernate: dark now, not a minute later; the main loop reconnects on resume
		return 1
	case m == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	case taskbarCreated != 0 && uintptr(m) == taskbarCreated:
		notify(nimAdd, hwnd)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(hwnd, uintptr(m), wParam, lParam)
	return r
}

func showMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	add := func(flags uintptr, id uintptr, text string) {
		t, _ := syscall.UTF16PtrFromString(text)
		procAppendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(t)))
	}
	add(mfString|mfGrayed, idStatus, "hwmond: "+getStatus())
	add(mfSeparator, 0, "")
	auto := uintptr(mfString)
	if autostartEnabled() {
		auto |= mfChecked
	}
	add(auto, idAutostart, "Start on login")
	add(mfString, idQuit, "Quit")

	var pt [2]int32
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWnd.Call(hwnd) // so the menu closes when clicking elsewhere
	id, _, _ := procTrackPopupMenu.Call(menu, tpmReturn|tpmRightBtn|tpmBottom, uintptr(pt[0]), uintptr(pt[1]), 0, hwnd, 0)
	procPostMessage.Call(hwnd, 0, 0, 0) // WM_NULL: menu-dismiss quirk (KB 135788)
	procDestroyMenu.Call(menu)
	switch id {
	case idAutostart:
		toggleAutostart()
	case idQuit:
		trayQuit(hwnd)
	}
}

func trayQuit(hwnd uintptr) {
	notify(nimDelete, hwnd)
	sendStandby()
	os.Exit(0)
}

// hookShutdown registers a console control handler so hwmond run from a
// terminal (or with -tray=false) also parks the display on Ctrl-C / window
// close / logoff / shutdown. Go's os/signal only maps the first two to
// os.Interrupt; with -tray the console is usually gone and WM_ENDSESSION on
// the tray window covers shutdown instead.
func hookShutdown() {
	procSetConsoleCtrlHandler.Call(consoleCtrlCB, 1)
}

var (
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	consoleCtrlCB             = syscall.NewCallback(func(ctrlType uint32) uintptr {
		// CTRL_C 0, CTRL_BREAK 1, CTRL_CLOSE 2, CTRL_LOGOFF 5, CTRL_SHUTDOWN 6
		sendStandby()
		if ctrlType >= 2 { // the system kills the process when this returns; exit on our terms
			os.Exit(0)
		}
		return 0 // Ctrl-C/Break: let os/signal's handler finish the job
	})
)

var (
	procLoadImage        = user32.NewProc("LoadImageW")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
)

// makeIcon loads the application icon embedded by rsrc_windows_*.syso (group
// icon id 1, see icon/gen.go) at the tray's small-icon size. Without the
// resource it draws the same motif in memory: a rounded frame with three
// meter bars (green / amber / cyan); last resort is the stock application icon.
func makeIcon(hinst uintptr) uintptr {
	const smCxSmIcon, smCySmIcon, imageIcon = 49, 50, 1
	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	if h, _, _ := procLoadImage.Call(hinst, 1, imageIcon, cx, cy, 0); h != 0 {
		return h
	}
	return drawIcon(hinst)
}

func drawIcon(hinst uintptr) uintptr {
	const n = 32
	xor := make([]byte, n*n*4) // BGRA
	and := make([]byte, n*n/8) // 1bpp mask, all opaque (alpha from BGRA wins)
	set := func(x, y int, b, g, r byte) {
		if x < 0 || y < 0 || x >= n || y >= n {
			return
		}
		i := (y*n + x) * 4
		xor[i], xor[i+1], xor[i+2], xor[i+3] = b, g, r, 255
	}
	for y := 2; y < n-2; y++ {
		for x := 2; x < n-2; x++ {
			edge := x == 2 || x == n-3 || y == 2 || y == n-3
			corner := (x <= 3 || x >= n-4) && (y <= 3 || y >= n-4)
			if corner && (x == 2 || x == n-3) && (y == 2 || y == n-3) {
				continue // rounded corners
			}
			if edge {
				set(x, y, 0xc8, 0xc8, 0xc8) // light grey frame
			} else {
				set(x, y, 0x30, 0x1c, 0x12) // dark navy screen
			}
		}
	}
	bars := []struct {
		y, w    int
		b, g, r byte
	}{{7, 18, 0x50, 0xd0, 0x40}, {13, 12, 0x30, 0xa8, 0xf0}, {19, 22, 0xe0, 0xb0, 0x20}}
	for _, br := range bars {
		for dy := 0; dy < 4; dy++ {
			for x := 6; x < 6+br.w; x++ {
				set(x, br.y+dy, br.b, br.g, br.r)
			}
		}
	}
	h, _, _ := procCreateIcon.Call(hinst, n, n, 1, 32, uintptr(unsafe.Pointer(&and[0])), uintptr(unsafe.Pointer(&xor[0])))
	if h == 0 {
		h, _, _ = procLoadIcon.Call(0, 32512) // IDI_APPLICATION
	}
	return h
}

// hideOwnConsole closes the console window when hwmond is the only process
// attached to it (launched from Explorer / the Run key), but leaves a shell's
// terminal alone so `hwmond -tray` run by hand still shows its log.
func hideOwnConsole() {
	var pids [4]uint32
	if n, _, _ := procGetConsoleProcs.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids))); n == 1 {
		procFreeConsole.Call()
	}
}

// ---------- login autostart: HKCU ...\CurrentVersion\Run ---------------------

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "hwmond"

func setAutostart(enable bool) error {
	key, _ := syscall.UTF16PtrFromString(runKey)
	name, _ := syscall.UTF16PtrFromString(runValue)
	var h syscall.Handle
	const keySetValue = 0x0002
	if r, _, _ := procRegCreateKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(key)), 0, 0, 0,
		keySetValue|syscall.KEY_READ, 0, uintptr(unsafe.Pointer(&h)), 0); r != 0 {
		return fmt.Errorf("RegCreateKeyEx: error %d", r)
	}
	defer syscall.RegCloseKey(h)
	if !enable {
		r, _, _ := procRegDeleteValue.Call(uintptr(h), uintptr(unsafe.Pointer(name)))
		if r != 0 && r != 2 { // 2 = ERROR_FILE_NOT_FOUND: already off
			return fmt.Errorf("RegDeleteValue: error %d", r)
		}
		return nil
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	cmd := quoteArg(exe)
	for _, a := range loginArgs {
		cmd += " " + quoteArg(a)
	}
	val, _ := syscall.UTF16FromString(cmd)
	const regSZ = 1
	if r, _, _ := procRegSetValueEx.Call(uintptr(h), uintptr(unsafe.Pointer(name)), 0, regSZ,
		uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2)); r != 0 {
		return fmt.Errorf("RegSetValueEx: error %d", r)
	}
	return nil
}

func autostartEnabled() bool {
	key, _ := syscall.UTF16PtrFromString(runKey)
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, key, 0, syscall.KEY_READ, &h) != nil {
		return false
	}
	defer syscall.RegCloseKey(h)
	name, _ := syscall.UTF16PtrFromString(runValue)
	var typ, size uint32
	return syscall.RegQueryValueEx(h, name, nil, &typ, nil, &size) == nil
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
