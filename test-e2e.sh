#!/usr/bin/env bash
# End-to-end test with zero hardware:
#   [UEFI app in QEMU/OVMF] --COM1--> virtual serial cable --> [display simulator] --> PNGs
#   [Go daemon on this host] ---------^   (takes over the same port after "boot")
#
# Needs: gcc, gnu-efi, qemu-system-x86, ovmf, socat, go.   Output: ./out/frame-*.png
set -euo pipefail
cd "$(dirname "$0")"
OUT=${OUT:-out}; rm -rf "$OUT"; mkdir -p "$OUT"

echo "== build"
make -C uefi esp >/dev/null
(cd daemon && go build -o hwmond .)
gcc -O2 -w -o display/sim/sim display/sim/sim.c display/core/display_core.c

echo "== virtual serial cable"
socat pty,raw,echo=0,link="$OUT/ttyPC" pty,raw,echo=0,link="$OUT/ttyDISPLAY" & SOCAT=$!
sleep 0.5
display/sim/sim "$OUT/ttyDISPLAY" "$OUT" --scale 2 > "$OUT/display.log" & SIM=$!
trap 'kill $SIM $SOCAT 2>/dev/null || true' EXIT

echo "== boot phase: UEFI app in QEMU, COM1 -> cable"
cp /usr/share/OVMF/OVMF_VARS_4M.fd "$OUT/ovmf_vars.fd"
timeout 10 qemu-system-x86_64 -machine q35 -m 4096 -smp 4 -cpu max \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd \
  -drive if=pflash,format=raw,file="$OUT/ovmf_vars.fd" \
  -drive format=raw,file=fat:rw:uefi/esp \
  -smbios type=2,manufacturer="ASUS",product="PRIME B650-PLUS" -smbios type=17,speed=6000 \
  -display none -chardev serial,id=s0,path="$OUT/ttyPC" -serial chardev:s0 -no-reboot >/dev/null 2>&1 || true

echo "== OS phase: daemon -> same cable"
timeout 3.5 daemon/hwmond -port "$OUT/ttyPC" || true
sleep 1

echo "== frames the display rendered:"
grep frame "$OUT/display.log"
echo "PNGs in $OUT/"
