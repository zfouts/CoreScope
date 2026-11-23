// Package grib2test builds small synthetic GRIB2 messages for tests: the
// low-level section helpers the grib2 decoder tests use to corrupt one field
// at a time, and Message, which encodes a whole simple-packing (template
// 5.0) field on a regular lat/lon lattice for the ingestor's fake NOAA
// server. Test-only; nothing here validates its input.
package grib2test

import (
	"encoding/binary"
	"math"
	"time"
)

// Sec returns section n holding the concatenated body parts, with its
// length prefix.
func Sec(n byte, body ...[]byte) []byte {
	l := 5
	for _, b := range body {
		l += len(b)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(l))
	out = append(out, n)
	for _, b := range body {
		out = append(out, b...)
	}
	return out
}

func Be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func Be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// Sm16 and Sm32 encode sign-magnitude integers.
func Sm16(v int) []byte {
	if v < 0 {
		return Be16(uint16(-v) | 0x8000)
	}
	return Be16(uint16(v))
}

func Sm32(v int64) []byte {
	if v < 0 {
		return Be32(uint32(-v) | 0x80000000)
	}
	return Be32(uint32(v))
}

// Cat concatenates byte slices.
func Cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Assemble wraps sections 1..7 in section 0 (discipline 0) and the end marker.
func Assemble(secs ...[]byte) []byte {
	body := Cat(secs...)
	total := uint64(16 + len(body) + 4)
	out := append([]byte("GRIB"), 0, 0, 0, 2)
	out = binary.BigEndian.AppendUint64(out, total)
	out = append(out, body...)
	return append(out, "7777"...)
}

// Sec1 is an identification section (centre 7, NCEP) with reference time ref.
func Sec1(ref time.Time) []byte {
	ref = ref.UTC()
	return Sec(1, Be16(7), Be16(0), []byte{2, 1, 1}, Be16(uint16(ref.Year())),
		[]byte{byte(ref.Month()), byte(ref.Day()), byte(ref.Hour()), byte(ref.Minute()), byte(ref.Second()), 0, 1})
}

// Lattice is a regular lat/lon grid in micro-degrees. Di and Dj are the
// unsigned increments; Scan is the scanning mode (0 = rows north to south,
// 0x40 = south to north).
type Lattice struct {
	Ni, Nj   uint32
	La1, Lo1 int64
	Di, Dj   uint32
	Scan     byte
}

// Sec3 is a grid definition section, template 3.0.
func Sec3(g Lattice) []byte {
	return Sec(3,
		[]byte{0}, Be32(g.Ni*g.Nj), []byte{0, 0}, Be16(0), // source, points, optional list, template 3.0
		[]byte{6}, make([]byte, 15), // earth shape + radius/axes (unused)
		Be32(g.Ni), Be32(g.Nj), Be32(0), Be32(math.MaxUint32), // Ni, Nj, basic angle, subdivisions
		Sm32(g.La1), Sm32(g.Lo1), []byte{0x30}, // La1, Lo1, resolution flags
		Sm32(-g.La1), Sm32(g.Lo1+int64(g.Ni-1)*int64(g.Di)), // La2, Lo2 (informational only)
		Be32(g.Di), Be32(g.Dj), []byte{g.Scan}) // Di, Dj, scanning mode
}

// Product describes a product definition section, template 4.0.
type Product struct {
	Category, Number uint8
	ForecastHour     uint32
	SurfaceType      uint8
	SurfaceScale     uint8  // decimal scale factor of the surface value
	SurfaceValue     uint32 // scaled value; with SurfaceScale 0xff and value 0xffffffff "missing"
}

// Sec4 is a product definition section, template 4.0 (forecast time in hours).
func Sec4(p Product) []byte {
	return Sec(4, Be16(0), Be16(0), // NV, template 4.0
		[]byte{p.Category, p.Number, 2, 0, 96}, Be16(0), []byte{0}, // parameter, process ids, cutoff
		[]byte{1}, Be32(p.ForecastHour), // hours
		[]byte{p.SurfaceType, p.SurfaceScale}, Be32(p.SurfaceValue),
		[]byte{255, 0}, Be32(0))
}

// Sec5Simple is a data representation section, template 5.0.
func Sec5Simple(npts uint32, r float32, e, d int, nbits byte) []byte {
	return Sec(5, Be32(npts), Be16(0), Be32(math.Float32bits(r)), Sm16(e), Sm16(d), []byte{nbits, 0})
}

// NoBitmap is section 6 with indicator 255 (no bitmap).
func NoBitmap() []byte { return Sec(6, []byte{255}) }

// BitWriter packs big-endian bit fields.
type BitWriter struct {
	Buf  []byte
	nbit uint
}

// Put appends the low n bits of v.
func (w *BitWriter) Put(v uint64, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.Buf = append(w.Buf, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.Buf[len(w.Buf)-1] |= 0x80 >> (w.nbit % 8)
		}
		w.nbit++
	}
}

// Align pads to the next whole octet.
func (w *BitWriter) Align() { w.nbit = uint(len(w.Buf)) * 8 }

// SectionOffset returns the offset of section n within msg (or -1).
func SectionOffset(msg []byte, n byte) int {
	off := 16
	for off+5 <= len(msg)-4 {
		l := int(binary.BigEndian.Uint32(msg[off:]))
		if msg[off+4] == n {
			return off
		}
		if l < 5 {
			return -1
		}
		off += l
	}
	return -1
}

// Field is one message for Message: values on a regular lattice in degrees
// (DLat negative for rows north to south, as in GFS), with the parameter,
// level and times the decoder reports back. NaN values are written as
// missing through a bitmap.
type Field struct {
	Ni, Nj           int
	La1, Lo1         float64
	DLat, DLon       float64
	Ref              time.Time
	ForecastHour     int
	Category, Number uint8
	SurfaceType      uint8
	// SurfaceValue is the level in SI units (Pa for isobaric, m above
	// ground); NaN writes the "missing" value.
	SurfaceValue float64
	Values       []float64
}

// decimalScale is the decimal scale factor D of Message: values keep two
// decimals.
const decimalScale = 2

// Message encodes f as one simple-packing (template 5.0) GRIB2 message.
func Message(f Field) []byte {
	scan := byte(0)
	if f.DLat > 0 {
		scan = 0x40
	}
	g := Lattice{
		Ni: uint32(f.Ni), Nj: uint32(f.Nj),
		La1: int64(math.Round(f.La1 * 1e6)), Lo1: int64(math.Round(f.Lo1 * 1e6)),
		Di: uint32(math.Round(math.Abs(f.DLon) * 1e6)), Dj: uint32(math.Round(math.Abs(f.DLat) * 1e6)),
		Scan: scan,
	}
	p := Product{Category: f.Category, Number: f.Number, ForecastHour: uint32(f.ForecastHour), SurfaceType: f.SurfaceType}
	if math.IsNaN(f.SurfaceValue) {
		p.SurfaceScale, p.SurfaceValue = 0xff, math.MaxUint32
	} else {
		p.SurfaceValue = uint32(math.Round(f.SurfaceValue))
	}

	// Y = (R + X) * 10^-D with R the scaled minimum.
	scaled := make([]float64, 0, len(f.Values))
	var bitmap BitWriter
	hasMissing := false
	ref := math.Inf(1)
	for _, v := range f.Values {
		if math.IsNaN(v) {
			hasMissing = true
			bitmap.Put(0, 1)
			continue
		}
		bitmap.Put(1, 1)
		s := math.Round(v * math.Pow10(decimalScale))
		scaled = append(scaled, s)
		ref = math.Min(ref, s)
	}
	if len(scaled) == 0 {
		ref = 0
	}
	r := float32(ref)
	var maxX uint64
	xs := make([]uint64, len(scaled))
	for i, s := range scaled {
		x := uint64(math.Max(0, math.Round(s-float64(r))))
		xs[i] = x
		if x > maxX {
			maxX = x
		}
	}
	nbits := byte(0)
	for maxX>>nbits != 0 {
		nbits++
	}
	var data BitWriter
	for _, x := range xs {
		data.Put(x, uint(nbits))
	}
	sec6 := NoBitmap()
	if hasMissing {
		sec6 = Sec(6, []byte{0}, bitmap.Buf)
	}
	return Assemble(Sec1(f.Ref), Sec3(g), Sec4(p), Sec5Simple(uint32(len(xs)), r, 0, decimalScale, nbits), sec6, Sec(7, data.Buf))
}
