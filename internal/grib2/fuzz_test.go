package grib2

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzDecode feeds arbitrary bytes to Decode. It must never panic, and a
// successful decode must return a well-formed field. The fixtures are large
// seeds, so run it with -fuzzminimizetime=0 (or a small count): minimizing a
// 100 KB input otherwise stalls the run.
func FuzzDecode(f *testing.F) {
	names, err := filepath.Glob(filepath.Join("testdata", "*.grib2"))
	if err != nil || len(names) == 0 {
		f.Fatalf("no fixtures: %v", err)
	}
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add(simpleMessage(3, 2, []uint64{0, 1, 2, 3, 4, 5}, 3))
	f.Add(goodComplex().message())
	c := goodComplex()
	c.order, c.octets, c.descrip = 2, 2, []int64{1, -2, 3}
	f.Add(c.message())
	f.Fuzz(func(t *testing.T, msg []byte) {
		checkDecodeSane(t, msg)
		// Most mutations break the declared length or the end marker and
		// stop at section 0; repair both so the section walk and unpacking
		// see the mutated bytes too.
		if len(msg) >= 20 {
			m := append([]byte(nil), msg...)
			binary.BigEndian.PutUint64(m[8:], uint64(len(m)))
			copy(m[len(m)-4:], "7777")
			checkDecodeSane(t, m)
		}
	})
}

// FuzzParseIndex checks ParseIndex never panics and that what it accepts
// satisfies its invariants.
func FuzzParseIndex(f *testing.F) {
	f.Add([]byte(sampleIdx))
	f.Add([]byte("1:0:d=2026111918:TMP:850 mb:6 hour fcst:"))
	f.Fuzz(func(t *testing.T, idx []byte) {
		es, err := ParseIndex(idx)
		if err != nil {
			return
		}
		for i, e := range es {
			if e.Offset < 0 || (i > 0 && e.Offset <= es[i-1].Offset) || len(e.RefDate) != 10 ||
				e.Variable == "" || strings.Contains(e.Variable, ":") {
				t.Fatalf("bad entry %d: %+v", i, e)
			}
		}
	})
}
