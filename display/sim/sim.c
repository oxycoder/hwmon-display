/*
 * sim — host-side simulator for the display firmware.
 *
 * Links the exact same display_core.c the ESP32 runs. Reads the serial
 * stream from a tty/pty/file, renders every frame to a 320x240 framebuffer,
 * and writes PNGs so you can look at what the LCD would show.
 *
 *   sim /dev/pts/7 out/              # live, one PNG per frame (frame-0001.png ...)
 *   sim frames.txt out/ --scale 2    # from a captured log
 */
#include "../core/display_core.h"
#include "font6x8.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <time.h>
#include <unistd.h>
#include <fcntl.h>
#include <termios.h>

static uint8_t fb[DC_H][DC_W][3];
static int scale = 2;

static void px(int x, int y, dc_color c)
{
    if (x < 0 || y < 0 || x >= DC_W || y >= DC_H) return;
    fb[y][x][0] = (c >> 8) & 0xF8; fb[y][x][1] = (c >> 3) & 0xFC; fb[y][x][2] = (c << 3) & 0xF8;
}
static void fill_rect(int x, int y, int w, int h, dc_color c)
{
    for (int j = y; j < y + h; j++) for (int i = x; i < x + w; i++) px(i, j, c);
}
static void draw_text(int x, int y, const char *s, dc_color fg, dc_color bg, int size)
{
    for (; *s; s++, x += 6 * size) {
        unsigned ch = (unsigned char)*s; if (ch < 32 || ch > 126) ch = '?';
        const uint8_t *gl = font6x8[ch - 32];
        for (int col = 0; col < 6; col++) {
            uint8_t bits = col < 5 ? gl[col] : 0;
            for (int row = 0; row < 8; row++) {
                dc_color c = (bits >> row) & 1 ? fg : bg;
                fill_rect(x + col * size, y + row * size, size, size, c);
            }
        }
    }
}
static const dc_gfx gfx = { fill_rect, draw_text };

/* ---- minimal PNG writer (stored/uncompressed deflate) ------------------- */
static uint32_t crc_table[256];
static void crc_init(void) { for (uint32_t n = 0; n < 256; n++) { uint32_t c = n; for (int k = 0; k < 8; k++) c = c & 1 ? 0xEDB88320u ^ (c >> 1) : c >> 1; crc_table[n] = c; } }
static uint32_t crc(const uint8_t *b, size_t n, uint32_t c) { c = ~c; while (n--) c = crc_table[(c ^ *b++) & 0xFF] ^ (c >> 8); return ~c; }
static void be32(uint8_t *p, uint32_t v) { p[0] = v >> 24; p[1] = v >> 16; p[2] = v >> 8; p[3] = v; }
static void chunk(FILE *f, const char *type, const uint8_t *data, uint32_t len)
{
    uint8_t hdr[8]; be32(hdr, len); memcpy(hdr + 4, type, 4);
    fwrite(hdr, 1, 8, f); if (len) fwrite(data, 1, len, f);
    uint32_t c = crc((const uint8_t *)type, 4, 0); if (len) c = crc(data, len, c);
    uint8_t cb[4]; be32(cb, c); fwrite(cb, 1, 4, f);
}
static void write_png(const char *path)
{
    int W = DC_W * scale, H = DC_H * scale;
    size_t rowlen = 1 + W * 3, raw_len = rowlen * H;
    uint8_t *raw = malloc(raw_len);
    for (int y = 0; y < H; y++) {
        raw[y * rowlen] = 0;
        for (int x = 0; x < W; x++) memcpy(raw + y * rowlen + 1 + x * 3, fb[y / scale][x / scale], 3);
    }
    /* zlib stream of stored blocks */
    size_t nblocks = (raw_len + 65534) / 65535, zlen = 2 + raw_len + nblocks * 5 + 4;
    uint8_t *z = malloc(zlen), *p = z; *p++ = 0x78; *p++ = 0x01;
    uint32_t a = 1, b = 0;
    for (size_t i = 0; i < raw_len; i++) { a = (a + raw[i]) % 65521; b = (b + a) % 65521; }
    for (size_t off = 0; off < raw_len; off += 65535) {
        size_t n = raw_len - off < 65535 ? raw_len - off : 65535;
        *p++ = off + n == raw_len; *p++ = n; *p++ = n >> 8; *p++ = ~n; *p++ = (~n) >> 8;
        memcpy(p, raw + off, n); p += n;
    }
    be32(p, (b << 16) | a); p += 4;
    FILE *f = fopen(path, "wb"); if (!f) { perror(path); return; }
    fwrite("\x89PNG\r\n\x1a\n", 1, 8, f);
    uint8_t ihdr[13]; be32(ihdr, W); be32(ihdr + 4, H); ihdr[8] = 8; ihdr[9] = 2; ihdr[10] = ihdr[11] = ihdr[12] = 0;
    chunk(f, "IHDR", ihdr, 13); chunk(f, "IDAT", z, p - z); chunk(f, "IEND", NULL, 0);
    fclose(f); free(raw); free(z);
}

static uint32_t now_ms(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return t.tv_sec * 1000 + t.tv_nsec / 1000000; }

int main(int argc, char **argv)
{
    if (argc < 3) { fprintf(stderr, "usage: %s <serial-or-file> <outdir> [--scale N] [--max N]\n", argv[0]); return 2; }
    int max = 0;
    for (int i = 3; i < argc; i++) {
        if (!strcmp(argv[i], "--scale") && i + 1 < argc) scale = atoi(argv[++i]);
        if (!strcmp(argv[i], "--max") && i + 1 < argc) max = atoi(argv[++i]);
    }
    crc_init();
    int fd = open(argv[1], O_RDONLY | O_NOCTTY);
    if (fd < 0) { perror(argv[1]); return 1; }
    struct termios t;
    if (tcgetattr(fd, &t) == 0) {      /* real tty: 115200 8N1 raw, same as the ESP32 UART */
        cfmakeraw(&t); cfsetispeed(&t, B115200); cfsetospeed(&t, B115200);
        t.c_cflag |= CLOCAL | CREAD; tcsetattr(fd, TCSANOW, &t);
    }

    dc_state st; dc_init(&st);
    char path[512]; int n = 0;
    snprintf(path, sizeof path, "%s/frame-0000-idle.png", argv[2]);
    dc_render(&st, &gfx, now_ms()); write_png(path);

    uint8_t buf[256]; ssize_t r;
    while ((r = read(fd, buf, sizeof buf)) > 0) {
        for (ssize_t i = 0; i < r; i++) {
            if (dc_feed(&st, buf[i], now_ms())) {
                if (dc_standby(&st, now_ms(), 0)) {       /* host said SRC=CMD|STANDBY=1: panel goes dark */
                    fill_rect(0, 0, DC_W, DC_H, 0);
                    snprintf(path, sizeof path, "%s/frame-%04d-STANDBY.png", argv[2], ++n);
                    write_png(path);
                    printf("frame %d (STANDBY): display off -> %s\n", n, path);
                    fflush(stdout);
                    continue;
                }
                dc_render(&st, &gfx, now_ms());
                snprintf(path, sizeof path, "%s/frame-%04d-%s.png", argv[2], ++n, dc_get(&st, "SRC"));
                write_png(path);
                printf("frame %d (%s): %d fields -> %s\n", n, dc_get(&st, "SRC"), st.nfields, path);
                fflush(stdout);
                if (max && n >= max) return 0;
            }
        }
    }
    return 0;
}
