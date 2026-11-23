package grib2

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

const maxSections = 10 // sections 1..7 of one field, with room to reject a second field cleanly

// sections holds the raw bytes of each section of one message (header
// included, so in-section offsets match the WMO octet numbers minus one).
type sections struct {
	s1, s3, s4, s5, s6, s7 []byte
}

// Decode decodes exactly one GRIB2 message. A malformed message returns an
// error wrapping ErrCorrupt, ErrUnsupported or ErrTooLarge; it never panics.
func Decode(msg []byte) (*Field, error) {
	secs, discipline, err := splitSections(msg)
	if err != nil {
		return nil, err
	}
	f := &Field{Discipline: discipline}
	if err := parseIdentification(secs.s1, f); err != nil {
		return nil, err
	}
	npts, err := parseGrid(secs.s3, f)
	if err != nil {
		return nil, err
	}
	if err := parseProduct(secs.s4, f); err != nil {
		return nil, err
	}
	bitmap, set, err := parseBitmap(secs.s6, npts)
	if err != nil {
		return nil, err
	}
	want := npts
	if bitmap != nil {
		want = set
	}
	d, err := parseDataRepresentation(secs.s5, want)
	if err != nil {
		return nil, err
	}
	f.Values, err = unpack(d, secs.s7[5:], bitmap, npts)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// splitSections validates section 0 and the end marker and walks sections
// 1..7 with a strictly advancing offset.
func splitSections(msg []byte) (sections, uint8, error) {
	var s sections
	if len(msg) > MaxMessageBytes {
		return s, 0, fmt.Errorf("%w: message is %d bytes", ErrTooLarge, len(msg))
	}
	if len(msg) < 16+4 || !bytes.Equal(msg[:4], []byte("GRIB")) {
		return s, 0, fmt.Errorf("%w: missing GRIB indicator", ErrCorrupt)
	}
	if msg[7] != 2 {
		return s, 0, fmt.Errorf("%w: GRIB edition %d", ErrUnsupported, msg[7])
	}
	total := binary.BigEndian.Uint64(msg[8:16])
	if total > MaxMessageBytes {
		return s, 0, fmt.Errorf("%w: declared length %d", ErrTooLarge, total)
	}
	if total != uint64(len(msg)) {
		return s, 0, fmt.Errorf("%w: declared length %d, have %d bytes", ErrCorrupt, total, len(msg))
	}
	end := len(msg) - 4
	if !bytes.Equal(msg[end:], []byte("7777")) {
		return s, 0, fmt.Errorf("%w: missing end section 7777", ErrCorrupt)
	}

	// allowed reports whether section n may follow section prev.
	allowed := func(prev, n uint8) bool {
		switch prev {
		case 0:
			return n == 1
		case 1:
			return n == 2 || n == 3
		default:
			return n == prev+1
		}
	}
	off, count := 16, 0
	var prev uint8
	for off < end {
		if count == maxSections {
			return s, 0, fmt.Errorf("%w: more than %d sections", ErrCorrupt, maxSections)
		}
		if end-off < 5 {
			return s, 0, fmt.Errorf("%w: truncated section header at %d", ErrCorrupt, off)
		}
		l := binary.BigEndian.Uint32(msg[off:])
		n := msg[off+4]
		if l < 5 || uint64(l) > uint64(end-off) {
			return s, 0, fmt.Errorf("%w: section %d length %d at %d", ErrCorrupt, n, l, off)
		}
		if prev == 7 && n >= 2 && n <= 7 {
			return s, 0, fmt.Errorf("%w: more than one field in a message", ErrUnsupported)
		}
		if !allowed(prev, n) {
			return s, 0, fmt.Errorf("%w: section %d after section %d", ErrCorrupt, n, prev)
		}
		sec := msg[off : off+int(l)]
		switch n {
		case 1:
			s.s1 = sec
		case 3:
			s.s3 = sec
		case 4:
			s.s4 = sec
		case 5:
			s.s5 = sec
		case 6:
			s.s6 = sec
		case 7:
			s.s7 = sec
		}
		prev = n
		off += int(l)
		count++
	}
	if prev != 7 {
		return s, 0, fmt.Errorf("%w: message ends after section %d", ErrCorrupt, prev)
	}
	return s, msg[6], nil
}

func parseIdentification(s []byte, f *Field) error {
	if len(s) < 21 {
		return fmt.Errorf("%w: section 1 length %d", ErrCorrupt, len(s))
	}
	year, month, day := int(u16(s, 12)), int(s[14]), int(s[15])
	hour, minute, second := int(s[16]), int(s[17]), int(s[18])
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return fmt.Errorf("%w: invalid reference time", ErrCorrupt)
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	if t.Day() != day {
		return fmt.Errorf("%w: invalid reference date", ErrCorrupt)
	}
	f.RefTime = t.Unix()
	return nil
}

// parseGrid decodes grid definition template 3.0 and returns ni*nj.
func parseGrid(s []byte, f *Field) (int, error) {
	if len(s) < 14 {
		return 0, fmt.Errorf("%w: section 3 length %d", ErrCorrupt, len(s))
	}
	if s[5] != 0 {
		return 0, fmt.Errorf("%w: grid definition source %d", ErrUnsupported, s[5])
	}
	if tmpl := u16(s, 12); tmpl != 0 {
		return 0, fmt.Errorf("%w: grid template 3.%d", ErrUnsupported, tmpl)
	}
	if s[10] != 0 {
		return 0, fmt.Errorf("%w: grid with an optional point list", ErrUnsupported)
	}
	if len(s) < 72 {
		return 0, fmt.Errorf("%w: section 3 length %d for template 3.0", ErrCorrupt, len(s))
	}
	// uint64 cannot overflow: (2^32-1)^2 < 2^64.
	ni, nj := uint64(u32(s, 30)), uint64(u32(s, 34))
	if ni == 0 || nj == 0 {
		return 0, fmt.Errorf("%w: empty grid %dx%d", ErrCorrupt, ni, nj)
	}
	n := ni * nj
	if n > MaxPoints {
		return 0, fmt.Errorf("%w: grid %dx%d", ErrTooLarge, ni, nj)
	}
	if uint64(u32(s, 6)) != n {
		return 0, fmt.Errorf("%w: section 3 point count %d != %dx%d", ErrCorrupt, u32(s, 6), ni, nj)
	}
	// Basic angle 0 (or missing) means coordinates are in micro-degrees.
	if ba := u32(s, 38); ba != 0 && ba != math.MaxUint32 {
		return 0, fmt.Errorf("%w: basic angle %d", ErrUnsupported, ba)
	}
	di, dj := u32(s, 63), u32(s, 67)
	if di == math.MaxUint32 || dj == math.MaxUint32 {
		return 0, fmt.Errorf("%w: grid increments not given", ErrUnsupported)
	}
	scan := s[71]
	if scan != 0x00 && scan != 0x40 {
		return 0, fmt.Errorf("%w: scanning mode 0x%02x", ErrUnsupported, scan)
	}
	f.Ni, f.Nj = int(ni), int(nj)
	f.La1 = float64(s32(s, 46)) / 1e6
	f.Lo1 = float64(s32(s, 50)) / 1e6
	f.DLon = float64(di) / 1e6
	f.DLat = float64(dj) / 1e6
	if scan&0x40 == 0 {
		f.DLat = -f.DLat // rows run north to south
	}
	return int(n), nil
}

// parseProduct decodes product definition template 4.0.
func parseProduct(s []byte, f *Field) error {
	if len(s) < 9 {
		return fmt.Errorf("%w: section 4 length %d", ErrCorrupt, len(s))
	}
	if tmpl := u16(s, 7); tmpl != 0 {
		return fmt.Errorf("%w: product template 4.%d", ErrUnsupported, tmpl)
	}
	if len(s) < 34 {
		return fmt.Errorf("%w: section 4 length %d for template 4.0", ErrCorrupt, len(s))
	}
	f.ParameterCategory = s[9]
	f.ParameterNumber = s[10]
	ft := s32(s, 18)
	switch s[17] {
	case 0: // minutes
		if ft%60 != 0 {
			return fmt.Errorf("%w: forecast time of %d minutes is not whole hours", ErrUnsupported, ft)
		}
		ft /= 60
	case 1: // hours
	case 2: // days
		ft *= 24
	default:
		return fmt.Errorf("%w: forecast time unit %d", ErrUnsupported, s[17])
	}
	f.ForecastHour = int(ft)
	f.SurfaceType = s[22]
	// Scale factor and scaled value all-ones means "missing" (e.g. a
	// surface with no value); leave SurfaceValue at 0 then.
	if s[23] != 0xff && u32(s, 24) != math.MaxUint32 {
		f.SurfaceValue = float64(s32(s, 24)) * math.Pow(10, -float64(s8(s, 23)))
	}
	return nil
}

// parseBitmap returns the bitmap (nil when absent) and its set-bit count
// over the first npts bits.
func parseBitmap(s []byte, npts int) ([]byte, int, error) {
	if len(s) < 6 {
		return nil, 0, fmt.Errorf("%w: section 6 length %d", ErrCorrupt, len(s))
	}
	switch s[5] {
	case 255:
		return nil, 0, nil
	case 0:
	default:
		return nil, 0, fmt.Errorf("%w: bitmap indicator %d", ErrUnsupported, s[5])
	}
	bm := s[6:]
	if len(bm) != (npts+7)/8 {
		return nil, 0, fmt.Errorf("%w: bitmap is %d bytes for %d points", ErrCorrupt, len(bm), npts)
	}
	set := 0
	for i := 0; i < npts/8; i++ {
		set += popcount8[bm[i]]
	}
	if rem := npts % 8; rem != 0 {
		set += popcount8[bm[npts/8]&^(0xff>>rem)]
	}
	return bm, set, nil
}

var popcount8 = func() (t [256]int) {
	for i := range t {
		for v := i; v != 0; v &= v - 1 {
			t[i]++
		}
	}
	return
}()

// drs is a validated section 5 (templates 5.0, 5.2, 5.3).
type drs struct {
	template uint16
	ndpts    int
	ref      float64 // R
	bscale   float64 // 2^E
	dscale   float64 // 10^-D
	nbits    uint

	// 5.2 / 5.3
	mvm       uint8
	ngroups   uint64
	widthRef  uint32
	widthBits uint
	lenRef    uint64
	lenIncr   uint64
	lastLen   uint64
	lenBits   uint
	order     int  // 5.3 spatial differencing order (0 for 5.2)
	descBits  uint // 5.3 extra descriptor width in bits
}

func parseDataRepresentation(s []byte, want int) (*drs, error) {
	if len(s) < 11 {
		return nil, fmt.Errorf("%w: section 5 length %d", ErrCorrupt, len(s))
	}
	d := &drs{template: u16(s, 9)}
	switch d.template {
	case 0:
		if len(s) < 21 {
			return nil, fmt.Errorf("%w: section 5 length %d for template 5.0", ErrCorrupt, len(s))
		}
	case 2:
		if len(s) < 47 {
			return nil, fmt.Errorf("%w: section 5 length %d for template 5.2", ErrCorrupt, len(s))
		}
	case 3:
		if len(s) < 49 {
			return nil, fmt.Errorf("%w: section 5 length %d for template 5.3", ErrCorrupt, len(s))
		}
	default:
		return nil, fmt.Errorf("%w: data representation template 5.%d", ErrUnsupported, d.template)
	}
	if n := u32(s, 5); uint64(n) != uint64(want) {
		return nil, fmt.Errorf("%w: section 5 has %d points, expected %d", ErrCorrupt, n, want)
	}
	d.ndpts = want
	r := math.Float32frombits(u32(s, 11))
	if math.IsNaN(float64(r)) || math.IsInf(float64(r), 0) {
		return nil, fmt.Errorf("%w: reference value is not finite", ErrCorrupt)
	}
	d.ref = float64(r)
	e, dec := s16(s, 15), s16(s, 17)
	if e < -30 || e > 30 || dec < -30 || dec > 30 {
		return nil, fmt.Errorf("%w: scale factors E=%d D=%d", ErrUnsupported, e, dec)
	}
	d.bscale = math.Ldexp(1, e)
	d.dscale = math.Pow(10, -float64(dec))
	if s[19] > MaxBitsPerValue {
		return nil, fmt.Errorf("%w: %d bits per value", ErrUnsupported, s[19])
	}
	d.nbits = uint(s[19])
	if d.template == 0 {
		return d, nil
	}

	d.mvm = s[22]
	if d.mvm > 2 {
		return nil, fmt.Errorf("%w: missing value management %d", ErrUnsupported, d.mvm)
	}
	d.ngroups = uint64(u32(s, 31))
	if d.ngroups == 0 || d.ngroups > MaxGroups || d.ngroups > uint64(d.ndpts) {
		return nil, fmt.Errorf("%w: %d groups for %d points", ErrCorrupt, d.ngroups, d.ndpts)
	}
	d.widthRef = uint32(s[35])
	d.widthBits = uint(s[36])
	d.lenRef = uint64(u32(s, 37))
	d.lenIncr = uint64(s[41])
	d.lastLen = uint64(u32(s, 42))
	d.lenBits = uint(s[46])
	if d.widthRef > MaxBitsPerValue || d.widthBits > MaxBitsPerValue || d.lenBits > MaxBitsPerValue {
		return nil, fmt.Errorf("%w: group width reference %d, width bits %d, length bits %d",
			ErrUnsupported, d.widthRef, d.widthBits, d.lenBits)
	}
	if d.template == 3 {
		d.order = int(s[47])
		if d.order != 1 && d.order != 2 {
			return nil, fmt.Errorf("%w: spatial differencing order %d", ErrUnsupported, d.order)
		}
		oct := uint(s[48])
		if oct < 1 || oct > 4 {
			return nil, fmt.Errorf("%w: %d extra descriptor octets", ErrUnsupported, oct)
		}
		d.descBits = oct * 8
	}
	return d, nil
}

// emitter writes decoded values into consecutive grid points, skipping
// points whose bitmap bit is off.
type emitter struct {
	out    []float32
	bitmap []byte
	pos    int
}

func (e *emitter) put(v float32) error {
	if e.bitmap != nil {
		for e.pos < len(e.out) && e.bitmap[e.pos>>3]&(0x80>>(e.pos&7)) == 0 {
			e.pos++
		}
	}
	if e.pos >= len(e.out) {
		return fmt.Errorf("%w: more values than grid points", ErrCorrupt)
	}
	e.out[e.pos] = v
	e.pos++
	return nil
}

// value scales a packed integer and rejects non-finite results.
func (d *drs) value(x float64) (float32, error) {
	v := float32((d.ref + x*d.bscale) * d.dscale)
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return 0, fmt.Errorf("%w: decoded value is not finite", ErrCorrupt)
	}
	return v, nil
}

var nan32 = float32(math.NaN())

func unpack(d *drs, data []byte, bitmap []byte, npts int) ([]float32, error) {
	r := newBitReader(data)
	// Check the packed bits exist before allocating the output.
	if d.template == 0 {
		if uint64(d.ndpts)*uint64(d.nbits) > r.nbits {
			return nil, fmt.Errorf("%w: section 7 holds %d bits, need %d", ErrCorrupt, r.nbits, uint64(d.ndpts)*uint64(d.nbits))
		}
	} else if err := checkComplexHeader(d, r); err != nil {
		return nil, err
	}

	out := make([]float32, npts)
	if bitmap != nil {
		for i := range out {
			out[i] = nan32
		}
	}
	em := &emitter{out: out, bitmap: bitmap}
	var err error
	if d.template == 0 {
		err = unpackSimple(d, r, em)
	} else {
		err = unpackComplex(d, r, em)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func unpackSimple(d *drs, r *bitReader, em *emitter) error {
	for i := 0; i < d.ndpts; i++ {
		x, err := r.read(d.nbits)
		if err != nil {
			return err
		}
		v, err := d.value(float64(x))
		if err != nil {
			return err
		}
		if err := em.put(v); err != nil {
			return err
		}
	}
	return nil
}

// complexLayout gives the bit offsets of the four sub-blocks of a complex
// packed section 7: group references, group widths, group lengths, values.
func complexLayout(d *drs) (refs, widths, lengths, values uint64) {
	if d.order > 0 { // 5.3: first value(s) and overall minimum precede the groups
		refs = uint64(d.order+1) * uint64(d.descBits)
	}
	widths = refs + roundUp8(d.ngroups*uint64(d.nbits))
	lengths = widths + roundUp8(d.ngroups*uint64(d.widthBits))
	values = lengths + roundUp8(d.ngroups*uint64(d.lenBits))
	return
}

func checkComplexHeader(d *drs, r *bitReader) error {
	_, _, _, values := complexLayout(d)
	if values > r.nbits {
		return fmt.Errorf("%w: section 7 holds %d bits, group descriptors need %d", ErrCorrupt, r.nbits, values)
	}
	return nil
}

// groupReaders reads the per-group reference, width and length streams.
type groupReaders struct {
	d                    *drs
	refs, widths, length *bitReader
}

func newGroupReaders(d *drs, data []byte) (*groupReaders, error) {
	refs, widths, lengths, _ := complexLayout(d)
	g := &groupReaders{d: d, refs: newBitReader(data), widths: newBitReader(data), length: newBitReader(data)}
	if err := g.refs.seek(refs); err != nil {
		return nil, err
	}
	if err := g.widths.seek(widths); err != nil {
		return nil, err
	}
	if err := g.length.seek(lengths); err != nil {
		return nil, err
	}
	return g, nil
}

// next returns group i's width and length (the reference is read separately).
func (g *groupReaders) next(i uint64) (width uint, length uint64, err error) {
	w, err := g.widths.read(g.d.widthBits)
	if err != nil {
		return 0, 0, err
	}
	width64 := uint64(g.d.widthRef) + uint64(w)
	if width64 > MaxBitsPerValue {
		return 0, 0, fmt.Errorf("%w: group width %d", ErrCorrupt, width64)
	}
	l, err := g.length.read(g.d.lenBits)
	if err != nil {
		return 0, 0, err
	}
	length = g.d.lenRef + uint64(l)*g.d.lenIncr
	if i == g.d.ngroups-1 {
		length = g.d.lastLen
	}
	return uint(width64), length, nil
}

func unpackComplex(d *drs, r *bitReader, em *emitter) error {
	data := r.buf
	_, _, _, valStart := complexLayout(d)

	// Pass 1: validate group lengths and the total packed value bits
	// before decoding anything.
	g, err := newGroupReaders(d, data)
	if err != nil {
		return err
	}
	var total, valueBits uint64
	for i := uint64(0); i < d.ngroups; i++ {
		w, l, err := g.next(i)
		if err != nil {
			return err
		}
		total += l
		if total > uint64(d.ndpts) {
			return fmt.Errorf("%w: group lengths exceed %d points", ErrCorrupt, d.ndpts)
		}
		valueBits += l * uint64(w)
	}
	if total != uint64(d.ndpts) {
		return fmt.Errorf("%w: group lengths sum to %d, expected %d", ErrCorrupt, total, d.ndpts)
	}
	if valStart+valueBits > r.nbits {
		return fmt.Errorf("%w: section 7 holds %d bits, need %d", ErrCorrupt, r.nbits, valStart+valueBits)
	}

	// Spatial differencing descriptors (5.3).
	var first [2]int64
	var minsd int64
	if d.order > 0 {
		for k := 0; k < d.order; k++ {
			v, err := r.read(d.descBits)
			if err != nil {
				return err
			}
			first[k] = signMagnitude(v, d.descBits)
		}
		v, err := r.read(d.descBits)
		if err != nil {
			return err
		}
		minsd = signMagnitude(v, d.descBits)
	}

	// Pass 2: decode.
	g, err = newGroupReaders(d, data)
	if err != nil {
		return err
	}
	vals := newBitReader(data)
	if err := vals.seek(valStart); err != nil {
		return err
	}
	primary := uint64(1)<<d.nbits - 1 // missing markers for constant groups
	var prev1, prev2 int64
	present := 0 // non-missing values seen, for spatial differencing
	for i := uint64(0); i < d.ngroups; i++ {
		ref32, err := g.refs.read(d.nbits)
		if err != nil {
			return err
		}
		ref := uint64(ref32)
		w, l, err := g.next(i)
		if err != nil {
			return err
		}
		// Within a group of width w, all-ones (and, for mvm 2, all-ones
		// minus one) mark missing values.
		groupMiss := uint64(1)<<w - 1
		for j := uint64(0); j < l; j++ {
			var x uint64
			missing := false
			if w == 0 {
				x = ref
				missing = d.mvm >= 1 && ref == primary || d.mvm == 2 && ref == primary-1
			} else {
				v, err := vals.read(w)
				if err != nil {
					return err
				}
				x = uint64(v)
				missing = d.mvm >= 1 && x == groupMiss || d.mvm == 2 && x == groupMiss-1
				x += ref
			}
			if missing {
				if err := em.put(nan32); err != nil {
					return err
				}
				continue
			}
			ix := int64(x)
			if d.order > 0 {
				switch {
				case present < d.order:
					ix = first[present]
				case d.order == 1:
					ix += minsd + prev1
				default:
					ix += minsd + 2*prev1 - prev2
				}
				prev2, prev1 = prev1, ix
			}
			present++
			v, err := d.value(float64(ix))
			if err != nil {
				return err
			}
			if err := em.put(v); err != nil {
				return err
			}
		}
	}
	return nil
}
