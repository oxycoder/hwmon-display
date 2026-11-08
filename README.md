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
| `daemon/` | OS daemon. `linux.go`: hwmon/procfs, every mounted block device from `/proc/mounts` (USB, NTFS Windows partitions). `windows.go`: kernel32 for load/RAM/drives (fixed + removable), LibreHardwareMonitor's web server for temp/fan, COM port via SetCommState. `tray_*.go`: tray icon + start-at-login (Windows: Shell_NotifyIcon + HKCU Run key, no deps; Linux: StatusNotifierItem over D-Bus via godbus + XDG autostart). | Linux **tested over a pty**; Windows tray/autostart/parser unit-tested on a real desktop |
| `display/core/` | Platform-independent parser + renderer (320×240). Shared by the simulator and the ESP32. | **tested** |
| `display/sim/` | Host simulator: reads a serial device/file, renders frames to PNG. | **tested** |
| `display/esp32_es3c28p/` | **The board in use.** Sketch + `build.sh` for the ESP32-S3 2.8" ES3C28P (ILI9341V, native USB). | syntax-checked; flash with `build.sh` |
| `display/esp32_cyd/` | Arduino sketch for the classic ESP32-2432S028 (COM build) + TFT_eSPI config. | syntax-checked |
| `test-e2e.sh` | Whole pipeline with zero hardware. | |

## Protocol

One line per frame, `\n`-terminated, must start with `SRC=`:

```
SRC=UEFI|CPU=...|THREADS=16|RAM_MB=32768|BOARD=...|BIOS=...|DIMMS=2|DIMM_MB=32768|DIMM_MHZ=6000|GPU=1002:744c|NIC=10ec:8125|STORAGE_CTRL=2
SRC=OS|HOST=tt-desktop|CPU=...|THREADS=16|OS=Ubuntu 24.04|LOAD=37|TEMP=68|FAN=1180|RAM_USED_MB=11850|RAM_MB=31900|DISK=/:62:931,home:40:1863,USB:5:64,Window:71:931|DISK_T=38,35|GPU=Radeon RX 7900 XTX|GPU_TEMP=61|GPU_LOAD=42|VRAM_USED_MB=7500|VRAM_MB=24576
```

Unknown keys are ignored; missing keys render as `--`. Anything not starting with `SRC=`
(firmware console echo, noise) is dropped. After 5 s of silence the display shows a NO SIGNAL banner.

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
   For temperatures and fan speed, run LibreHardwareMonitor with *Options → Remote Web Server → Run*
   (port 8085; override with `HWMON_LHM_URL`). `hwmond.exe -autostart on` registers it under
   `HKCU\...\CurrentVersion\Run` (visible in Task Manager → Startup apps) so it starts at login with
   `-tray`; `-autostart off` removes it. When started from the Run key / Explorer the console window
   closes itself; from a terminal the log stays visible. For the COM build find the port in
   Device Manager → Ports (USB Serial Device).

   **Tray icon (both)**: `hwmond -tray` adds an icon whose tooltip shows the state (*display on COM3*,
   *waiting for a USB display...*, *display disconnected, searching...*); its menu has a
   *Start on login* checkbox (same as `-autostart`) and *Quit*.
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
