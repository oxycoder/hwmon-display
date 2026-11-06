ARCH      := x86_64
EFIINC    := /usr/include/efi
EFILIB    := /usr/lib
CRT0      := $(EFILIB)/crt0-efi-$(ARCH).o
LDS       := $(EFILIB)/elf_$(ARCH)_efi.lds
OVMF_CODE := /usr/share/OVMF/OVMF_CODE_4M.fd
OVMF_VARS := /usr/share/OVMF/OVMF_VARS_4M.fd

CFLAGS  := -I$(EFIINC) -I$(EFIINC)/$(ARCH) -I$(EFIINC)/protocol \
           -fpic -fno-stack-protector -fshort-wchar -mno-red-zone -maccumulate-outgoing-args \
           -DGNU_EFI_USE_MS_ABI -Wall -O2
LDFLAGS := -nostdlib -znocombreloc -T $(LDS) -shared -Bsymbolic -L$(EFILIB) $(CRT0)

all: hwmon.efi

hwmon.o: hwmon.c
	gcc $(CFLAGS) -c $< -o $@

hwmon.so: hwmon.o
	ld $(LDFLAGS) $< -o $@ -lefi -lgnuefi

hwmon.efi: hwmon.so
	objcopy -j .text -j .sdata -j .data -j .rodata -j .dynamic -j .dynsym -j .dynstr \
	        -j .rel -j .rela -j '.rel.*' -j '.rela.*' -j .reloc \
	        --target efi-app-$(ARCH) --subsystem=10 $< $@

# ESP layout: our app as the default boot file. (On a real machine it would live at
# EFI/hwmon/hwmon.efi and chainload the real loader; in QEMU there is nothing to chainload.)
esp: hwmon.efi
	rm -rf esp && mkdir -p esp/EFI/BOOT
	cp hwmon.efi esp/EFI/BOOT/BOOTX64.EFI

# Serial on stdio: what the display would receive is printed to the terminal.
run: esp
	cp $(OVMF_VARS) /tmp/ovmf_vars.fd
	timeout 25 qemu-system-x86_64 -machine q35 -m 4096 -smp 4 -cpu max \
	  -drive if=pflash,format=raw,readonly=on,file=$(OVMF_CODE) \
	  -drive if=pflash,format=raw,file=/tmp/ovmf_vars.fd \
	  -drive format=raw,file=fat:rw:esp \
	  -smbios type=2,manufacturer="ASUS",product="PRIME B650-PLUS" \
	  -smbios type=17,speed=6000 \
	  -display none -serial file:/tmp/ser.log -no-reboot

clean:
	rm -rf *.o *.so *.efi esp
