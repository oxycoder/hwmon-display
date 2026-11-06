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
| `daemon/main.go` | Linux daemon. Reads hwmon/procfs, writes the same protocol at 1 Hz. No deps. | **tested over a pty** |
| `display/core/` | Platform-independent parser + renderer (320×240). Shared by the simulator and the ESP32. | **tested** |
| `display/sim/` | Host simulator: reads a serial device/file, renders frames to PNG. | **tested** |
| `display/esp32_cyd/` | Arduino sketch for the ESP32-2432S028 + TFT_eSPI config. | syntax-checked; needs the board to flash |
| `test-e2e.sh` | Whole pipeline with zero hardware. | |

## Protocol

One line per frame, `\n`-terminated, must start with `SRC=`:

```
SRC=UEFI|CPU=...|THREADS=16|RAM_MB=32768|BOARD=...|BIOS=...|DIMMS=2|DIMM_MB=32768|DIMM_MHZ=6000|GPU=1002:744c|NIC=10ec:8125|STORAGE_CTRL=2
SRC=OS|HOST=tt-desktop|CPU=...|THREADS=16|OS=Ubuntu 24.04|LOAD=37|TEMP=68|FAN=1180|RAM_USED_MB=11850|RAM_MB=31900
```

Unknown keys are ignored; missing keys render as `--`. Anything not starting with `SRC=`
(firmware console echo, noise) is dropped. After 5 s of silence the display shows a NO SIGNAL banner.

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

**USB-C build (ESP32-S3 / Pico):** plug the board into any USB port — that's the wiring.
Daemon: `hwmond -port /dev/ttyACM0`. Verify the board's USB-C is the MCU's *native* USB
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
4. **Daemon**: `go build -o /usr/local/bin/hwmond ./daemon` + a systemd unit running
   `hwmond -port /dev/ttyS0`. (Windows port: same protocol, read LibreHardwareMonitor instead of hwmon.)
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
