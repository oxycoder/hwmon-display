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
 * Protocol: "SRC=...|KEY=VAL|...\n" at any baud (USB ignores it); see display_core.c.
 * Build: ./build.sh (arduino-cli; TFT_eSPI is configured via -D flags, no library edits).
 */
#include <TFT_eSPI.h>
#include "display_core.h"

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

/* ---- standby: panel + backlight off when the PC is gone ------------------
 * USB 5 V usually stays on after the PC shuts down (and always while it sleeps),
 * so without this the LCD would burn 24/7. After STANDBY_AFTER_MS of silence,
 * or at once when hwmond says "SRC=CMD|STANDBY=1" on quit / OS shutdown, the
 * backlight (~100 mA, the bulk of the board's draw) is cut, the ILI9341 goes to
 * sleep (SLPIN) and the CPU drops to 80 MHz. The first byte of the next frame
 * wakes everything and the page is redrawn before the backlight comes back. */
#define STANDBY_AFTER_MS 60000UL
static bool standby = false;

static void enterStandby()
{
    standby = true;
    digitalWrite(BACKLIGHT_PIN, LOW);
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
    digitalWrite(BACKLIGHT_PIN, HIGH);                      // ...then light it, no stale flash
}

void setup()
{
    pinMode(BACKLIGHT_PIN, OUTPUT);
    digitalWrite(BACKLIGHT_PIN, HIGH);

    Serial.begin(115200);                                   // USB-Serial/JTAG (HWCDC)
    Serial.setRxBufferSize(1024);                           // a frame is up to ~300 bytes
    auxSerial.begin(115200, SERIAL_8N1, AUX_RX_PIN, AUX_TX_PIN);

    tft.init();
    tft.setRotation(1);                                     // landscape 320x240, USB-C on the left
    frame.setColorDepth(16);
    useSprite = frame.createSprite(DC_W, DC_H) != nullptr;  // 150 KB; falls back to direct drawing if no RAM
    dc_init(&state);
    render(millis());
}

void loop()
{
    bool redraw = false;
    uint32_t now = millis();
    while (Serial.available())    redraw |= dc_feed(&state, Serial.read(), now);
    while (auxSerial.available()) redraw |= dc_feed(&state, auxSerial.read(), now);

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
