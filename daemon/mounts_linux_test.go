//go:build linux

package main

import (
	"reflect"
	"testing"
)

const sampleMounts = `sysfs /sys sysfs rw,nosuid 0 0
proc /proc proc rw 0 0
udev /dev devtmpfs rw 0 0
/dev/nvme0n1p2 / ext4 rw,relatime,errors=remount-ro 0 0
/dev/nvme0n1p1 /boot/efi vfat rw,relatime 0 0
/dev/loop3 /snap/core22/1234 squashfs ro 0 0
/dev/loop7 /snap/firefox/1 squashfs ro 0 0
tmpfs /run/user/1000 tmpfs rw 0 0
/dev/sda1 /home ext4 rw 0 0
/dev/sdb1 /media/tt/My\040USB exfat rw,uid=1000 0 0
/dev/nvme1n1p3 /media/tt/Windows fuseblk rw,user_id=0 0 0
/dev/nvme1n1p4 /mnt/win-data ntfs3 rw 0 0
overlay /var/lib/docker/overlay2/abc/merged overlay rw 0 0
/dev/sdc1 /run/media/tt/CARD vfat rw 0 0
`

func TestParseMounts(t *testing.T) {
	got := parseMounts(sampleMounts)
	want := []string{"/", "/home", "/media/tt/My USB", "/media/tt/Windows", "/mnt/win-data", "/run/media/tt/CARD"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseMounts =\n%q\nwant\n%q", got, want)
	}
	if parseMounts("") == nil || parseMounts("")[0] != "/" {
		t.Fatal("empty mounts should fall back to /")
	}
	if l := diskLabel("/media/tt/Windows"); l != "Windows" {
		t.Fatalf("label %q", l)
	}
}
