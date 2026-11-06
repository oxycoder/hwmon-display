// TFT_eSPI User_Setup.h for the ESP32-2432S028 (Cheap Yellow Display).
// Copy this over <Arduino libraries>/TFT_eSPI/User_Setup.h (or include it from User_Setup_Select.h).
#define USER_SETUP_INFO "ESP32-2432S028 CYD"

#define ILI9341_2_DRIVER       // the CYD's ILI9341 variant

#define TFT_WIDTH  240
#define TFT_HEIGHT 320

#define TFT_MISO 12
#define TFT_MOSI 13
#define TFT_SCLK 14
#define TFT_CS   15
#define TFT_DC    2
#define TFT_RST  -1
#define TFT_BL   21
#define TFT_BACKLIGHT_ON HIGH

#define TOUCH_CS 33            // XPT2046, not used by this firmware but keep it deselected

#define LOAD_GLCD              // font 1: the 6x8 bitmap font the layout is designed around
#define LOAD_FONT2
#define LOAD_GFXFF
#define SMOOTH_FONT

#define SPI_FREQUENCY       55000000
#define SPI_READ_FREQUENCY  20000000
#define SPI_TOUCH_FREQUENCY  2500000
