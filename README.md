# hwmon — boot-time + OS-time hardware monitor on an external LCD

One small LCD shows hardware info from the moment the firmware runs (before any OS)
and keeps updating once the OS is up. Works with Linux, Windows, or no OS at all.

Two physical links are supported; the protocol and display code are the same for both.

```
A) USB-C (single cable, needs an MCU with native USB: ESP32-S3, RP2040/Pico)
 PC firmware (UEFI)  ──┐
   hwmon.efi           ├── USB (CDC-ACM, 115200) ──────────────────── ESP32-S3 + 2.8" LCD
 PC OS (Linux)        ─┘   "KEY=VAL|KEY=VAL\n"                          (power + data)
   hwmond (Go)

B) COM port (any MCU, e.g. classic ESP32 "Cheap Yellow Display")
 PC firmware (UEFI)  ──┐                          ┌─────────────┐
   hwmon.efi           ├── COM1 (16550, 115200) ──┤ MAX3232     ├── ESP32 + 2.8" LCD
 PC OS (Linux)        ─┘                          │ RS232→TTL   │   + USB for power
   hwmond (Go)                                    └─────────────┘
```

`hwmon.efi` always writes to the 16550 (COM1 or a PCIe serial card) *and* to any USB-serial
device it finds (CDC-ACM data interface, or FTDI — the latter is what QEMU emulates), so one
binary covers both builds.

## Layout

| Path | What | Status |
|---|---|---|
| `uefi/hwmon.c` | UEFI app (GNU-EFI, C). Reads CPUID, SMBIOS, memory map, PCI; writes to the 16550 directly and to USB-serial via `EFI_USB_IO_PROTOCOL`; chainloads the real bootloader. | **tested in QEMU/OVMF, serial + USB** |
| `uefi/usbio.h` | `EFI_USB_IO_PROTOCOL` definition (gnu-efi doesn't ship one). | |
| `daemon/` | OS daemon. `linux.go`: hwmon/procfs, every mounted block device from `/proc/mounts` (USB, NTFS Windows partitions). `windows.go`: kernel32 for load/RAM/drives (fixed + removable), COM port via SetCommState. `wmi_windows.go`: dependency-free WMI client over raw COM; `sensors_windows.go`: GPU name/load/VRAM, ACPI thermal zone and disk temperatures (storage IOCTL, else WMI S.M.A.R.T.); `gpuvendor_windows.go`: GPU temperature via the AMD ADL / NVIDIA NVML driver libraries; `asuswmi_windows.go`: CPU temperature + fan RPM on ASUS boards via the firmware.s ASUSHW ACPI-WMI Super I/O reads (elevated); with LibreHardwareMonitor (`lhm_windows.go`) as an optional fallback for what WMI lacks. `icon/gen.go`: draws the app icon and emits `hwmond.ico` plus the `rsrc_windows_*.syso` resource objects (hand-built COFF, no tools) that give `hwmond.exe` its icon. `tray_*.go`: tray icon + start-at-login (Windows: Shell_NotifyIcon; `autostart_windows.go`: highest-privilege Task Scheduler logon task via schtasks with UAC self-elevation, HKCU Run key as the non-elevated fallback, no deps; Linux: StatusNotifierItem over D-Bus via godbus + XDG autostart). `panel.go` + `panel.html`: the control panel, a loopback-only web UI / JSON API (device information from the ESP32, screen mode: hardware monitor / custom text page / any raw frame, one-shot commands, backlight), net/http only. | Linux **tested over a pty**; Windows tray/autostart/parser unit-tested on a real desktop |
| `display/core/` | Platform-independent parser + renderer (320×240). Shared by the simulator and the ESP32. | **tested** |
| `display/sim/` | Host simulator: reads a serial device/file, renders frames to PNG. | **tested** |
| `display/esp32_es3c28p/` | **The board in use.** Sketch + `build.sh` for the ESP32-S3 2.8" ES3C28P (ILI9341V, native USB). | syntax-checked; flash with `build.sh` |
| `display/esp32_cyd/` | Arduino sketch for the classic ESP32-2432S028 (COM build) + TFT_eSPI config. | syntax-checked |
| `test-e2e.sh` | Whole pipeline with zero hardware. | |

## Protocol

One line per frame, `\n`-terminated, must start with `SRC=`:

```
SRC=UEFI|CPU=...|THREADS=16|RAM_MB=32768|BOARD=...|BIOS=...|DIMMS=2|DIMM_MB=32768|DIMM_MHZ=6000|GPU=1002:744c|NIC=10ec:8125|STORAGE_CTRL=2
SRC=OS|HOST=tt-desktop|CPU=...|THREADS=16|OS=Ubuntu 24.04|LOAD=37|TEMP=68|FAN=1180|CPU_W=88|RAM_USED_MB=11850|RAM_MB=31900|DISK=/:62:931,home:40:1863,USB:5:64,Window:71:931|DISK_T=38,35|GPU=Radeon RX 7900 XTX|GPU_TEMP=61|GPU_LOAD=42|GPU_W=210|VRAM_USED_MB=7500|VRAM_MB=24576
```

Unknown keys are ignored; missing keys render as `--`. Anything not starting with `SRC=`
(firmware console echo, noise) is dropped. After 5 s of silence the display shows a NO SIGNAL banner.

**Standby.** USB 5 V usually stays on after the PC shuts down (and always while it sleeps), so the
display puts itself to sleep instead of showing NO SIGNAL forever: after 60 s without a frame
(`STANDBY_AFTER_MS` in the sketch) the backlight is cut, the ILI9341 enters sleep mode and the ESP32
drops to 80 MHz. The next data frame, from `hwmon.efi` at boot or from `hwmond` after resume, wakes it
and the page is redrawn before the backlight comes back. `hwmond` also sends one control frame,

```
SRC=CMD|STANDBY=1
```

when it quits (tray *Quit*, Ctrl-C, SIGTERM), when the OS shuts down or logs off (Windows:
`WM_ENDSESSION` on the tray window / console control events) and when the PC goes to sleep
(Windows: `WM_POWERBROADCAST` / `PBT_APMSUSPEND`; Linux: logind `PrepareForSleep` on the system
bus), so the screen goes dark at once instead of a minute later. On resume the next frame wakes it. `SRC=CMD` frames never replace the data on screen; the last readings
come back on wake.

**Control frames and replies.** `SRC=CMD` carries `STANDBY=1|0` (sleep / wake), `INFO=1` (send device
information) and `BL=0..100` (backlight brightness, PWM; the firmware forgets it on reset, `hwmond`
re-sends the last value when the display reconnects). The display is the only thing that ever talks
back, and only when asked: to `INFO=1` it answers one line on the same port,

```
SRC=DEV|FW=1.2|BOARD=ES3C28P|CHIP=ESP32-S3|REV=2|CORES=2|CPU_MHZ=240|FLASH_MB=16|PSRAM_KB=0|PSRAM_FREE_KB=0|HEAP_TOTAL_KB=320|HEAP_KB=140|HEAP_MIN_KB=120|SKETCH_KB=358|APP_FREE_KB=921|SDK=v5.5.1|CORE=3.3.12|UP_S=812|RX_BYTES=240113|FRAMES=801|SPRITE=1|BL=100|STANDBY=0|RESET=1|TEMP=41|MAC=F412FA123456
```

and sends the same line once whenever the USB host (re)connects. `hwmon.efi` never reads the port, so
the reply is harmless at boot. `hwmond` asks every 10 s while a display is connected and keeps the
last two hours (`GET /api/history`) for the control panel's charts: chip temperature and free heap
over time, link rate derived from `RX_BYTES`/`FRAMES`, and meters for the app partition
(`SKETCH_KB` / `SKETCH_KB + APP_FREE_KB`), heap (`HEAP_TOTAL_KB - HEAP_KB`) and PSRAM.
Open the panel with `?demo=1` to see the charts with made-up data before the board is flashed.

**Text page.** `SRC=TEXT` shows whatever the host wants instead of the hardware page:

```
SRC=TEXT|TITLE=Build status|L1=#ok:main - passing|L2=#hot:release - FAILED|L3=|L4=next deploy 18:00|SIZE=2|COLOR=text|ALIGN=center
```

`TITLE` (optional, accent colour) and `L1`..`L12` are the lines; a line may start with `#accent:`,
`#ok:`, `#warn:`, `#hot:`, `#dim:` or `#text:` to colour just that line, an empty line leaves a gap,
long lines wrap at the last space that fits. `SIZE` is the 6×8 font multiplier (1 = 52 chars per
line, 2 = 26, default; 3 = 17; 4 = 13), `COLOR` the default colour, `ALIGN` `left` (default) or
`center`. Like every data frame it has to keep coming (the sender repeats it every tick) or the
NO SIGNAL banner / standby kick in. Values are ASCII without `|` or `=`, at most 63 bytes each, the
whole line at most 511 bytes.

`CPU_W` / `GPU_W` are the CPU package and GPU board power in watts. The OS page shows them
as a third column next to temperature and load, and the footer shows their sum (`PWR 298W`)
next to the CPU fan. Linux: CPU power from the RAPL energy counters under
`/sys/class/powercap` (root-readable since kernel 5.10; `zenpower` as the fallback), GPU power
from the amdgpu hwmon node. Windows: GPU power from the vendor library (ADL `PMLog` ASIC power
or NVML); CPU package power from the RAPL energy MSRs (AMD `0xC001029B`, Intel `0x611`) read
through the ASUS firmware's `ASUSHW` MSR method, which needs an elevated `hwmond` like the
Tctl path; on other boards it comes from LibreHardwareMonitor when that is running, otherwise
the cell shows `--`.

`DISK` is `label:used%:GB`, up to four volumes, labels cut to 6 chars, root first then in mount
order. On Linux that is every mounted block-device filesystem (ext4/xfs/btrfs, ntfs/ntfs3/ntfs-3g,
exfat/vfat...) except `/boot*`, snaps, loop/zram and container overlays, so a plugged-in USB stick
(`/media/<user>/USB` → `USB`) or a mounted Windows partition (`/media/<user>/Windows` → `Window`)
appears as soon as the desktop mounts it. On Windows it is every fixed or removable drive letter
with media present. Hot-plug is picked up on the next frame; nothing needs restarting.

## Test with no hardware

```
sudo apt install gnu-efi qemu-system-x86 ovmf socat golang gcc
./test-e2e.sh            # COM-port link  → out/frame-0001-UEFI.png, out/frame-000N-OS.png
./test-e2e.sh usb        # USB link: QEMU xHCI + emulated USB-serial device
make -C uefi run-usb     # just the UEFI app over USB; frame lands in /tmp/usb.log
```

To try the display firmware on a *real* CYD before wiring the PC: flash the sketch, then
`cat display/sim/sample.txt > /dev/ttyUSB0` — the USB serial accepts the same protocol.

## Deploy on the real PC

**USB-C build (ES3C28P / ESP32-S3):**
```
brew install arduino-cli                                   # Windows: winget install ArduinoSA.CLI, then run build.sh from Git Bash with COM3 as the port
display/esp32_es3c28p/build.sh setup                       # once
display/esp32_es3c28p/build.sh && display/esp32_es3c28p/build.sh flash /dev/cu.usbmodemXXXX
display/esp32_es3c28p/build.sh test /dev/cu.usbmodemXXXX   # boot frame + OS frame on the LCD
```
Then plug the board into any USB port of the PC — that's the wiring.
Daemon: just `hwmond` — it finds the display by USB ID (303a:1001 ESP32-S3, 303a:4001, 2e8a:000a Pico), waits for it to appear, and reconnects after a re-plug; `-port` only overrides this. Verify the board's USB-C is the MCU's *native* USB
(`lsusb` shows `303a:` Espressif or `2e8a:` Raspberry Pi), not a CH340 (`1a86:`) — a CH340 only
does power + flashing, so use the COM build below with it.
If the firmware doesn't enumerate USB before boot, check for "Fast Boot" / "USB legacy" BIOS
settings; the app already asks the firmware to connect all controllers and retries.

**COM-port build:**

1. **Check for a COM port**: `dmesg | grep ttyS` (want `0x3F8 irq 4`), or board manual → "COM header".
   No header → any PCIe serial card; `hwmon.efi` finds it via PCI class 07:00 and uses its BAR0.
2. **Wiring**: COM header → bracket cable → DB9 → MAX3232 → ESP32 GPIO27 (RX) + GND + 3.3V.
   Only PC-TX → display-RX is needed. CYD powered from a rear USB port.
3. **UEFI**: copy `uefi/hwmon.efi` to the ESP as `EFI/hwmon/hwmon.efi`, then
   `efibootmgr -c -d /dev/nvme0n1 -p 1 -L hwmon -l '\EFI\hwmon\hwmon.efi' -u '\EFI\ubuntu\shimx64.efi'`
   (the `-u` string is the loader it chainloads; Windows: `\EFI\Microsoft\Boot\bootmgfw.efi`).
   Secure Boot: sign with `sbctl sign uefi/hwmon.efi` or disable.
4. **Daemon (Linux)**: `go build -o /usr/local/bin/hwmond ./daemon`, then either a systemd unit running
   `hwmond` (USB display, auto-detected) or `hwmond -port /dev/ttyS0` for the COM build, **or** the
   desktop route: `hwmond -autostart on` writes `~/.config/autostart/hwmond.desktop` so it starts at
   login with `-tray` (plus any `-port`/`-interval` you passed). The tray icon uses the
   StatusNotifierItem protocol: KDE/Xfce/Cinnamon show it natively, GNOME needs the AppIndicator
   extension (preinstalled on Ubuntu). The user must be in the `dialout` group to open the port.

   **Daemon (Windows)**: `GOOS=windows go build -o hwmond.exe ./daemon`, then `hwmond.exe` (USB display auto-detected; `-port COM5` for the COM build).
   Load, RAM, disks and disk temperatures (storage temperature IOCTL, NVMe answers it for any user;
   S.M.A.R.T. counters via WMI when run as administrator), GPU name/load/VRAM (WMI performance
   counters) and GPU temperature (the vendor driver.s own ADL or NVML library) all come from Windows
   and the drivers already installed, no helper needed. CPU temperature and fan RPM come from the ACPI
   thermal zone where the firmware has one (most laptops), or, on ASUS boards, from the Nuvoton Super I/O
   through the firmware.s own `ASUSHW` WMI methods, which Windows opens to administrators only, so run
   `hwmond.exe` elevated for that (an elevated prompt, or a Task Scheduler task with *Run with highest
   privileges* at logon). On AMD Zen CPUs the same firmware methods also reach PCI configuration space,
   which `hwmond` uses to read the CPU's own Tctl/Tdie through the SMN index/data window, the same
   register LibreHardwareMonitor and Ryzen Master show; the Super I/O "CPU" register is only the
   fallback, since it is the socket thermistor and sits 10-15 °C below the die. Everything else has no
   driver-free source. For what is still missing, optionally run LibreHardwareMonitor with
   *Options → Remote Web Server → Run*: `hwmond` fills the missing fields from it, and a die sensor
   from LHM replaces a board-thermistor CPU reading (port 8085; `HWMON_LHM_URL`
   overrides the URL, `HWMON_LHM_URL=off` disables the fallback). `hwmond.exe -autostart on` (or the tray's
   *Start on login*) registers a Task Scheduler logon task `hwmond` that runs with highest privileges,
   so the login instance is elevated and the ASUSHW CPU temperature / CPU power work. Creating that
   task needs administrator rights, so a non-elevated hwmond relaunches itself through a UAC prompt;
   decline it and nothing is registered (the tray shows a message box, the command exits with an
   error): an unelevated login instance would start fine but show `--` for CPU temperature and power,
   so there is deliberately no `HKCU\...\CurrentVersion\Run` fallback any more. `-autostart off`
   removes the task and any Run entry left from an older version (the tray checkbox and the control
   panel's *autostart* field report only the task). When started from the task / Explorer the console window
   closes itself; from a terminal the log stays visible. The logon task starts hwmond in the same
   second as `explorer.exe`, before the taskbar exists, so the tray icon is retried every second until
   the shell accepts it (and re-added whenever Explorer restarts; the elevated instance opts its window
   in to the `TaskbarCreated` broadcast, which UIPI would otherwise drop). For the COM build find the port in
   Device Manager → Ports (USB Serial Device).

   **Tray icon (both)**: `hwmond -tray` adds an icon whose tooltip shows the state (*display on COM3*,
   *waiting for a USB display...*, *display disconnected, searching...*); its menu has
   *Open control panel*, a *Start on login* checkbox (same as `-autostart`) and *Quit* (which also parks
   the display in standby). A left click on the icon opens the control panel directly.

   **Control panel (both)**: `hwmond` serves a small web UI at <http://127.0.0.1:8086/> (`-web host:port`
   to move it, `-web ""` to turn it off; loopback addresses only). It shows the daemon state and the
   last frame sent, the display's own information (firmware, chip, flash/PSRAM, free heap, uptime,
   frames received, chip temperature, last reset reason — from the `SRC=DEV` reply, *Refresh* asks
   again) with a backlight slider, and lets you choose what the screen shows: the hardware monitor,
   a **custom text page** (title, up to 12 lines with per-line colours, size, alignment, with a
   preview; kept on screen, re-sent every second, until you switch back) or **any raw protocol line**,
   either held on screen the same way or sent once (for example `SRC=CMD|STANDBY=1`). The JSON API
   behind it: `GET /api/state`, `POST /api/screen {"mode":"monitor"|"text"|"raw", "text":{...}, "raw":"..."}`,
   `POST /api/send {"line":"SRC=..."}`, `POST /api/device/refresh`, `POST /api/device/backlight {"percent":n}`;
   POSTs need `Content-Type: application/json` and no foreign `Origin`, so a web page you happen to
   have open cannot drive the display. Device information and the text page need display firmware
   1.2 or newer (this tree); older firmware shows the boot page for `SRC=TEXT` and never answers `INFO`.
5. **ESP32**: Arduino IDE → ESP32 Dev Module, TFT_eSPI with `User_Setup_CYD.h`, flash `esp32_cyd.ino`.

## Notes / gotchas learned

- gnu-efi's crt0 calls `efi_main` with the SysV ABI: **do not** mark it `EFIAPI`.
- objcopy section globs (`.rel.*`) must be quoted in a Makefile or the shell eats them and the
  PE loses its relocations (→ #GP on the first boot-services call).
- QEMU: use `-chardev serial,id=s0,path=/dev/pts/N -serial chardev:s0` to drive a pty; the
  bare `-serial /dev/pts/N` form does not.
- OVMF echoes its console onto COM1; real boards usually don't, but the parser tolerates it.
- Build freestanding (`-ffreestanding -fno-builtin -fno-plt`): otherwise GCC turns a byte loop
  into a `strlen@plt` call, the PLT isn't in the PE, and execution jumps into `.data`.
- `EFI_USB_IO_PROTOCOL_GUID` is `2B2F68D6-0CD2-44CF-8E8B-BBA20B1B5B75` (last byte 0x75).
- QEMU's `usb-serial` is full-speed; it works on `qemu-xhci`, not on `usb-ehci`.
