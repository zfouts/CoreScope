package grib2

import (
	"encoding/binary"
	"math"
)

// A minimal GRIB2 encoder for tests: it builds small synthetic messages so
// the negative tests can corrupt one field at a time.

func sec(n byte, body ...[]byte) []byte {
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

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// sm16/sm32 encode sign-magnitude integers.
func sm16(v int) []byte {
	if v < 0 {
		return be16(uint16(-v) | 0x8000)
	}
	return be16(uint16(v))
}

func sm32(v int64) []byte {
	if v < 0 {
		return be32(uint32(-v) | 0x80000000)
	}
	return be32(uint32(v))
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// assemble wraps sections 1..7 in section 0 and the end marker.
func assemble(secs ...[]byte) []byte {
	body := cat(secs...)
	total := uint64(16 + len(body) + 4)
	out := append([]byte("GRIB"), 0, 0, 0, 2)
	out = binary.BigEndian.AppendUint64(out, total)
	out = append(out, body...)
	return append(out, "7777"...)
}

func testSec1() []byte {
	// centre 7, subcentre 0, master 2, local 1, significance 1,
	// 2026-11-19 18:00:00, status 0, type 1
	return sec(1, be16(7), be16(0), []byte{2, 1, 1}, be16(2026), []byte{11, 19, 18, 0, 0, 0, 1})
}

func testSec3(ni, nj uint32, scan byte) []byte {
	return sec(3,
		[]byte{0}, be32(ni*nj), []byte{0, 0}, be16(0), // source, points, optional list, template 3.0
		[]byte{6}, make([]byte, 15), // earth shape + radius/axes (unused)
		be32(ni), be32(nj), be32(0), be32(math.MaxUint32), // Ni, Nj, basic angle, subdivisions
		sm32(90000000), sm32(0), []byte{0x30}, // La1, Lo1, resolution flags
		sm32(-90000000), sm32(359000000), // La2, Lo2
		be32(1000000), be32(1000000), []byte{scan}) // Di, Dj, scanning mode
}

func testSec4() []byte {
	return sec(4, be16(0), be16(0), // NV, template 4.0
		[]byte{0, 0, 2, 0, 96}, be16(0), []byte{0}, // TMP, process ids, cutoff
		[]byte{1}, be32(6), // hours, 6
		[]byte{100, 0}, be32(85000), // isobaric 85000 Pa
		[]byte{255, 0}, be32(0))
}

func testSec5Simple(npts uint32, r float32, e, d int, nbits byte) []byte {
	return sec(5, be32(npts), be16(0), be32(math.Float32bits(r)), sm16(e), sm16(d), []byte{nbits, 0})
}

func noBitmap() []byte { return sec(6, []byte{255}) }

// bitWriter packs big-endian bit fields.
type bitWriter struct {
	buf  []byte
	nbit uint
}

func (w *bitWriter) put(v uint64, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 != 0 {
			w.buf[len(w.buf)-1] |= 0x80 >> (w.nbit % 8)
		}
		w.nbit++
	}
}

func (w *bitWriter) align() { w.nbit = uint(len(w.buf)) * 8 }

// simpleMessage is a 5.0 message of ni x nj points holding vals with nbits.
func simpleMessage(ni, nj uint32, vals []uint64, nbits byte) []byte {
	var w bitWriter
	for _, v := range vals {
		w.put(v, uint(nbits))
	}
	return assemble(testSec1(), testSec3(ni, nj, 0), testSec4(),
		testSec5Simple(uint32(len(vals)), 0, 0, 0, nbits), noBitmap(), sec(7, w.buf))
}

// complexSpec describes a 5.2 / 5.3 message group by group.
type complexSpec struct {
	ni, nj    uint32
	nbits     byte // group reference width
	mvm       byte
	widthRef  byte
	widthBits byte
	lenRef    uint32
	lenIncr   byte
	lenBits   byte
	lastLen   uint32
	refs      []uint64
	widths    []uint64 // encoded (minus widthRef)
	lens      []uint64 // encoded ((len - lenRef) / lenIncr)
	values    [][]uint64
	valWidths []uint // actual width of each group's values

	order   byte    // 0: template 5.2
	octets  byte    // 5.3 descriptor octets
	descrip []int64 // 5.3: first value(s), then overall minimum
	npts    uint32  // section 5 point count (defaults to ni*nj)
}

func (c complexSpec) sec5() []byte {
	npts := c.npts
	if npts == 0 {
		npts = c.ni * c.nj
	}
	tmpl := uint16(2)
	if c.order > 0 {
		tmpl = 3
	}
	body := cat(be32(npts), be16(tmpl), be32(0), sm16(0), sm16(0), []byte{c.nbits, 0},
		[]byte{1, c.mvm}, be32(0), be32(0), be32(uint32(len(c.refs))),
		[]byte{c.widthRef, c.widthBits}, be32(c.lenRef), []byte{c.lenIncr}, be32(c.lastLen), []byte{c.lenBits})
	if c.order > 0 {
		body = append(body, c.order, c.octets)
	}
	return sec(5, body)
}

func (c complexSpec) sec7() []byte {
	var w bitWriter
	for _, d := range c.descrip {
		n := uint(c.octets) * 8
		v := uint64(d)
		if d < 0 {
			v = uint64(-d) | 1<<(n-1)
		}
		w.put(v, n)
	}
	for _, r := range c.refs {
		w.put(r, uint(c.nbits))
	}
	w.align()
	for _, x := range c.widths {
		w.put(x, uint(c.widthBits))
	}
	w.align()
	for _, x := range c.lens {
		w.put(x, uint(c.lenBits))
	}
	w.align()
	for g, vs := range c.values {
		for _, v := range vs {
			w.put(v, c.valWidths[g])
		}
	}
	return sec(7, w.buf)
}

func (c complexSpec) message() []byte {
	return assemble(testSec1(), testSec3(c.ni, c.nj, 0), testSec4(), c.sec5(), noBitmap(), c.sec7())
}

// sectionOffset returns the offset of section n within msg (or -1).
func sectionOffset(msg []byte, n byte) int {
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
