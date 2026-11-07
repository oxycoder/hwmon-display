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
 -DTFT_MOSI=11 -DTFT_MISO=13 -DTFT_SCLK=12 -DTFT_CS=10 -DTFT_DC=46 -DTFT_RST=-1 -DTFT_BL=45 -DTFT_BACKLIGHT_ON=1 \
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
    arduino-cli compile --fqbn "$FQBN" \
      --build-property "compiler.cpp.extra_flags=$TFT_FLAGS" \
      --build-property "compiler.c.extra_flags=$TFT_FLAGS" \
      --output-dir build .
    ls -la build/*.bin;;
  flash)
    PORT=${2:?usage: ./build.sh flash /dev/cu.usbmodemXXXX}
    arduino-cli upload --fqbn "$FQBN" -p "$PORT" --input-dir build .
    echo "flashed; the board re-enumerates in ~2 s";;
  test)
    PORT=${2:?usage: ./build.sh test /dev/cu.usbmodemXXXX}
    stty -f "$PORT" 115200 raw 2>/dev/null || stty -F "$PORT" 115200 raw
    sleep 0.2
    echo 'SRC=UEFI|CPU=AMD Ryzen 7 7800X3D 8-Core Processor|THREADS=16|RAM_MB=32768|BOARD=ASUS PRIME B650-PLUS|BIOS=American Megatrends 3024|DIMMS=2|DIMM_MB=32768|DIMM_MHZ=6000|GPU=1002:744c|NIC=10ec:8125|STORAGE_CTRL=2' > "$PORT"
    sleep 2
    echo 'SRC=OS|HOST=tt-desktop|CPU=AMD Ryzen 7 7800X3D 8-Core Processor|THREADS=16|OS=Ubuntu 24.04.5 LTS|LOAD=37|TEMP=68|FAN=1180|RAM_USED_MB=11850|RAM_MB=31900' > "$PORT"
    echo "sent a BOOT frame, then an OS frame; after 5 s of silence the NO SIGNAL banner appears";;
  *) echo "usage: $0 [setup|build|flash PORT|test PORT]"; exit 2;;
esac
