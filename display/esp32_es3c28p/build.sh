#!/usr/bin/env bash
# Build + flash the ES3C28P display firmware with arduino-cli (macOS/Linux).
#
#   brew install arduino-cli            # macOS (Linux: see arduino.github.io/arduino-cli)
#   ./build.sh setup                    # once: ESP32 core + TFT_eSPI
#   ./build.sh                          # compile
#   ./build.sh flash /dev/cu.usbmodem3101
#   ./build.sh test  /dev/cu.usbmodem3101   # push a sample frame at the screen
#
# TFT_eSPI is configured entirely through -D flags (USER_SETUP_LOADED), so the
# library's own User_Setup.h is never touched.
set -euo pipefail
cd "$(dirname "$0")"

FQBN="esp32:esp32:esp32s3:USBMode=hwcdc,CDCOnBoot=cdc,FlashSize=16M,PSRAM=disabled,PartitionScheme=default,UploadSpeed=921600"

# ES3C28P pin map (ILI9341V, 4-wire SPI). LCD reset is tied to the chip reset.
TFT_FLAGS="-DUSER_SETUP_LOADED=1 -DILI9341_DRIVER=1 -DTFT_WIDTH=240 -DTFT_HEIGHT=320 \
 -DTFT_MOSI=11 -DTFT_MISO=13 -DTFT_SCLK=12 -DTFT_CS=10 -DTFT_DC=46 -DTFT_RST=-1 -DTFT_BL=45 -DTFT_BACKLIGHT_ON=1 -DTFT_INVERSION_ON=1 \
 -DLOAD_GLCD=1 -DLOAD_FONT2=1 -DLOAD_GFXFF=1 -DSMOOTH_FONT=1 \
 -DSPI_FREQUENCY=40000000 -DSPI_READ_FREQUENCY=16000000 -DUSE_HSPI_PORT=1"

case "${1:-build}" in
  setup)
    arduino-cli config init --overwrite >/dev/null
    arduino-cli config add board_manager.additional_urls https://espressif.github.io/arduino-esp32/package_esp32_index.json
    arduino-cli core update-index
    arduino-cli core install esp32:esp32
    arduino-cli lib install "TFT_eSPI"
    echo "setup done";;
  build)
    # display_core.{c,h} are symlinks into ../core. On a Windows checkout without
    # symlink support git writes them as one-line text files, so stage a copy of
    # the sketch with the real files instead of compiling in place.
    SKETCH=build/sketch/esp32_es3c28p
    rm -rf "$SKETCH"; mkdir -p "$SKETCH"
    cp esp32_es3c28p.ino ../core/display_core.c ../core/display_core.h "$SKETCH/"
    arduino-cli compile --fqbn "$FQBN" \
      --build-property "compiler.cpp.extra_flags=$TFT_FLAGS" \
      --build-property "compiler.c.extra_flags=$TFT_FLAGS" \
      --output-dir build "$SKETCH"
    ls -la build/*.bin;;
  flash)
    PORT=${2:?usage: ./build.sh flash /dev/cu.usbmodemXXXX   (Windows: COM3)}
    arduino-cli upload --fqbn "$FQBN" -p "$PORT" --input-dir build build/sketch/esp32_es3c28p
    echo "flashed; the board re-enumerates in ~2 s";;
  test)
    PORT=${2:?usage: ./build.sh test /dev/cu.usbmodemXXXX}
    # Hold the port open for the whole test: opening/closing it toggles DTR/RTS,
    # which the S3's USB-Serial/JTAG peripheral can interpret as a reset.
    exec 3<>"$PORT"
    stty -f "$PORT" 115200 raw -hupcl 2>/dev/null || stty -F "$PORT" 115200 raw -hupcl
    sleep 1
    echo 'SRC=UEFI|CPU=AMD Ryzen 7 7800X3D 8-Core Processor|THREADS=16|RAM_MB=32768|BOARD=ASUS PRIME B650-PLUS|BIOS=American Megatrends 3024|DIMMS=2|DIMM_MB=32768|DIMM_MHZ=6000|GPU=1002:744c|NIC=10ec:8125|STORAGE_CTRL=2' >&3
    echo "sent BOOT frame (screen should show the boot page now)"; sleep 3
    echo 'SRC=OS|HOST=tt-desktop|CPU=AMD Ryzen 7 7800X3D 8-Core Processor|THREADS=16|OS=Ubuntu 24.04.5 LTS|LOAD=37|TEMP=68|FAN=1180|RAM_USED_MB=11850|RAM_MB=31900|DISK=/:62:931,/home:40:1863|DISK_T=38,35|GPU=AMD Radeon RX 7900 XTX|GPU_TEMP=61|GPU_LOAD=42|VRAM_USED_MB=7500|VRAM_MB=24576' >&3
    echo "sent OS frame (screen should show the OS page now)"; sleep 3
    exec 3>&-
    echo "port closed; NO SIGNAL banner appears after 5 s of silence";;
  *) echo "usage: $0 [setup|build|flash PORT|test PORT]"; exit 2;;
esac
