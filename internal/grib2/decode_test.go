package grib2

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// reference.json is produced by testdata/gen_reference.py with ecCodes, an
// independent decoder; see that script for how the fixtures were made.
type refField struct {
	File         string        `json:"file"`
	DRT          int           `json:"drt"`
	Bitmap       bool          `json:"bitmap"`
	Discipline   uint8         `json:"discipline"`
	Category     uint8         `json:"category"`
	Number       uint8         `json:"number"`
	SurfaceType  uint8         `json:"surfaceType"`
	SurfaceValue float64       `json:"surfaceValue"`
	RefTime      int64         `json:"refTime"`
	ForecastHour int           `json:"forecastHour"`
	Ni           int           `json:"ni"`
	Nj           int           `json:"nj"`
	La1          float64       `json:"la1"`
	Lo1          float64       `json:"lo1"`
	DLat         float64       `json:"dlat"`
	DLon         float64       `json:"dlon"`
	Missing      int           `json:"missing"`
	Min          float64       `json:"min"`
	Max          float64       `json:"max"`
	Mean         float64       `json:"mean"`
	Samples      [][2]*float64 `json:"samples"`
}

func loadReference(t testing.TB) []refField {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Fields []refField `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Fields) == 0 {
		t.Fatal("reference.json has no fields")
	}
	return doc.Fields
}

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func near(got, want float64) bool {
	return math.Abs(got-want) <= 1e-3*math.Abs(want)+1e-6
}

func TestDecodeMatchesECCodes(t *testing.T) {
	templates := map[int]bool{}
	for _, ref := range loadReference(t) {
		ref := ref
		templates[ref.DRT] = true
		t.Run(ref.File, func(t *testing.T) {
			f, err := Decode(readFixture(t, ref.File))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if f.Discipline != ref.Discipline || f.ParameterCategory != ref.Category || f.ParameterNumber != ref.Number {
				t.Errorf("parameter %d/%d/%d, want %d/%d/%d", f.Discipline, f.ParameterCategory, f.ParameterNumber,
					ref.Discipline, ref.Category, ref.Number)
			}
			if f.SurfaceType != ref.SurfaceType || !near(f.SurfaceValue, ref.SurfaceValue) {
				t.Errorf("surface %d %v, want %d %v", f.SurfaceType, f.SurfaceValue, ref.SurfaceType, ref.SurfaceValue)
			}
			if f.RefTime != ref.RefTime || f.ForecastHour != ref.ForecastHour {
				t.Errorf("time %d +%dh, want %d +%dh", f.RefTime, f.ForecastHour, ref.RefTime, ref.ForecastHour)
			}
			if f.Ni != ref.Ni || f.Nj != ref.Nj || f.La1 != ref.La1 || f.Lo1 != ref.Lo1 || f.DLat != ref.DLat || f.DLon != ref.DLon {
				t.Errorf("grid %dx%d at (%v,%v) step (%v,%v), want %dx%d at (%v,%v) step (%v,%v)",
					f.Ni, f.Nj, f.La1, f.Lo1, f.DLat, f.DLon, ref.Ni, ref.Nj, ref.La1, ref.Lo1, ref.DLat, ref.DLon)
			}
			if len(f.Values) != ref.Ni*ref.Nj {
				t.Fatalf("%d values, want %d", len(f.Values), ref.Ni*ref.Nj)
			}
			missing, sum := 0, 0.0
			lo, hi := math.Inf(1), math.Inf(-1)
			for _, v := range f.Values {
				x := float64(v)
				if math.IsNaN(x) {
					missing++
					continue
				}
				sum += x
				lo, hi = math.Min(lo, x), math.Max(hi, x)
			}
			if missing != ref.Missing {
				t.Errorf("%d missing, want %d", missing, ref.Missing)
			}
			mean := sum / float64(len(f.Values)-missing)
			if !near(lo, ref.Min) || !near(hi, ref.Max) || !near(mean, ref.Mean) {
				t.Errorf("min/max/mean %v/%v/%v, want %v/%v/%v", lo, hi, mean, ref.Min, ref.Max, ref.Mean)
			}
			if len(ref.Samples) < 20 {
				t.Errorf("only %d reference samples", len(ref.Samples))
			}
			for _, s := range ref.Samples {
				i := int(*s[0])
				got := float64(f.Values[i])
				if s[1] == nil {
					if !math.IsNaN(got) {
						t.Errorf("value[%d] = %v, want missing", i, got)
					}
					continue
				}
				if !near(got, *s[1]) {
					t.Errorf("value[%d] = %v, want %v", i, got, *s[1])
				}
			}
		})
	}
	for _, drt := range []int{0, 2, 3} {
		if !templates[drt] {
			t.Errorf("no reference fixture for template 5.%d", drt)
		}
	}
}

func TestDecodeSimpleSynthetic(t *testing.T) {
	f, err := Decode(simpleMessage(3, 2, []uint64{0, 1, 2, 3, 4, 5}, 3))
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range f.Values {
		if v != float32(i) {
			t.Errorf("value[%d] = %v", i, v)
		}
	}
	if f.SurfaceType != 100 || f.SurfaceValue != 85000 || f.ForecastHour != 6 || f.DLat != -1 || f.La1 != 90 {
		t.Errorf("metadata %+v", f)
	}

	// nbits 0: a constant field with no packed data at all.
	msg := assemble(testSec1(), testSec3(2, 2, 0), testSec4(), testSec5Simple(4, 12.5, 0, 1, 0), noBitmap(), sec(7))
	f, err = Decode(msg)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range f.Values {
		if v != 1.25 {
			t.Errorf("constant value[%d] = %v", i, v)
		}
	}
}

func TestDecodeBitmapSynthetic(t *testing.T) {
	// 3x3 grid, bitmap 101 010 101 (5 points present).
	var w bitWriter
	for _, v := range []uint64{1, 2, 3, 4, 5} {
		w.put(v, 4)
	}
	msg := assemble(testSec1(), testSec3(3, 3, 0x40), testSec4(), testSec5Simple(5, 0, 0, 0, 4),
		sec(6, []byte{0, 0b10101010, 0b10000000}), sec(7, w.buf))
	f, err := Decode(msg)
	if err != nil {
		t.Fatal(err)
	}
	nan := float32(math.NaN())
	want := []float32{1, nan, 2, nan, 3, nan, 4, nan, 5}
	for i := range want {
		if got := f.Values[i]; got != want[i] && !(math.IsNaN(float64(got)) && math.IsNaN(float64(want[i]))) {
			t.Errorf("value[%d] = %v, want %v", i, got, want[i])
		}
	}
	if f.DLat != 1 {
		t.Errorf("DLat %v for +j scan", f.DLat)
	}
}

func TestDecodeMissingValueManagement2(t *testing.T) {
	// nbits 4: constant groups use 15 (primary) / 14 (secondary) as missing;
	// in a 3-bit group 7 is primary and 6 secondary.
	c := complexSpec{
		ni: 4, nj: 2, nbits: 4, mvm: 2, widthBits: 2, lenBits: 3, lenIncr: 1, lastLen: 5,
		refs: []uint64{15, 14, 10}, widths: []uint64{0, 0, 3}, lens: []uint64{2, 1, 0},
		values: [][]uint64{nil, nil, {7, 6, 2, 0, 5}}, valWidths: []uint{0, 0, 3},
	}
	f, err := Decode(c.message())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if !math.IsNaN(float64(f.Values[i])) {
			t.Errorf("value[%d] = %v, want missing", i, f.Values[i])
		}
	}
	for i, want := range []float32{12, 10, 15} {
		if got := f.Values[5+i]; got != want {
			t.Errorf("value[%d] = %v, want %v", 5+i, got, want)
		}
	}

	// With mvm 1 the secondary markers are ordinary values.
	c.mvm = 1
	f, err = Decode(c.message())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Values[2]; got != 14 {
		t.Errorf("mvm 1: constant group value %v, want 14", got)
	}
	if got := f.Values[4]; got != 16 {
		t.Errorf("mvm 1: value %v, want 16", got)
	}
}

func TestDecodeSpatialDifferencingSigned(t *testing.T) {
	// Order 1, first value -5, overall minimum -3 (sign-magnitude).
	c := complexSpec{
		ni: 2, nj: 2, nbits: 1, widthBits: 2, lenBits: 1, lastLen: 4, lenIncr: 1,
		refs: []uint64{0}, widths: []uint64{2}, lens: []uint64{0},
		values: [][]uint64{{0, 1, 3, 0}}, valWidths: []uint{2},
		order: 1, octets: 2, descrip: []int64{-5, -3},
	}
	f, err := Decode(c.message())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float32{-5, -7, -7, -10} {
		if f.Values[i] != want {
			t.Errorf("order 1 value[%d] = %v, want %v", i, f.Values[i], want)
		}
	}
	// Order 2, first values 4 and -1, minimum 2: x[n] = v + 2 + 2x[n-1] - x[n-2].
	c.order, c.descrip = 2, []int64{4, -1, 2}
	f, err = Decode(c.message())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float32{4, -1, -1, 1} {
		if f.Values[i] != want {
			t.Errorf("order 2 value[%d] = %v, want %v", i, f.Values[i], want)
		}
	}
}

func goodComplex() complexSpec {
	return complexSpec{
		ni: 4, nj: 2, nbits: 4, widthBits: 2, lenBits: 3, lenIncr: 1, lastLen: 5,
		refs: []uint64{1, 2, 3}, widths: []uint64{0, 0, 3}, lens: []uint64{2, 1, 0},
		values: [][]uint64{nil, nil, {1, 2, 3, 4, 5}}, valWidths: []uint{0, 0, 3},
	}
}

func TestDecodeRejects(t *testing.T) {
	base := simpleMessage(3, 2, []uint64{0, 1, 2, 3, 4, 5}, 3)
	if _, err := Decode(base); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(goodComplex().message()); err != nil {
		t.Fatal(err)
	}
	s3 := sectionOffset(base, 3)
	s4 := sectionOffset(base, 4)
	s5 := sectionOffset(base, 5)
	s6 := sectionOffset(base, 6)

	patch := func(msg []byte, off int, b ...byte) []byte {
		m := append([]byte(nil), msg...)
		copy(m[off:], b)
		return m
	}
	complexWith := func(edit func(*complexSpec)) []byte {
		c := goodComplex()
		edit(&c)
		return c.message()
	}

	cases := []struct {
		name string
		msg  []byte
		want error
	}{
		{"empty", nil, ErrCorrupt},
		{"not GRIB", patch(base, 0, 'X'), ErrCorrupt},
		{"edition 1", patch(base, 7, 1), ErrUnsupported},
		{"total length too small", patch(base, 15, byte(len(base)-1)), ErrCorrupt},
		{"total length over limit", patch(base, 8, 0, 0, 0, 0, 0x7f), ErrTooLarge},
		{"missing 7777", patch(base, len(base)-1, '8'), ErrCorrupt},
		{"section length 4", patch(base, s3, 0, 0, 0, 4), ErrCorrupt},
		{"section length 0", patch(base, s3, 0, 0, 0, 0), ErrCorrupt},
		{"section length past end", patch(base, s3, 0, 0, 0x10, 0), ErrCorrupt},
		{"section length short of template", patch(base, s3, 0, 0, 0, 30), ErrCorrupt},
		{"sections out of order", patch(base, s3+4, 4), ErrCorrupt},
		{"unknown section", patch(base, s4+4, 9), ErrCorrupt},
		{"no section 7", assemble(testSec1(), testSec3(1, 1, 0), testSec4(), testSec5Simple(1, 0, 0, 0, 0), noBitmap()), ErrCorrupt},
		{"second field", assemble(testSec1(), testSec3(1, 1, 0), testSec4(), testSec5Simple(1, 0, 0, 0, 0), noBitmap(), sec(7),
			testSec4(), testSec5Simple(1, 0, 0, 0, 0), noBitmap(), sec(7)), ErrUnsupported},
		{"local section after the field", assemble(testSec1(), sec(2), testSec3(1, 1, 0), testSec4(), testSec5Simple(1, 0, 0, 0, 0), noBitmap(), sec(7),
			sec(2), sec(2), sec(2), sec(2)), ErrUnsupported},
		{"bad month", patch(base, 16+14, 13), ErrCorrupt},
		{"day 11 is valid", patch(base, 16+15, 11), nil},
		{"Nov 31", patch(base, 16+15, 31), ErrCorrupt},
		{"grid template 3.1", patch(base, s3+13, 1), ErrUnsupported},
		{"grid optional list", patch(base, s3+10, 4), ErrUnsupported},
		{"Ni zero", patch(base, s3+30, 0, 0, 0, 0), ErrCorrupt},
		{"Ni*Nj over limit", assemble(testSec1(), testSec3(1441, 721, 0), testSec4(), testSec5Simple(1441*721, 0, 0, 0, 0), noBitmap(), sec(7)), ErrTooLarge},
		{"Ni*Nj overflow uint32", patch(base, s3+30, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff), ErrTooLarge},
		{"grid point count mismatch", patch(base, s3+9, 7), ErrCorrupt},
		{"basic angle", patch(base, s3+41, 1), ErrUnsupported},
		{"scan -i", patch(base, s3+71, 0x80), ErrUnsupported},
		{"scan j consecutive", patch(base, s3+71, 0x20), ErrUnsupported},
		{"scan boustrophedon", patch(base, s3+71, 0x10), ErrUnsupported},
		{"product template 4.8", patch(base, s4+8, 8), ErrUnsupported},
		{"forecast unit 3h", patch(base, s4+17, 10), ErrUnsupported},
		{"forecast 90 minutes", patch(patch(base, s4+17, 0), s4+21, 90), ErrUnsupported},
		{"data template 5.40", patch(base, s5+10, 40), ErrUnsupported},
		{"data template 5.41", patch(base, s5+10, 41), ErrUnsupported},
		{"data template 5.42", patch(base, s5+10, 42), ErrUnsupported},
		{"data template 5.4", patch(base, s5+10, 4), ErrUnsupported},
		{"data point count", patch(base, s5+8, 7), ErrCorrupt},
		{"bits per value 33", patch(base, s5+19, 33), ErrUnsupported},
		{"binary scale 31", patch(base, s5+15, 0, 31), ErrUnsupported},
		{"decimal scale -31", patch(base, s5+17, 0x80, 31), ErrUnsupported},
		{"reference NaN", patch(base, s5+11, 0x7f, 0xc0, 0, 0), ErrCorrupt},
		{"section 7 too short", patch(base, s5+19, 32), ErrCorrupt},
		{"value overflows float32", assemble(testSec1(), testSec3(1, 1, 0), testSec4(), testSec5Simple(1, 3e38, 0, -1, 32), noBitmap(), sec(7, be32(math.MaxUint32))), ErrCorrupt},
		{"bitmap 254", patch(base, s6+5, 254), ErrUnsupported},
		{"bitmap 7", patch(base, s6+5, 7), ErrUnsupported},
		{"bitmap length mismatch", assemble(testSec1(), testSec3(3, 2, 0), testSec4(), testSec5Simple(1, 0, 0, 0, 0), sec(6, []byte{0, 0x80, 0}), sec(7)), ErrCorrupt},
		{"bitmap count mismatch", assemble(testSec1(), testSec3(3, 2, 0), testSec4(), testSec5Simple(2, 0, 0, 0, 0), sec(6, []byte{0, 0x80}), sec(7)), ErrCorrupt},
		{"bitmap pad bits ignored", assemble(testSec1(), testSec3(3, 2, 0), testSec4(), testSec5Simple(1, 0, 0, 0, 0), sec(6, []byte{0, 0x83}), sec(7)), nil},

		{"groups zero", complexWith(func(c *complexSpec) { c.refs, c.widths, c.lens, c.values, c.valWidths = nil, nil, nil, nil, nil }), ErrCorrupt},
		{"group lengths overflow", complexWith(func(c *complexSpec) { c.lenRef = math.MaxUint32 }), ErrCorrupt},
		{"group lengths short", complexWith(func(c *complexSpec) { c.lastLen = 4 }), ErrCorrupt},
		{"last group length huge", complexWith(func(c *complexSpec) { c.lastLen = math.MaxUint32 }), ErrCorrupt},
		{"group width over 32", complexWith(func(c *complexSpec) { c.widthRef = 31 }), ErrCorrupt},
		{"group width reference 33", complexWith(func(c *complexSpec) { c.widthRef = 33 }), ErrUnsupported},
		{"group width bits 33", complexWith(func(c *complexSpec) { c.widthBits = 33 }), ErrUnsupported},
		{"group length bits 33", complexWith(func(c *complexSpec) { c.lenBits = 33 }), ErrUnsupported},
		{"group reference bits 33", complexWith(func(c *complexSpec) { c.nbits = 33 }), ErrUnsupported},
		{"missing value management 3", complexWith(func(c *complexSpec) { c.mvm = 3 }), ErrUnsupported},
		{"group values truncated", complexWith(func(c *complexSpec) { c.values[2] = c.values[2][:2] }), ErrCorrupt},
		{"spatial order 3", complexWith(func(c *complexSpec) { c.order, c.octets, c.descrip = 3, 1, []int64{0, 0, 0, 0} }), ErrUnsupported},
		{"descriptor octets 0", complexWith(func(c *complexSpec) { c.order, c.octets = 1, 0 }), ErrUnsupported},
		{"descriptor octets 5", complexWith(func(c *complexSpec) { c.order, c.octets, c.descrip = 1, 5, []int64{0, 0} }), ErrUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Decode(tc.msg)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
			if f != nil {
				t.Fatalf("field returned with error")
			}
		})
	}
}

func TestDecodeComplexGroupsMoreThanPoints(t *testing.T) {
	c := goodComplex()
	msg := c.message()
	s5 := sectionOffset(msg, 5)
	binary.BigEndian.PutUint32(msg[s5+31:], 9) // 9 groups for 8 points
	if _, err := Decode(msg); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("error %v", err)
	}
	binary.BigEndian.PutUint32(msg[s5+31:], 8) // 8 groups: descriptors no longer fit
	if _, err := Decode(msg); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("error %v", err)
	}
}

// TestDecodeTruncated cuts real messages at many offsets, both bluntly and
// with the section 7 and total lengths patched to match so the decoder has
// to notice the missing bits itself.
func TestDecodeTruncated(t *testing.T) {
	for _, name := range []string{"tmp850_drt53.grib2", "tmp850_drt52.grib2", "tmp850_drt50.grib2", "weasd_drt53_bitmap.grib2", "tmp850_drt53_mvm.grib2"} {
		msg := readFixture(t, name)
		step := len(msg)/97 + 1
		for k := 0; k < len(msg); k += step {
			if _, err := Decode(msg[:k]); err == nil {
				t.Errorf("%s cut at %d decoded", name, k)
			}
		}
		s7 := sectionOffset(msg, 7)
		l7 := len(msg) - 4 - s7
		for cut := 1; cut < l7-5; cut += (l7-5)/61 + 1 {
			m := append([]byte(nil), msg[:len(msg)-4-cut]...)
			m = append(m, "7777"...)
			binary.BigEndian.PutUint64(m[8:], uint64(len(m)))
			binary.BigEndian.PutUint32(m[s7:], uint32(l7-cut))
			if _, err := Decode(m); !errors.Is(err, ErrCorrupt) {
				t.Errorf("%s section 7 short by %d: error %v", name, cut, err)
			}
		}
	}
}

// TestDecodeByteFlips corrupts every header byte of a real message; any
// outcome but a panic is acceptable, and a nil error must give a sane field.
func TestDecodeByteFlips(t *testing.T) {
	msg := readFixture(t, "weasd_drt53_bitmap.grib2")
	s7 := sectionOffset(msg, 7)
	for i := 0; i < s7+64; i++ {
		for _, x := range []byte{0x01, 0x80, 0xff} {
			m := append([]byte(nil), msg...)
			m[i] ^= x
			checkDecodeSane(t, m)
		}
	}
}

func checkDecodeSane(t testing.TB, msg []byte) {
	t.Helper()
	f, err := Decode(msg)
	if err != nil {
		if f != nil {
			t.Fatalf("field returned with error %v", err)
		}
		if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrTooLarge) {
			t.Fatalf("error %v wraps none of the package errors", err)
		}
		return
	}
	if f.Ni <= 0 || f.Nj <= 0 || len(f.Values) != f.Ni*f.Nj || len(f.Values) > MaxPoints {
		t.Fatalf("bad field shape %dx%d with %d values", f.Ni, f.Nj, len(f.Values))
	}
	for i, v := range f.Values {
		if math.IsInf(float64(v), 0) {
			t.Fatalf("value[%d] is infinite", i)
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	msg := readFixture(b, "tmp850_drt53.grib2")
	b.SetBytes(int64(len(msg)))
	for i := 0; i < b.N; i++ {
		if _, err := Decode(msg); err != nil {
			b.Fatal(err)
		}
	}
}
