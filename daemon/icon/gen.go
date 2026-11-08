//go:build ignore

// gen draws the hwmond application icon and writes:
//
//	hwmond.ico                       multi-size icon (BMP entries up to 48 px, PNG above)
//	hwmond.png                       256 px PNG (Linux desktop / docs)
//	../rsrc_windows_amd64.syso       COFF resource objects that `go build` links into
//	../rsrc_windows_arm64.syso       hwmond.exe: RT_ICON entries + RT_GROUP_ICON id 1
//
// Run by hand from this directory (`go run gen.go`); the outputs are committed.
// No external tools: the .syso is a hand-built COFF object with one .rsrc
// section, one relocation per resource data entry, and one section symbol.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

var (
	frame  = color.NRGBA{0xc8, 0xc8, 0xc8, 0xff}
	screen = color.NRGBA{0x12, 0x1c, 0x30, 0xff}
	bars   = []color.NRGBA{{0x40, 0xd0, 0x50, 0xff}, {0xf0, 0xa8, 0x30, 0xff}, {0x20, 0xb0, 0xe0, 0xff}}
	widths = []float64{0.56, 0.38, 0.70} // bar length as a fraction of the screen width
)

const groupIconID = 1 // the tray loads this group with LoadImage

func main() {
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 128, 256}
	var images [][]byte
	for _, s := range sizes {
		img := render(s)
		if s <= 48 {
			images = append(images, dib(img))
		} else {
			images = append(images, pngBytes(img))
		}
		if s == 256 {
			must(os.WriteFile("hwmond.png", pngBytes(img), 0o644))
		}
	}
	must(os.WriteFile("hwmond.ico", ico(sizes, images), 0o644))
	must(os.WriteFile("../rsrc_windows_amd64.syso", syso(0x8664, 0x0003, sizes, images), 0o644))
	must(os.WriteFile("../rsrc_windows_arm64.syso", syso(0xAA64, 0x0002, sizes, images), 0o644))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func pngBytes(img image.Image) []byte {
	var b bytes.Buffer
	must(png.Encode(&b, img))
	return b.Bytes()
}

// ---------- drawing -----------------------------------------------------------

// render draws at 4x and box-filters down for smooth edges at small sizes.
func render(size int) *image.NRGBA {
	const ss = 4
	n := size * ss
	hi := image.NewNRGBA(image.Rect(0, 0, n, n))
	u := float64(n) / 32 // one pixel of the 32-px tray design
	outer := rrect{1 * u, 1 * u, 31 * u, 31 * u, 3.5 * u}
	inner := rrect{2.5 * u, 2.5 * u, 29.5 * u, 29.5 * u, 2.2 * u}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			switch {
			case inner.contains(px, py):
				hi.SetNRGBA(x, y, screen)
			case outer.contains(px, py):
				hi.SetNRGBA(x, y, frame)
			}
		}
	}
	screenW := 29.5*u - 2.5*u
	for i, w := range widths {
		y0 := (7 + 6*float64(i)) * u
		b := rrect{6 * u, y0, 6*u + w*screenW, y0 + 4*u, 1.2 * u}
		for y := int(y0) - 1; y <= int(y0+4*u)+1; y++ {
			for x := int(6 * u); x <= int(6*u+w*screenW)+1; x++ {
				if b.contains(float64(x)+0.5, float64(y)+0.5) {
					hi.SetNRGBA(x, y, bars[i])
				}
			}
		}
	}
	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					c := hi.NRGBAAt(x*ss+dx, y*ss+dy)
					w := float64(c.A) / 255
					r += float64(c.R) * w
					g += float64(c.G) * w
					b += float64(c.B) * w
					a += w
				}
			}
			if a == 0 {
				continue
			}
			out.SetNRGBA(x, y, color.NRGBA{uint8(r/a + 0.5), uint8(g/a + 0.5), uint8(b/a + 0.5), uint8(a/(ss*ss)*255 + 0.5)})
		}
	}
	return out
}

type rrect struct{ x0, y0, x1, y1, r float64 }

func (q rrect) contains(x, y float64) bool {
	if x < q.x0 || x > q.x1 || y < q.y0 || y > q.y1 {
		return false
	}
	cx := math.Max(q.x0+q.r, math.Min(x, q.x1-q.r))
	cy := math.Max(q.y0+q.r, math.Min(y, q.y1-q.r))
	return math.Hypot(x-cx, y-cy) <= q.r
}

// ---------- ICO / DIB -----------------------------------------------------------

// dib encodes a 32-bit BGRA bottom-up DIB with the 1-bpp AND mask that the
// classic icon format requires (BITMAPINFOHEADER height is doubled).
func dib(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	var b bytes.Buffer
	maskStride := ((w + 31) / 32) * 4
	binary.Write(&b, binary.LittleEndian, struct {
		Size                   uint32
		Width, Height          int32
		Planes, BitCount       uint16
		Compression, SizeImage uint32
		XPels, YPels           int32
		ClrUsed, ClrImportant  uint32
	}{40, int32(w), int32(2 * h), 1, 32, 0, uint32(w*h*4 + maskStride*h), 0, 0, 0, 0})
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(x, y)
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	for y := h - 1; y >= 0; y-- {
		row := make([]byte, maskStride)
		for x := 0; x < w; x++ {
			if img.NRGBAAt(x, y).A == 0 {
				row[x/8] |= 0x80 >> uint(x%8)
			}
		}
		b.Write(row)
	}
	return b.Bytes()
}

func dirEntry(size int, data []byte) [12]byte {
	var e [12]byte
	if size < 256 {
		e[0], e[1] = byte(size), byte(size)
	}
	binary.LittleEndian.PutUint16(e[4:], 1)
	binary.LittleEndian.PutUint16(e[6:], 32)
	binary.LittleEndian.PutUint32(e[8:], uint32(len(data)))
	return e
}

func ico(sizes []int, images [][]byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	off := 6 + 16*len(sizes)
	for i, s := range sizes {
		e := dirEntry(s, images[i])
		b.Write(e[:])
		binary.Write(&b, binary.LittleEndian, uint32(off))
		off += len(images[i])
	}
	for _, im := range images {
		b.Write(im)
	}
	return b.Bytes()
}

// ---------- COFF .syso ------------------------------------------------------------

const (
	rtIcon, rtGroupIcon = 3, 14
	langNeutral         = 0
)

type resource struct {
	typ, id uint32
	data    []byte
}

// syso builds a relocatable COFF object holding only a .rsrc section.
// Layout of the section: root dir → type dirs → id dirs → language dirs →
// data entries → resource bytes; every data entry's OffsetToData gets an
// ADDR32NB relocation against the section symbol so the linker turns the
// section offset into an RVA.
func syso(machine uint16, relocType uint16, sizes []int, images [][]byte) []byte {
	var res []resource
	for i := range images {
		res = append(res, resource{rtIcon, uint32(i + 1), images[i]})
	}
	// GRPICONDIR: header + 14-byte entries (ICONDIRENTRY with a WORD id).
	var grp bytes.Buffer
	binary.Write(&grp, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	for i, s := range sizes {
		e := dirEntry(s, images[i])
		grp.Write(e[:])
		binary.Write(&grp, binary.LittleEndian, uint16(i+1))
	}
	res = append(res, resource{rtGroupIcon, groupIconID, grp.Bytes()})

	// Group by type (sorted ids), as the directory tree requires.
	types := []uint32{rtIcon, rtGroupIcon}
	byType := map[uint32][]resource{}
	for _, r := range res {
		byType[r.typ] = append(byType[r.typ], r)
	}
	const dirHdr, entry, dataEntry = 16, 8, 16
	// Sizes of each level.
	rootSize := dirHdr + entry*len(types)
	typeDirsSize := 0
	for _, t := range types {
		typeDirsSize += dirHdr + entry*len(byType[t])
	}
	idDirsSize := (dirHdr + entry) * len(res) // one language entry each
	dataEntriesOff := rootSize + typeDirsSize + idDirsSize
	dataOff := dataEntriesOff + dataEntry*len(res)

	var sec bytes.Buffer
	dir := func(n int) {
		binary.Write(&sec, binary.LittleEndian, struct {
			Chars, Stamp         uint32
			Major, Minor, Nn, Ni uint16
		}{0, 0, 0, 0, 0, uint16(n)})
	}
	ent := func(id, off uint32, sub bool) {
		if sub {
			off |= 0x80000000
		}
		binary.Write(&sec, binary.LittleEndian, [2]uint32{id, off})
	}
	// Root.
	dir(len(types))
	off := rootSize
	for _, t := range types {
		ent(t, uint32(off), true)
		off += dirHdr + entry*len(byType[t])
	}
	// Type dirs → id dirs (one per resource, in flattened order).
	idDirOff := rootSize + typeDirsSize
	k := 0
	for _, t := range types {
		dir(len(byType[t]))
		for _, r := range byType[t] {
			ent(r.id, uint32(idDirOff+k*(dirHdr+entry)), true)
			k++
		}
	}
	// Id dirs → language → data entry.
	type relocAt struct{ off uint32 }
	var relocs []relocAt
	flat := []resource{}
	for _, t := range types {
		flat = append(flat, byType[t]...)
	}
	for i := range flat {
		dir(1)
		ent(langNeutral, uint32(dataEntriesOff+i*dataEntry), false)
	}
	// Data entries.
	cur := dataOff
	for _, r := range flat {
		relocs = append(relocs, relocAt{uint32(sec.Len())})
		binary.Write(&sec, binary.LittleEndian, [4]uint32{uint32(cur), uint32(len(r.data)), 0, 0})
		cur += pad8(len(r.data))
	}
	for _, r := range flat {
		sec.Write(r.data)
		sec.Write(make([]byte, pad8(len(r.data))-len(r.data)))
	}

	// Assemble COFF: header, section header, raw data, relocations, symbols, string table.
	const fileHdr, secHdr = 20, 40
	rawOff := fileHdr + secHdr
	relocOff := rawOff + sec.Len()
	symOff := relocOff + 10*len(relocs)
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, struct {
		Machine, NSections          uint16
		Stamp, SymPtr, NSyms        uint32
		OptHdrSize, Characteristics uint16
	}{machine, 1, 0, uint32(symOff), 1, 0, 0x0004 /*IMAGE_FILE_LINE_NUMS_STRIPPED*/})
	var name [8]byte
	copy(name[:], ".rsrc")
	binary.Write(&out, binary.LittleEndian, struct {
		Name                          [8]byte
		VSize, VAddr, RawSize, RawPtr uint32
		RelocPtr, LinePtr             uint32
		NRelocs, NLines               uint16
		Characteristics               uint32
	}{name, 0, 0, uint32(sec.Len()), uint32(rawOff), uint32(relocOff), 0, uint16(len(relocs)), 0,
		0x40000040 /*IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ*/})
	out.Write(sec.Bytes())
	for _, r := range relocs {
		binary.Write(&out, binary.LittleEndian, r.off)
		binary.Write(&out, binary.LittleEndian, uint32(0)) // symbol 0: the section
		binary.Write(&out, binary.LittleEndian, relocType)
	}
	// One symbol: the .rsrc section (static), no aux records.
	out.Write(name[:])
	binary.Write(&out, binary.LittleEndian, struct {
		Value   uint32
		Section int16
		Type    uint16
		Class   uint8
		NAux    uint8
	}{0, 1, 0, 3 /*IMAGE_SYM_CLASS_STATIC*/, 0})
	binary.Write(&out, binary.LittleEndian, uint32(4)) // empty string table
	return out.Bytes()
}

func pad8(n int) int { return (n + 7) &^ 7 }
