#include "display_core.h"
#include <string.h>
#include <stdio.h>
#include <stdlib.h>

/* ---------- parsing ------------------------------------------------------- */

void dc_init(dc_state *s) { memset(s, 0, sizeof(*s)); s->bl_req = -1; }

/* Value of `|KEY=` in a raw (unsplit) line, or NULL. The value runs up to the
   next '|' or the end of the line — fine for the numeric CMD arguments. */
static const char *cmd_val(const char *line, const char *key)
{
    size_t n = strlen(key);
    for (const char *p = line; (p = strchr(p, '|')) != NULL; p++)
        if (!strncmp(p + 1, key, n) && p[1 + n] == '=') return p + 2 + n;
    return NULL;
}

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
            if (!memcmp(s->line, "SRC=CMD", 7) && (s->line[7] == '|' || s->line[7] == 0)) {
                /* Control frame from the host: does not replace the data on
                   screen, so the last readings come back on wake. */
                const char *v;
                if ((v = cmd_val(s->line, "STANDBY")) != NULL) {
                    s->standby_req = atoi(v) != 0;
                    if (!s->standby_req) s->last_rx_ms = now_ms;   /* STANDBY=0: wake without new data */
                    ok = 1;
                }
                if ((v = cmd_val(s->line, "INFO")) != NULL && atoi(v)) { s->info_req = 1; ok = 1; }
                if ((v = cmd_val(s->line, "BL")) != NULL) {
                    int n = atoi(v);
                    s->bl_req = (int16_t)(n < 0 ? 0 : n > 100 ? 100 : n);
                    ok = 1;
                }
            } else {
                parse_line(s);
                s->frames++;
                s->last_rx_ms = now_ms;
                s->standby_req = 0;          /* any data frame wakes the panel */
                ok = s->nfields > 0;
            }
        }
        s->linelen = 0;
        return ok;
    }
    if (s->linelen < DC_MAX_LINE - 1) s->line[s->linelen++] = (char)b;
    else s->linelen = 0;                     /* overflow: drop the garbage line */
    return 0;
}

int dc_standby(const dc_state *s, uint32_t now_ms, uint32_t timeout_ms)
{
    if (s->standby_req) return 1;
    return timeout_ms && now_ms - s->last_rx_ms >= timeout_ms;
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

/* Remove every occurrence of `sub` from `s` in place. */
static void strip_sub(char *s, const char *sub)
{
    size_t n = strlen(sub);
    char *p;
    while ((p = strstr(s, sub)) != NULL) memmove(p, p + n, strlen(p + n) + 1);
}

/* "AMD Ryzen 9 3900X 12-Core Processor" -> "Ryzen 9 3900X",
   "Intel(R) Core(TM) i7-9700K CPU @ 3.60GHz" -> "Core i7-9700K",
   "NVIDIA GeForce RTX 4070" -> "GeForce RTX 4070". */
static void short_chip(char *dst, int dstsz, const char *src)
{
    snprintf(dst, dstsz, "%s", src);
    const char *junk[] = { "AMD ", "Intel(R) ", "Intel ", "NVIDIA ", "(R)", "(TM)", " Processor", " CPU", " Graphics Family" };
    for (unsigned i = 0; i < sizeof junk / sizeof junk[0]; i++) strip_sub(dst, junk[i]);
    char *p = strstr(dst, "-Core");                 /* " 12-Core" */
    if (p) {
        char *q = p;
        while (q > dst && q[-1] != ' ') q--;
        if (q > dst) q--;
        memmove(q, p + 5, strlen(p + 5) + 1);
    }
    if ((p = strstr(dst, " @")) != NULL) *p = 0;    /* " @ 3.60GHz" */
    char *w = dst;                                  /* collapse runs of spaces, trim */
    for (char *r = dst; *r; r++) if (!(*r == ' ' && (w == dst || w[-1] == ' '))) *w++ = *r;
    while (w > dst && w[-1] == ' ') w--;
    *w = 0;
}

/* One "big number" cell: small label, large value, small unit after it. */
static void cell(const dc_gfx *g, int x, int y, const char *label, const char *val, dc_color c, int sz, const char *unit)
{
    text(g, x, y, label, C_DIM, 1);
    text(g, x, y + 12 + (4 - sz) * 4, val, c, sz);                   /* 32px (sz 4) / 24px (sz 3) digits */
    text(g, x + (int)strlen(val) * 6 * sz + 2, y + 36, unit, C_DIM, 1); /* unit sits on the baseline */
}

static void num_val(char *out, int sz, int t)    { if (t >= 0) snprintf(out, sz, "%d", t); else snprintf(out, sz, "--"); }
static dc_color load_color(int l) { return l < 0 ? C_DIM : l < 70 ? C_OK : l < 90 ? C_WARN : C_HOT; }
static dc_color power_color(int w) { return w < 0 ? C_DIM : C_ACCENT; }

/* Memory cell: "14.4" big + "/32GB" small; colour by fill percentage. */
static void mem_cell(const dc_gfx *g, int x, int y, const char *label, int used_mb, int total_mb)
{
    char val[16], unit[16];
    dc_color c = C_DIM;
    if (used_mb >= 0 && total_mb > 0) {
        snprintf(val, sizeof val, "%.1f", used_mb / 1024.0);
        snprintf(unit, sizeof unit, "/%dGB", (total_mb + 512) / 1024);
        int pct = used_mb * 100 / total_mb;
        c = pct < 70 ? C_ACCENT : pct < 90 ? C_WARN : C_HOT;
    } else {
        snprintf(val, sizeof val, "--");
        unit[0] = 0;
    }
    cell(g, x, y, label, val, c, 3, unit);
}

static void render_os(const dc_state *s, const dc_gfx *g)
{
    char buf[80], v[16];
    int load = ival(s, "LOAD", -1), temp = ival(s, "TEMP", -1), fan = ival(s, "FAN", -1);
    int used = ival(s, "RAM_USED_MB", -1), total = ival(s, "RAM_MB", -1);
    int gload = ival(s, "GPU_LOAD", -1), gtemp = ival(s, "GPU_TEMP", -1);
    int vused = ival(s, "VRAM_USED_MB", -1), vtotal = ival(s, "VRAM_MB", -1);
    int cpuw = ival(s, "CPU_W", -1), gpuw = ival(s, "GPU_W", -1);   /* package power, watts */

    /* header: "<CPU> - <GPU>" on one line (52 chars max at 6px/char) */
    char cpu[64], gpu[64];
    short_chip(cpu, sizeof cpu, dc_get(s, "CPU"));
    short_chip(gpu, sizeof gpu, dc_get(s, "GPU"));
    int x = 8;
    if (*gpu) {
        fit(buf, sizeof buf, cpu, 24);
        text(g, x, 6, buf, C_ACCENT, 1); x += (int)strlen(buf) * 6;
        text(g, x, 6, " - ", C_DIM, 1);   x += 18;
        fit(buf, sizeof buf, gpu, (DC_W - 8 - x) / 6);
        text(g, x, 6, buf, C_TEXT, 1);
    } else {
        fit(buf, sizeof buf, cpu, 50);
        text(g, x, 6, buf, C_ACCENT, 1);
    }
    g->fill_rect(0, 18, DC_W, 1, C_PANEL);

    /* row 1: CPU temp / load / power / RAM used;  row 2: GPU temp / load / power / VRAM used.
       Four columns at 18px digits: "68"+C, "100"+%, "450"+W, "14.4"+"/32GB" all fit their slot. */
    const int cx[4] = { 8, 66, 136, 210 };
    int y = 26;
    num_val(v, sizeof v, temp);  cell(g, cx[0], y, "CPU TEMP", v, temp_color(temp), 3, "C");
    num_val(v, sizeof v, load);  cell(g, cx[1], y, "CPU LOAD", v, load_color(load), 3, "%");
    num_val(v, sizeof v, cpuw);  cell(g, cx[2], y, "CPU PWR", v, power_color(cpuw), 3, "W");
    mem_cell(g, cx[3], y, "RAM USED", used, total);
    y += 54;
    num_val(v, sizeof v, gtemp); cell(g, cx[0], y, "GPU TEMP", v, temp_color(gtemp), 3, "C");
    num_val(v, sizeof v, gload); cell(g, cx[1], y, "GPU LOAD", v, load_color(gload), 3, "%");
    num_val(v, sizeof v, gpuw);  cell(g, cx[2], y, "GPU PWR", v, power_color(gpuw), 3, "W");
    mem_cell(g, cx[3], y, "VRAM USED", vused, vtotal);
    y += 54;
    g->fill_rect(0, y - 2, DC_W, 1, C_PANEL);

    /* DISKS: "C:62:931,D:40:1863" = name : used% : total GB; temps in DISK_T="38,35" */
    y += 4;
    const char *dk = dc_get(s, "DISK");
    const char *dt = dc_get(s, "DISK_T");
    if (*dk) {
        text(g, 8, y, "DISKS", C_DIM, 1);
        y += 14;
        char list[64]; snprintf(list, sizeof list, "%s", dk);
        char tlist[32]; snprintf(tlist, sizeof tlist, "%s", dt);
        char *save = NULL, *tsave = NULL;
        char *tok = strtok_r(list, ",", &save);
        char *ttok = strtok_r(tlist, ",", &tsave);
        for (int n = 0; tok && n < 4 && y < DC_H - 30; n++) {
            char name[16] = ""; int pct = -1, gb = -1;
            char *c1 = strchr(tok, ':'); char *c2 = c1 ? strchr(c1 + 1, ':') : NULL;
            if (c1) { snprintf(name, sizeof name, "%.*s", (int)(c1 - tok), tok); pct = atoi(c1 + 1); }
            if (c2) gb = atoi(c2 + 1);
            if (!strcmp(name, "/")) snprintf(name, sizeof name, "root");
            if (name[0] >= 'A' && name[0] <= 'Z' && name[1] == 0) snprintf(name, sizeof name, "%c:", name[0]);
            char lbl[12]; fit(lbl, sizeof lbl, name, 6);
            text(g, 8, y, lbl, C_TEXT, 1);
            dc_color dc = pct < 0 ? C_DIM : pct < 80 ? C_ACCENT : pct < 92 ? C_WARN : C_HOT;
            bar(g, 56, y, 150, 8, pct, dc);
            if (gb >= 1000) snprintf(buf, sizeof buf, "%d%%  %.1f TB", pct, gb / 1000.0);
            else            snprintf(buf, sizeof buf, "%d%%  %d GB", pct, gb);
            if (ttok) { size_t l = strlen(buf); snprintf(buf + l, sizeof buf - l, "  %sC", ttok); }
            text(g, 214, y, buf, C_DIM, 1);
            y += 14;
            tok = strtok_r(NULL, ",", &save);
            ttok = ttok ? strtok_r(NULL, ",", &tsave) : NULL;
        }
    }

    /* footer: OS name | CPU fan | total (CPU + GPU) power, right-aligned */
    g->fill_rect(0, DC_H - 14, DC_W, 14, C_PANEL);
    fit(buf, sizeof buf, dc_get(s, "OS"), 26);
    g->draw_text(8, DC_H - 11, buf, C_DIM, C_PANEL, 1);
    if (fan > 0) {
        snprintf(buf, sizeof buf, "FAN %drpm", fan);
        g->draw_text(170, DC_H - 11, buf, C_DIM, C_PANEL, 1);
    }
    if (cpuw >= 0 || gpuw >= 0) {
        snprintf(buf, sizeof buf, "PWR %dW", (cpuw > 0 ? cpuw : 0) + (gpuw > 0 ? gpuw : 0));
        g->draw_text(DC_W - 8 - (int)strlen(buf) * 6, DC_H - 11, buf, C_TEXT, C_PANEL, 1);
    }
}

/* ---------- TEXT page: whatever the host wants to show ----------------------
 * SRC=TEXT|TITLE=Build|L1=#ok:main passing|L2=#hot:release FAILED|L3=...|SIZE=2|COLOR=text|ALIGN=center
 * Lines are word-wrapped to the screen width at the chosen font size and drawn
 * until the screen is full. An empty / blank line value leaves a gap. */

static dc_color color_named(const char *n, dc_color def)
{
    if (!strcmp(n, "accent")) return C_ACCENT;
    if (!strcmp(n, "ok"))     return C_OK;
    if (!strcmp(n, "warn"))   return C_WARN;
    if (!strcmp(n, "hot"))    return C_HOT;
    if (!strcmp(n, "dim"))    return C_DIM;
    if (!strcmp(n, "text"))   return C_TEXT;
    return def;
}

/* "#warn:disk almost full" -> colour C_WARN, text after the colon. Anything
   that is not a known colour name stays literal. */
static const char *line_color(const char *v, dc_color def, dc_color *out)
{
    *out = def;
    if (v[0] != '#') return v;
    const char *colon = strchr(v, ':');
    if (!colon || colon - v > 8) return v;
    char name[8];
    snprintf(name, sizeof name, "%.*s", (int)(colon - v - 1), v + 1);
    dc_color c = color_named(name, 0xFFFF);
    if (c == 0xFFFF) return v;
    *out = c;
    return colon + 1;
}

static void render_text(const dc_state *s, const dc_gfx *g)
{
    char buf[80];
    int size = ival(s, "SIZE", 2);
    if (size < 1) size = 1;
    if (size > 4) size = 4;
    int center = dc_get(s, "ALIGN")[0] == 'c';
    dc_color def = color_named(dc_get(s, "COLOR"), C_TEXT);

    int y = 6;
    const char *title = dc_get(s, "TITLE");
    if (*title) {
        fit(buf, sizeof buf, title, (DC_W - 16) / 12);
        text(g, center ? (DC_W - (int)strlen(buf) * 12) / 2 : 8, y, buf, C_ACCENT, 2);
        y += 22;
        g->fill_rect(0, y, DC_W, 1, C_PANEL);
        y += 6;
    }

    int cw = 6 * size, ch = 8 * size, lh = ch + (size > 1 ? 4 : 3);
    int maxc = (DC_W - 16) / cw;
    for (int i = 1; i <= 12 && y + ch <= DC_H; i++) {
        char key[8];
        snprintf(key, sizeof key, "L%d", i);
        dc_color col;
        const char *p = line_color(dc_get(s, key), def, &col);
        while (*p == ' ') p++;
        if (!*p) { y += lh; continue; }                    /* blank line: just a gap */
        while (*p && y + ch <= DC_H) {
            int n = (int)strlen(p);
            if (n > maxc) {                                 /* wrap at the last space that fits */
                int k = maxc;
                while (k > 0 && p[k] != ' ') k--;
                n = k > 0 ? k : maxc;
            }
            snprintf(buf, sizeof buf, "%.*s", n, p);
            text(g, center ? (DC_W - n * cw) / 2 : 8, y, buf, col, size);
            y += lh;
            p += n;
            while (*p == ' ') p++;
        }
    }
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
    else if (!strcmp(src, "TEXT")) render_text(s, g);
    else render_boot(s, g);
}
