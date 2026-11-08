/*
 * esp32_es3c28p.ino — hardware monitor display firmware for the
 * ESP32-S3 2.8" "ES3C28P" board (ILI9341V 240x320 SPI, FT6336G touch,
 * native USB on the S3's USB-Serial/JTAG peripheral; VID:PID 303a:1001).
 *
 * Single cable: the PC's USB-C provides power and the data stream.
 *   - at boot, hwmon.efi writes the frame to the CDC-ACM interface
 *   - once the OS is up, hwmond writes to /dev/ttyACM0 (macOS: /dev/cu.usbmodem*)
 * Both arrive on `Serial` (USB-Serial/JTAG), nothing to configure.
 *
 * Optional fallback input: a MAX3232 on the IO2/3/14/21 header, TTL TX -> IO14.
 *
 * Protocol: "SRC=...|KEY=VAL|...\n" at any baud (USB ignores it); see display_core.h.
 * The board talks back on the same port, only when asked:
 *   host: SRC=CMD|INFO=1      ->  board: SRC=DEV|FW=..|CHIP=..|HEAP_KB=..|UP_S=..|...
 *   host: SRC=CMD|BL=0..100   ->  backlight brightness (PWM), kept until reset
 * A SRC=DEV line is also sent once whenever the USB host (re)connects.
 * Build: ./build.sh (arduino-cli; TFT_eSPI is configured via -D flags, no library edits).
 */
#include <TFT_eSPI.h>
#include <esp_system.h>
#include "display_core.h"

#define FW_VERSION    "1.2"
#define BOARD_NAME    "ES3C28P"
#define BACKLIGHT_PIN 45
#define AUX_RX_PIN    14      // expansion header, optional RS-232 fallback via MAX3232
#define AUX_TX_PIN    21

TFT_eSPI tft;
TFT_eSprite frame(&tft);          // full-screen off-screen buffer: no flicker, one push per frame
static bool useSprite = false;
HardwareSerial auxSerial(1);
static dc_state state;

static void gfx_fill_rect(int x, int y, int w, int h, dc_color c)
{
    if (useSprite) frame.fillRect(x, y, w, h, c); else tft.fillRect(x, y, w, h, c);
}
static void gfx_draw_text(int x, int y, const char *s, dc_color fg, dc_color bg, int size)
{
    TFT_eSPI &g = useSprite ? (TFT_eSPI &)frame : tft;
    g.setTextFont(1);
    g.setTextSize(size);
    g.setTextColor(fg, bg);
    g.setCursor(x, y);
    g.print(s);
}
static const dc_gfx gfx = { gfx_fill_rect, gfx_draw_text };

static void render(uint32_t now)
{
    dc_render(&state, &gfx, now);
    if (useSprite) frame.pushSprite(0, 0);
}

/* ---- backlight ------------------------------------------------------------
 * PWM via analogWrite (LEDC). Once a pin is on LEDC, digitalWrite() on it is
 * refused by the arduino-esp32 3.x pin manager, so every backlight change goes
 * through here — including standby (0) and wake (the chosen level). */
static int brightness = 100;                                // percent, set by SRC=CMD|BL=n
static bool standby = false;

static void applyBacklight()
{
    analogWrite(BACKLIGHT_PIN, standby ? 0 : brightness * 255 / 100);
}

/* ---- device info: "SRC=DEV|..." on request / on USB connect ---------------- */
static void sendDeviceInfo(Stream &out, uint32_t now)
{
    char line[480];
    uint64_t mac = ESP.getEfuseMac();
    snprintf(line, sizeof line,
        "SRC=DEV|FW=%s|BOARD=%s|CHIP=%s|REV=%d|CORES=%d|CPU_MHZ=%u|FLASH_MB=%u|PSRAM_KB=%u|PSRAM_FREE_KB=%u"
        "|HEAP_TOTAL_KB=%u|HEAP_KB=%u|HEAP_MIN_KB=%u|SKETCH_KB=%u|APP_FREE_KB=%u|SDK=%s|CORE=%s|UP_S=%lu|RX_BYTES=%lu|FRAMES=%lu"
        "|SPRITE=%d|BL=%d|STANDBY=%d|RESET=%d|TEMP=%d|MAC=%04X%08X\n",
        FW_VERSION, BOARD_NAME, ESP.getChipModel(), ESP.getChipRevision(), ESP.getChipCores(),
        (unsigned)getCpuFrequencyMhz(), (unsigned)(ESP.getFlashChipSize() >> 20), (unsigned)(ESP.getPsramSize() >> 10), (unsigned)(ESP.getFreePsram() >> 10),
        (unsigned)(ESP.getHeapSize() >> 10), (unsigned)(ESP.getFreeHeap() >> 10), (unsigned)(ESP.getMinFreeHeap() >> 10),
        (unsigned)(ESP.getSketchSize() >> 10), (unsigned)(ESP.getFreeSketchSpace() >> 10),
        ESP.getSdkVersion(), ESP_ARDUINO_VERSION_STR, (unsigned long)(now / 1000),
        (unsigned long)state.rx_bytes, (unsigned long)state.frames,
        useSprite, brightness, standby, (int)esp_reset_reason(), (int)(temperatureRead() + 0.5f),
        (unsigned)(mac >> 32), (unsigned)mac);
    out.print(line);
}

/* Commands carried by the last CMD frame fed from `in`: answer / apply, then clear. */
static void handleCommands(Stream &in, uint32_t now)
{
    if (state.info_req) { state.info_req = 0; sendDeviceInfo(in, now); }
    if (state.bl_req >= 0) { brightness = state.bl_req; state.bl_req = -1; applyBacklight(); }
}

/* ---- standby: panel + backlight off when the PC is gone ------------------
 * USB 5 V usually stays on after the PC shuts down (and always while it sleeps),
 * so without this the LCD would burn 24/7. After STANDBY_AFTER_MS of silence,
 * or at once when hwmond says "SRC=CMD|STANDBY=1" on quit / OS shutdown, the
 * backlight (~100 mA, the bulk of the board's draw) is cut, the ILI9341 goes to
 * sleep (SLPIN) and the CPU drops to 80 MHz. The first byte of the next frame
 * wakes everything and the page is redrawn before the backlight comes back. */
#define STANDBY_AFTER_MS 60000UL

static void enterStandby()
{
    standby = true;
    applyBacklight();
    tft.writecommand(TFT_DISPOFF);
    tft.writecommand(0x10);                                 // panel sleep: ~1 mA; needs 120 ms before SLPOUT
    setCpuFrequencyMhz(80);                                 // USB-Serial/JTAG and UART keep running
}

static void leaveStandby(uint32_t now)
{
    standby = false;
    setCpuFrequencyMhz(240);
    tft.writecommand(0x11);          // SLPOUT
    delay(120);                                             // ILI9341 datasheet: wait before DISPON
    tft.writecommand(TFT_DISPON);
    render(now);                                            // fresh page first...
    applyBacklight();                                       // ...then light it, no stale flash
}

void setup()
{
    Serial.begin(115200);                                   // USB-Serial/JTAG (HWCDC)
    Serial.setRxBufferSize(1024);                           // a frame is up to ~300 bytes
    auxSerial.begin(115200, SERIAL_8N1, AUX_RX_PIN, AUX_TX_PIN);

    tft.init();                                             // drives TFT_BL high as a GPIO...
    tft.setRotation(1);                                     // landscape 320x240, USB-C on the left
    frame.setColorDepth(16);
    useSprite = frame.createSprite(DC_W, DC_H) != nullptr;  // 150 KB; falls back to direct drawing if no RAM
    dc_init(&state);
    render(millis());
    applyBacklight();                                       // ...then LEDC takes the pin over for good
}

void loop()
{
    bool redraw = false;
    uint32_t now = millis();
    while (Serial.available())    redraw |= dc_feed(&state, Serial.read(), now);
    handleCommands(Serial, now);
    while (auxSerial.available()) redraw |= dc_feed(&state, auxSerial.read(), now);
    handleCommands(auxSerial, now);

    static bool wasConnected = false;                       // announce ourselves when the USB host (re)attaches
    bool connected = Serial.isConnected();
    if (connected && !wasConnected) sendDeviceInfo(Serial, now);
    wasConnected = connected;

    bool wantStandby = dc_standby(&state, now, STANDBY_AFTER_MS);
    if (wantStandby != standby) {
        if (wantStandby) enterStandby(); else leaveStandby(now);
        delay(5);
        return;
    }
    if (standby) { delay(50); return; }                     // dark: just keep draining the serial ports

    static uint32_t lastTick = 0;
    static bool staleShown = false;
    if (redraw) staleShown = false;
    if (!redraw && now - lastTick > 1000) {
        lastTick = now;
        if (!state.frames) redraw = true;                   // idle page: refresh uptime / rx counter
        else if (!staleShown && now - state.last_rx_ms > 5000) { redraw = true; staleShown = true; }  // NO SIGNAL, painted once
    }
    if (redraw) render(now);
    delay(5);
}
