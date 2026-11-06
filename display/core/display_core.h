/*
 * display_core — platform-independent part of the hardware monitor display.
 *
 * Feeds bytes from the serial line, parses "KEY=VAL|KEY=VAL\n" frames, and
 * renders a 320x240 layout through a tiny drawing callback interface, so the
 * exact same code runs on the ESP32 (TFT_eSPI) and on a PC (PNG simulator).
 */
#ifndef DISPLAY_CORE_H
#define DISPLAY_CORE_H

#include <stdint.h>

#define DC_W 320
#define DC_H 240
#define DC_MAX_FIELDS 24
#define DC_MAX_LINE   512

typedef uint16_t dc_color;                      /* RGB565, same as ILI9341 */
#define DC_RGB(r, g, b) ((dc_color)((((r) & 0xF8) << 8) | (((g) & 0xFC) << 3) | ((b) >> 3)))

/* Drawing primitives the platform must provide. Text is a 6x8 bitmap font
   scaled by `size` — matches TFT_eSPI's default font 1 so it looks identical. */
typedef struct {
    void (*fill_rect)(int x, int y, int w, int h, dc_color c);
    void (*draw_text)(int x, int y, const char *s, dc_color fg, dc_color bg, int size);
} dc_gfx;

typedef struct {
    char key[16];
    char val[64];
} dc_field;

typedef struct {
    dc_field fields[DC_MAX_FIELDS];
    int      nfields;
    char     line[DC_MAX_LINE];
    int      linelen;
    uint32_t frames;            /* frames received so far */
    uint32_t last_rx_ms;        /* for the "no signal" state */
} dc_state;

void        dc_init(dc_state *s);
/* Feed one byte from the UART. Returns 1 when a complete frame was parsed. */
int         dc_feed(dc_state *s, uint8_t byte, uint32_t now_ms);
const char *dc_get(const dc_state *s, const char *key);       /* "" if absent */
/* Redraw the whole screen from the current state. */
void        dc_render(const dc_state *s, const dc_gfx *g, uint32_t now_ms);

#endif
