/*
 * esp32_cyd.ino — hardware monitor display firmware for the
 * ESP32-2432S028 "Cheap Yellow Display" (2.8" ILI9341, 320x240).
 *
 * Wiring (P3 header on the CYD; use a MAX3232 between the PC's RS-232 and this):
 *   MAX3232 TTL TX  -> GPIO 27  (we only receive; PC TX -> us)
 *   MAX3232 GND     -> GND
 *   MAX3232 VCC     -> 3.3V
 *   CYD micro-USB   -> any PC USB port (5 V power)
 *
 * Arduino IDE: board "ESP32 Dev Module", library TFT_eSPI with the CYD
 * User_Setup.h (see User_Setup_CYD.h next to this file).
 *
 * Protocol: lines of "KEY=VAL|KEY=VAL\n" at 115200 8N1; see display_core.c.
 * The USB serial (Serial) also accepts the same lines, so you can test the
 * screen from a PC with nothing but the USB cable:  cat sample.txt > /dev/ttyUSB0
 */
#include <TFT_eSPI.h>
#include "display_core.h"

#define PC_RX_PIN   27        // free GPIO on the CYD P3 header
#define PC_TX_PIN   22        // unused (we never transmit); CYD's P3 has 22 too
#define BACKLIGHT   21

TFT_eSPI tft;
HardwareSerial pcSerial(2);
static dc_state state;

/* ---- gfx callbacks: map the core's primitives onto TFT_eSPI ---- */
static void gfx_fill_rect(int x, int y, int w, int h, dc_color c) { tft.fillRect(x, y, w, h, c); }
static void gfx_draw_text(int x, int y, const char *s, dc_color fg, dc_color bg, int size)
{
    tft.setTextFont(1);
    tft.setTextSize(size);
    tft.setTextColor(fg, bg);
    tft.setCursor(x, y);
    tft.print(s);
}
static const dc_gfx gfx = { gfx_fill_rect, gfx_draw_text };

void setup()
{
    pinMode(BACKLIGHT, OUTPUT);
    digitalWrite(BACKLIGHT, HIGH);

    Serial.begin(115200);                                  // USB, for testing
    pcSerial.begin(115200, SERIAL_8N1, PC_RX_PIN, PC_TX_PIN);

    tft.init();
    tft.setRotation(1);                                    // landscape, 320x240
    dc_init(&state);
    dc_render(&state, &gfx, millis());
}

/* ---- standby: backlight + panel off once the PC is gone -------------------
 * The CYD is USB-powered and most PCs keep USB 5 V up after shutdown, so after
 * STANDBY_AFTER_MS of silence (or at once on "SRC=CMD|STANDBY=1" from hwmond)
 * the backlight is cut, the ILI9341 sleeps and the CPU drops to 80 MHz.
 * The next data frame wakes it and redraws before the backlight returns. */
#define STANDBY_AFTER_MS 60000UL
static bool standby = false;

static void enterStandby()
{
    standby = true;
    digitalWrite(BACKLIGHT, LOW);
    tft.writecommand(TFT_DISPOFF);
    tft.writecommand(0x10);          // SLPIN
    setCpuFrequencyMhz(80);
}

static void leaveStandby(uint32_t now)
{
    standby = false;
    setCpuFrequencyMhz(240);
    tft.writecommand(0x11);          // SLPOUT
    delay(120);
    tft.writecommand(TFT_DISPON);
    dc_render(&state, &gfx, now);
    digitalWrite(BACKLIGHT, HIGH);
}

void loop()
{
    bool redraw = false;
    uint32_t now = millis();
    while (pcSerial.available()) redraw |= dc_feed(&state, pcSerial.read(), now);
    while (Serial.available())   redraw |= dc_feed(&state, Serial.read(), now);

    bool wantStandby = dc_standby(&state, now, STANDBY_AFTER_MS);
    if (wantStandby != standby) {
        if (wantStandby) enterStandby(); else leaveStandby(now);
        delay(5);
        return;
    }
    if (standby) { delay(50); return; }

    static uint32_t lastStaleCheck = 0;
    if (!redraw && now - lastStaleCheck > 1000) {          // repaint "NO SIGNAL" banner if the PC went quiet
        lastStaleCheck = now;
        if (state.frames && now - state.last_rx_ms > 5000) redraw = true;
    }
    if (redraw) dc_render(&state, &gfx, now);
    delay(5);
}
