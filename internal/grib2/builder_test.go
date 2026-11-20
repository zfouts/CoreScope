package grib2

import (
	"time"

	"github.com/meshcore-analyzer/grib2/grib2test"
)

// Short names for the shared GRIB2 test builder (grib2test) and the fixed
// sections the negative tests corrupt one field at a time.

var (
	sec           = grib2test.Sec
	be32          = grib2test.Be32
	assemble      = grib2test.Assemble
	noBitmap      = grib2test.NoBitmap
	sectionOffset = grib2test.SectionOffset
)

// bitWriter adapts grib2test.BitWriter to the names the decoder tests use.
type bitWriter struct {
	w   grib2test.BitWriter
	buf []byte
}

func (b *bitWriter) put(v uint64, n uint) { b.w.Put(v, n); b.buf = b.w.Buf }

// testRef is the reference time of every test message: 2026-11-19 18:00 UTC.
var testRef = time.Date(2026, 11, 19, 18, 0, 0, 0, time.UTC)

func testSec1() []byte { return grib2test.Sec1(testRef) }

// testSec3 is a 1° lattice of ni x nj points from 90N 0E.
func testSec3(ni, nj uint32, scan byte) []byte {
	return grib2test.Sec3(grib2test.Lattice{Ni: ni, Nj: nj, La1: 90000000, Lo1: 0, Di: 1000000, Dj: 1000000, Scan: scan})
}

// testSec4 is TMP at 850 hPa, 6 hour forecast.
func testSec4() []byte {
	return grib2test.Sec4(grib2test.Product{Category: 0, Number: 0, ForecastHour: 6, SurfaceType: 100, SurfaceValue: 85000})
}

func testSec5Simple(npts uint32, r float32, e, d int, nbits byte) []byte {
	return grib2test.Sec5Simple(npts, r, e, d, nbits)
}

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
	body := grib2test.Cat(grib2test.Be32(npts), grib2test.Be16(tmpl), grib2test.Be32(0), grib2test.Sm16(0), grib2test.Sm16(0), []byte{c.nbits, 0},
		[]byte{1, c.mvm}, grib2test.Be32(0), grib2test.Be32(0), grib2test.Be32(uint32(len(c.refs))),
		[]byte{c.widthRef, c.widthBits}, grib2test.Be32(c.lenRef), []byte{c.lenIncr}, grib2test.Be32(c.lastLen), []byte{c.lenBits})
	if c.order > 0 {
		body = append(body, c.order, c.octets)
	}
	return grib2test.Sec(5, body)
}

func (c complexSpec) sec7() []byte {
	var w grib2test.BitWriter
	for _, d := range c.descrip {
		n := uint(c.octets) * 8
		v := uint64(d)
		if d < 0 {
			v = uint64(-d) | 1<<(n-1)
		}
		w.Put(v, n)
	}
	for _, r := range c.refs {
		w.Put(r, uint(c.nbits))
	}
	w.Align()
	for _, x := range c.widths {
		w.Put(x, uint(c.widthBits))
	}
	w.Align()
	for _, x := range c.lens {
		w.Put(x, uint(c.lenBits))
	}
	w.Align()
	for g, vs := range c.values {
		for _, v := range vs {
			w.Put(v, c.valWidths[g])
		}
	}
	return grib2test.Sec(7, w.Buf)
}

func (c complexSpec) message() []byte {
	return grib2test.Assemble(testSec1(), testSec3(c.ni, c.nj, 0), testSec4(), c.sec5(), noBitmap(), c.sec7())
}
