#include "display_core.h"
#include <string.h>
#include <stdio.h>
#include <stdlib.h>

/* ---------- parsing ------------------------------------------------------- */

void dc_init(dc_state *s) { memset(s, 0, sizeof(*s)); }

static void parse_line(dc_state *s)
{
    s->nfields = 0;
    char *p = s->line;
    while (*p && s->nfields < DC_MAX_FIELDS) {
        char *end = strchr(p, '|');
        if (end) *end = 0;
        char *eq = strchr(p, '=');
        if (eq) {
            *eq = 0;
            dc_field *f = &s->fields[s->nfields++];
            strncpy(f->key, p, sizeof(f->key) - 1);      f->key[sizeof(f->key) - 1] = 0;
            strncpy(f->val, eq + 1, sizeof(f->val) - 1); f->val[sizeof(f->val) - 1] = 0;
        }
        if (!end) break;
        p = end + 1;
    }
}

int dc_feed(dc_state *s, uint8_t b, uint32_t now_ms)
{
    s->rx_bytes++;
    if (b == '\r') return 0;
    if (b == '\n') {
        s->line[s->linelen] = 0;
        int ok = 0;
        /* Only accept lines that start with "SRC=" — anything else on the wire
           (firmware console echo, ANSI escapes, noise) is ignored, and the
           previous good frame stays on screen. */
        if (s->linelen > 4 && !memcmp(s->line, "SRC=", 4)) {
            parse_line(s);
            s->frames++;
            s->last_rx_ms = now_ms;
            ok = s->nfields > 0;
        }
        s->linelen = 0;
        return ok;
    }
    if (s->linelen < DC_MAX_LINE - 1) s->line[s->linelen++] = (char)b;
    else s->linelen = 0;                     /* overflow: drop the garbage line */
    return 0;
}

const char *dc_get(const dc_state *s, const char *key)
{
    for (int i = 0; i < s->nfields; i++)
        if (!strcmp(s->fields[i].key, key)) return s->fields[i].val;
    return "";
}

/* ---------- rendering ----------------------------------------------------- */

#define C_BG      DC_RGB(0x10, 0x12, 0x18)
#define C_PANEL   DC_RGB(0x1C, 0x20, 0x2A)
#define C_TEXT    DC_RGB(0xE6, 0xE8, 0xEC)
#define C_DIM     DC_RGB(0x8A, 0x90, 0x9C)
#define C_ACCENT  DC_RGB(0x4C, 0xC2, 0xFF)
#define C_OK      DC_RGB(0x4C, 0xD9, 0x7A)
#define C_WARN    DC_RGB(0xFF, 0xB8, 0x3C)
#define C_HOT     DC_RGB(0xFF, 0x5A, 0x5A)

static int ival(const dc_state *s, const char *k, int def)
{
    const char *v = dc_get(s, k);
    return *v ? atoi(v) : def;
}

static void text(const dc_gfx *g, int x, int y, const char *s, dc_color fg, int size)
{
    g->draw_text(x, y, s, fg, C_BG, size);
}

/* Truncate a string to fit `maxchars` at the given font size, with "…" → ".." */
static void fit(char *dst, int dstsz, const char *src, int maxchars)
{
    int n = (int)strlen(src);
    if (n <= maxchars) { snprintf(dst, dstsz, "%s", src); return; }
    snprintf(dst, dstsz, "%.*s..", maxchars - 2, src);
}

static void bar(const dc_gfx *g, int x, int y, int w, int h, int pct, dc_color c)
{
    if (pct < 0) pct = 0;
    if (pct > 100) pct = 100;
    g->fill_rect(x, y, w, h, C_PANEL);
    g->fill_rect(x, y, w * pct / 100, h, c);
}

static dc_color temp_color(int t)
{
    if (t < 0)  return C_DIM;
    if (t < 60) return C_OK;
    if (t < 80) return C_WARN;
    return C_HOT;
}

static void render_boot(const dc_state *s, const dc_gfx *g)
{
    char buf[80];
    text(g, 8, 6, "BOOT", C_ACCENT, 2);
    text(g, 8 + 5 * 12 + 4, 10, "firmware stage", C_DIM, 1);
    g->fill_rect(0, 28, DC_W, 1, C_PANEL);

    int y = 40;
    const char *labels[][2] = {
        { "CPU",   "CPU" }, { "Threads", "THREADS" }, { "RAM", "RAM_MB" },
        { "DIMMs", "DIMMS" }, { "Board", "BOARD" }, { "BIOS", "BIOS" },
        { "GPU",   "GPU" }, { "NIC", "NIC" },
    };
    for (unsigned i = 0; i < sizeof(labels) / sizeof(labels[0]); i++) {
        const char *v = dc_get(s, labels[i][1]);
        if (!*v) continue;
        text(g, 8, y, labels[i][0], C_DIM, 1);
        if (!strcmp(labels[i][1], "RAM_MB")) {
            snprintf(buf, sizeof buf, "%d GB", (atoi(v) + 512) / 1024);
            if (*dc_get(s, "DIMM_MHZ")) {
                size_t n = strlen(buf);
                snprintf(buf + n, sizeof buf - n, "  @ %s MT/s", dc_get(s, "DIMM_MHZ"));
            }
        } else {
            fit(buf, sizeof buf, v, 40);
        }
        text(g, 64, y, buf, C_TEXT, 1);
        y += 16;
    }
    g->fill_rect(0, DC_H - 14, DC_W, 14, C_PANEL);
    g->draw_text(8, DC_H - 11, "waiting for OS...", C_DIM, C_PANEL, 1);
}

static void render_os(const dc_state *s, const dc_gfx *g)
{
    char buf[80];
    int load = ival(s, "LOAD", -1), temp = ival(s, "TEMP", -1), fan = ival(s, "FAN", -1);
    int used = ival(s, "RAM_USED_MB", -1), total = ival(s, "RAM_MB", -1);

    /* header */
    fit(buf, sizeof buf, dc_get(s, "HOST"), 20);
    text(g, 8, 6, buf, C_ACCENT, 2);
    fit(buf, sizeof buf, dc_get(s, "CPU"), 50);
    text(g, 8, 24, buf, C_DIM, 1);
    g->fill_rect(0, 36, DC_W, 1, C_PANEL);

    /* big numbers: temp / load / fan */
    int col = DC_W / 3;
    const char *hdr[3] = { "CPU TEMP", "CPU LOAD", "FAN" };
    char vals[3][16]; dc_color cols[3];
    if (temp >= 0) snprintf(vals[0], 16, "%d", temp); else snprintf(vals[0], 16, "--");
    if (load >= 0) snprintf(vals[1], 16, "%d", load); else snprintf(vals[1], 16, "--");
    if (fan > 0)   snprintf(vals[2], 16, "%d", fan);  else snprintf(vals[2], 16, "--");
    cols[0] = temp_color(temp);
    cols[1] = load < 0 ? C_DIM : load < 70 ? C_OK : load < 90 ? C_WARN : C_HOT;
    cols[2] = fan > 0 ? C_TEXT : C_DIM;
    const char *unit[3] = { "C", "%", "rpm" };
    for (int i = 0; i < 3; i++) {
        int x = 8 + i * col;
        int sz = i == 2 ? 3 : 4;                                 /* fan has 4 digits: a bit smaller */
        text(g, x, 48, hdr[i], C_DIM, 1);
        text(g, x, 62 + (4 - sz) * 4, vals[i], cols[i], sz);     /* 32px / 24px tall digits */
        text(g, x + (int)strlen(vals[i]) * 6 * sz + 2, 84, unit[i], C_DIM, 1);
    }

    /* bars */
    int y = 110;
    text(g, 8, y, "LOAD", C_DIM, 1);
    bar(g, 64, y, DC_W - 72, 8, load, cols[1]);
    y += 18;
    text(g, 8, y, "TEMP", C_DIM, 1);
    bar(g, 64, y, DC_W - 72, 8, temp, cols[0]);
    y += 18;
    text(g, 8, y, "RAM", C_DIM, 1);
    int rpct = (used >= 0 && total > 0) ? used * 100 / total : 0;
    bar(g, 64, y, DC_W - 72, 8, rpct, C_ACCENT);
    y += 14;
    if (total > 0) {
        snprintf(buf, sizeof buf, "%.1f / %.1f GB", used / 1024.0, total / 1024.0);
        text(g, 64, y, buf, C_DIM, 1);
    }

    /* footer */
    g->fill_rect(0, DC_H - 14, DC_W, 14, C_PANEL);
    fit(buf, sizeof buf, dc_get(s, "OS"), 34);
    g->draw_text(8, DC_H - 11, buf, C_DIM, C_PANEL, 1);
    snprintf(buf, sizeof buf, "#%lu", (unsigned long)s->frames);
    g->draw_text(DC_W - 8 - (int)strlen(buf) * 6, DC_H - 11, buf, C_DIM, C_PANEL, 1);
}

void dc_render(const dc_state *s, const dc_gfx *g, uint32_t now_ms)
{
    g->fill_rect(0, 0, DC_W, DC_H, C_BG);

    if (s->frames == 0) {
        char buf[64];
        text(g, 8, 100, "HW MONITOR", C_ACCENT, 3);
        text(g, 8, 130, "waiting for serial data @115200", C_DIM, 1);
        snprintf(buf, sizeof buf, "up %lus   rx %lu bytes", (unsigned long)(now_ms / 1000), (unsigned long)s->rx_bytes);
        text(g, 8, 150, buf, C_DIM, 1);
        return;
    }
    if (now_ms - s->last_rx_ms > 5000) {
        /* stale: keep last frame but flag it */
        g->fill_rect(0, 0, DC_W, 14, C_HOT);
        g->draw_text(8, 3, "NO SIGNAL - last data shown", C_BG, C_HOT, 1);
        /* draw the rest shifted by nothing; simple approach: fall through */
    }
    const char *src = dc_get(s, "SRC");
    if (!strcmp(src, "OS")) render_os(s, g);
    else render_boot(s, g);
}
