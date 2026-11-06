/*
 * hwmon.efi — boot-time hardware info → serial (16550 @ 0x3F8)
 *
 * Runs before any OS. Reads CPUID, SMBIOS, the UEFI memory map and PCI,
 * writes one "KEY=VAL|KEY=VAL|...\n" line to COM1, mirrors it on screen,
 * then chainloads the real bootloader (\EFI\BOOT\BOOTX64.EFI by default;
 * override with CHAINLOAD_PATH below or by passing a path in LoadOptions).
 *
 * Build: make      Test: make run   (QEMU + OVMF, serial on stdio)
 */
#include <efi.h>
#include <efilib.h>

/* ---------- 16550 UART, programmed directly, no firmware driver ---------- */

static UINT16 g_uart = 0x3F8;      /* COM1; overridden if a PCI serial card is found */
static int    g_uart_mmio = 0;     /* 1 if BAR is memory-mapped (most PCIe cards are I/O) */

static inline void outb(UINT16 port, UINT8 v)  { __asm__ volatile("outb %0,%1" :: "a"(v), "Nd"(port)); }
static inline UINT8 inb(UINT16 port)           { UINT8 v; __asm__ volatile("inb %1,%0" : "=a"(v) : "Nd"(port)); return v; }

static void uart_init(void)
{
    outb(g_uart + 1, 0x00);   /* IER: no interrupts            */
    outb(g_uart + 3, 0x80);   /* LCR: DLAB on                   */
    outb(g_uart + 0, 0x01);   /* DLL: divisor 1 → 115200 baud   */
    outb(g_uart + 1, 0x00);   /* DLM                            */
    outb(g_uart + 3, 0x03);   /* LCR: 8N1, DLAB off             */
    outb(g_uart + 2, 0xC7);   /* FCR: FIFO on, clear, 14-byte   */
    outb(g_uart + 4, 0x0B);   /* MCR: DTR|RTS|OUT2              */
}

static void uart_putc(char c)
{
    UINTN spin = 100000;
    while (!(inb(g_uart + 5) & 0x20) && --spin) ;   /* LSR THRE */
    outb(g_uart, (UINT8)c);
}

static void uart_puts(const char *s) { while (*s) uart_putc(*s++); }

/* ---------- tiny ASCII line builder (we print to both UART and console) --- */

static char  g_line[1024];
static UINTN g_len = 0;

static void emit(const char *s)
{
    while (*s && g_len < sizeof(g_line) - 1) g_line[g_len++] = *s++;
    g_line[g_len] = 0;
}
static void emit_u(UINT64 v)
{
    char b[24]; int i = 23; b[i] = 0;
    if (!v) b[--i] = '0';
    while (v) { b[--i] = '0' + v % 10; v /= 10; }
    emit(&b[i]);
}
static void emit_hex(UINT32 v, int digits)
{
    static const char h[] = "0123456789abcdef";
    char b[12]; b[digits] = 0;
    for (int i = digits - 1; i >= 0; i--) { b[i] = h[v & 0xF]; v >>= 4; }
    emit(b);
}
/* copy a string, trimming spaces and replacing protocol delimiters */
static void emit_clean(const char *s)
{
    while (*s == ' ') s++;
    const char *end = s; const char *last = s;
    while (*end) { if (*end != ' ') last = end + 1; end++; }
    for (; s < last && g_len < sizeof(g_line) - 1; s++) {
        char c = *s;
        if (c == '|' || c == '=' || c == '\n' || c == '\r') c = ' ';
        if (c < 0x20 || c > 0x7E) c = '?';
        g_line[g_len++] = c;
    }
    g_line[g_len] = 0;
}

/* ---------- CPUID ------------------------------------------------------- */

static inline void cpuid(UINT32 leaf, UINT32 sub, UINT32 *a, UINT32 *b, UINT32 *c, UINT32 *d)
{
    __asm__ volatile("cpuid" : "=a"(*a), "=b"(*b), "=c"(*c), "=d"(*d) : "a"(leaf), "c"(sub));
}

static void read_cpu(void)
{
    UINT32 a, b, c, d;
    char brand[49]; UINT32 *p = (UINT32 *)brand;

    cpuid(0x80000000, 0, &a, &b, &c, &d);
    if (a >= 0x80000004) {
        for (UINT32 leaf = 0x80000002; leaf <= 0x80000004; leaf++) {
            cpuid(leaf, 0, &a, &b, &c, &d);
            *p++ = a; *p++ = b; *p++ = c; *p++ = d;
        }
        brand[48] = 0;
        emit("CPU="); emit_clean(brand);
    } else {
        char vendor[13]; UINT32 *v = (UINT32 *)vendor;
        cpuid(0, 0, &a, &b, &c, &d);
        v[0] = b; v[1] = d; v[2] = c; vendor[12] = 0;
        emit("CPU="); emit_clean(vendor);
    }

    /* logical processor count (leaf 0xB if present, else leaf 1) */
    UINT32 threads = 0;
    cpuid(0, 0, &a, &b, &c, &d);
    if (a >= 0xB) {
        cpuid(0xB, 1, &a, &b, &c, &d);      /* level 1 = core level: logical procs per package */
        threads = b & 0xFFFF;
    }
    if (!threads) { cpuid(1, 0, &a, &b, &c, &d); threads = (b >> 16) & 0xFF; }
    emit("|THREADS="); emit_u(threads);
}

/* ---------- SMBIOS ------------------------------------------------------ */

typedef struct { UINT8 type, length; UINT16 handle; } SMB_HDR;

static const char *smb_string(const SMB_HDR *h, UINT8 idx)
{
    if (!idx) return "";
    const char *s = (const char *)h + h->length;
    while (--idx) { while (*s) s++; s++; if (!*s) return ""; }
    return s;
}
static const SMB_HDR *smb_next(const SMB_HDR *h)
{
    const char *s = (const char *)h + h->length;
    while (s[0] || s[1]) s++;
    return (const SMB_HDR *)(s + 2);
}

static void read_smbios(void)
{
    EFI_GUID g3 = SMBIOS3_TABLE_GUID, g2 = SMBIOS_TABLE_GUID;
    const UINT8 *table = NULL; UINTN len = 0;

    for (UINTN i = 0; i < ST->NumberOfTableEntries; i++) {
        EFI_CONFIGURATION_TABLE *t = &ST->ConfigurationTable[i];
        if (!CompareGuid(&t->VendorGuid, &g3)) {
            const UINT8 *ep = t->VendorTable;            /* "_SM3_" */
            table = (const UINT8 *)(UINTN)*(UINT64 *)(ep + 0x10);
            len   = *(UINT32 *)(ep + 0x0C);
            break;
        }
        if (!CompareGuid(&t->VendorGuid, &g2)) {
            const UINT8 *ep = t->VendorTable;            /* "_SM_" */
            table = (const UINT8 *)(UINTN)*(UINT32 *)(ep + 0x18);
            len   = *(UINT16 *)(ep + 0x16);
        }
    }
    if (!table) { emit("|BOARD=?|BIOS=?"); return; }

    UINT64 dimm_total_mb = 0; UINT32 dimm_speed = 0; int dimms = 0;
    const SMB_HDR *h = (const SMB_HDR *)table;
    const UINT8 *end = table + len;

    while ((const UINT8 *)h + sizeof(*h) < end && h->type != 127) {
        const UINT8 *b = (const UINT8 *)h;
        switch (h->type) {
        case 0:   /* BIOS */
            emit("|BIOS="); emit_clean(smb_string(h, b[4])); emit(" "); emit_clean(smb_string(h, b[5]));
            break;
        case 2:   /* Baseboard */
            emit("|BOARD="); emit_clean(smb_string(h, b[4])); emit(" "); emit_clean(smb_string(h, b[5]));
            break;
        case 17: { /* Memory device */
            UINT16 size = *(UINT16 *)(b + 0x0C);
            if (size == 0 || size == 0xFFFF) break;        /* empty slot / unknown */
            UINT64 mb;
            if (size == 0x7FFF && h->length >= 0x20) mb = *(UINT32 *)(b + 0x1C);
            else mb = (size & 0x8000) ? (size & 0x7FFF) / 1024 : size;
            dimm_total_mb += mb; dimms++;
            if (h->length >= 0x17) { UINT16 sp = *(UINT16 *)(b + 0x15); if (sp > dimm_speed) dimm_speed = sp; }
            break;
        }
        }
        h = smb_next(h);
    }
    emit("|DIMMS="); emit_u(dimms);
    emit("|DIMM_MB="); emit_u(dimm_total_mb);
    emit("|DIMM_MHZ="); emit_u(dimm_speed);
}

/* ---------- UEFI memory map → usable RAM --------------------------------- */

static void read_memmap(void)
{
    UINTN size = 0, key, dsz; UINT32 dver;
    EFI_MEMORY_DESCRIPTOR *map = NULL;
    uefi_call_wrapper(BS->GetMemoryMap, 5, &size, map, &key, &dsz, &dver);
    size += 2 * dsz;
    map = AllocatePool(size);
    if (!map || EFI_ERROR(uefi_call_wrapper(BS->GetMemoryMap, 5, &size, map, &key, &dsz, &dver))) {
        emit("|RAM_MB=?"); return;
    }
    UINT64 pages = 0;
    for (UINTN off = 0; off < size; off += dsz) {
        EFI_MEMORY_DESCRIPTOR *d = (EFI_MEMORY_DESCRIPTOR *)((UINT8 *)map + off);
        switch (d->Type) {
        case EfiConventionalMemory: case EfiLoaderCode: case EfiLoaderData:
        case EfiBootServicesCode: case EfiBootServicesData:
        case EfiRuntimeServicesCode: case EfiRuntimeServicesData: case EfiACPIReclaimMemory:
            pages += d->NumberOfPages;
        }
    }
    FreePool(map);
    emit("|RAM_MB="); emit_u(pages * 4096 / (1024 * 1024));
}

/* ---------- PCI: find GPU, NIC, and (optionally) a serial card ------------ */

static void read_pci(void)
{
    EFI_GUID pciio = EFI_PCI_IO_PROTOCOL_GUID;
    UINTN n = 0; EFI_HANDLE *handles = NULL;
    if (EFI_ERROR(uefi_call_wrapper(BS->LocateHandleBuffer, 5, ByProtocol, &pciio, NULL, &n, &handles))) {
        emit("|GPU=?"); return;
    }
    int gpu = 0, nic = 0, disks = 0;
    for (UINTN i = 0; i < n; i++) {
        EFI_PCI_IO *io;
        if (EFI_ERROR(uefi_call_wrapper(BS->HandleProtocol, 3, handles[i], &pciio, (void **)&io))) continue;
        UINT32 id, cls;
        uefi_call_wrapper(io->Pci.Read, 5, io, EfiPciIoWidthUint32, 0x00, 1, &id);
        uefi_call_wrapper(io->Pci.Read, 5, io, EfiPciIoWidthUint32, 0x08, 1, &cls);
        UINT8 base = cls >> 24, sub = (cls >> 16) & 0xFF;
        if (base == 0x03 && !gpu++) { emit("|GPU="); emit_hex(id & 0xFFFF, 4); emit(":"); emit_hex(id >> 16, 4); }
        if (base == 0x02 && !nic++) { emit("|NIC="); emit_hex(id & 0xFFFF, 4); emit(":"); emit_hex(id >> 16, 4); }
        if (base == 0x01) disks++;
        /* Serial controller (class 07, subclass 00): use its BAR0 I/O port as the UART.
           Only taken if it's an I/O BAR; COM1 at 0x3F8 stays the default otherwise. */
        if (base == 0x07 && sub == 0x00) {
            UINT32 bar0;
            uefi_call_wrapper(io->Pci.Read, 5, io, EfiPciIoWidthUint32, 0x10, 1, &bar0);
            if (bar0 & 1) { g_uart = bar0 & 0xFFFC; g_uart_mmio = 0; uart_init(); }
        }
    }
    FreePool(handles);
    emit("|STORAGE_CTRL="); emit_u(disks);
}

/* ---------- chainload --------------------------------------------------- */

#define CHAINLOAD_PATH L"\\EFI\\BOOT\\BOOTX64.EFI"

static EFI_STATUS chainload(EFI_HANDLE image)
{
    EFI_LOADED_IMAGE *li;
    EFI_GUID lip = LOADED_IMAGE_PROTOCOL;
    if (EFI_ERROR(uefi_call_wrapper(BS->HandleProtocol, 3, image, &lip, (void **)&li))) return EFI_NOT_FOUND;

    CHAR16 *path = CHAINLOAD_PATH;
    if (li->LoadOptions && li->LoadOptionsSize > 2) path = (CHAR16 *)li->LoadOptions;

    EFI_DEVICE_PATH *dp = FileDevicePath(li->DeviceHandle, path);
    EFI_HANDLE next;
    EFI_STATUS st = uefi_call_wrapper(BS->LoadImage, 6, FALSE, image, dp, NULL, 0, &next);
    if (EFI_ERROR(st)) return st;
    return uefi_call_wrapper(BS->StartImage, 3, next, NULL, NULL);
}

/* ---------- main -------------------------------------------------------- */

/* NOTE: no EFIAPI here — gnu-efi's crt0 calls efi_main with the SysV convention. */
EFI_STATUS efi_main(EFI_HANDLE image, EFI_SYSTEM_TABLE *systab)
{
    InitializeLib(image, systab);
    uart_init();

    emit("SRC=UEFI|");
    read_cpu();
    read_memmap();
    read_smbios();
    read_pci();
    emit("\n");

    uart_puts(g_line);

    /* mirror on the console so you can see it even without the display */
    Print(L"hwmon.efi: %a", g_line);

    /* keep it readable for a moment, then hand off */
    uefi_call_wrapper(BS->Stall, 1, 1500000);
    EFI_STATUS st = chainload(image);
    Print(L"hwmon.efi: chainload returned %r — falling back to firmware boot order\n", st);
    uefi_call_wrapper(BS->Stall, 1, 2000000);
    return EFI_SUCCESS;   /* returning lets the firmware continue its normal boot order */
}
