package ducting

import (
	"math/rand"
	"reflect"
	"runtime"
	"testing"
)

// decodePlanesAgrees checks that DecodePlanes accepts exactly what Decode
// accepts and, when it does, yields the same header, C and F planes.
func decodePlanesAgrees(t *testing.T, d *Decoder, name string, b []byte) bool {
	t.Helper()
	want, werr := Decode(b)
	got, gerr := d.DecodePlanes(b)
	if (werr == nil) != (gerr == nil) {
		t.Fatalf("%s: Decode err %v, DecodePlanes err %v", name, werr, gerr)
	}
	if werr != nil {
		return false
	}
	if got.Model != want.Model || got.RunTime != want.RunTime || got.ValidTime != want.ValidTime ||
		got.Lat0 != want.Lat0 || got.Lon0 != want.Lon0 || got.DLat != want.DLat || got.DLon != want.DLon ||
		got.Ni != want.Ni || got.Nj != want.Nj {
		t.Fatalf("%s: header differs: %+v vs %+v", name, got, want)
	}
	if !reflect.DeepEqual(got.C, want.C) || !reflect.DeepEqual(got.F, want.F) {
		t.Fatalf("%s: C or F plane differs", name)
	}
	if a, b := got.PathStats(10, 170, -10, -170, 869.5, 50), want.PathStats(10, 170, -10, -170, 869.5, 50); a != b {
		t.Fatalf("%s: PathStats %+v vs %+v", name, a, b)
	}
	return true
}

func TestDecodePlanesMatchesDecode(t *testing.T) {
	var d Decoder // one decoder for every case: failures must not poison it
	for _, tc := range []struct{ ni, nj int }{{6, 4}, {720, 361}, {7, 3}, {6, 4}} {
		b, err := Encode(randomGrid(t, tc.ni, tc.nj, int64(tc.ni)))
		if err != nil {
			t.Fatal(err)
		}
		if !decodePlanesAgrees(t, &d, "valid", b) {
			t.Fatal("valid grid refused")
		}
	}

	good, _ := Encode(randomGrid(t, 6, 4, 1))
	okRaw := make([]byte, 6*4*13)
	cases := map[string][]byte{
		"empty":            nil,
		"short":            good[:5],
		"header truncated": good[:20],
		"truncated stream": good[:len(good)-5],
		"no payload":       encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, nil),
		"short inflate":    encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(okRaw[:len(okRaw)-1])),
		"short in C/F":     encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(okRaw[:2*6*4])),
		"long inflate":     encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(append(okRaw[:len(okRaw):len(okRaw)], 0))),
		"bomb":             encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(make([]byte, 64<<20))),
		"trailing garbage": append(append([]byte(nil), good...), 0, 1, 2),
		"class 7":          encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(append([]byte{7}, okRaw[1:]...))),
		"bad magic":        append([]byte("X"), good[1:]...),
	}
	for name, b := range cases {
		if decodePlanesAgrees(t, &d, name, b) {
			t.Errorf("%s: decoded without error", name)
		}
	}

	// Random byte flips: the two decoders must always agree.
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		b := append([]byte(nil), good...)
		for k := 0; k < 1+rng.Intn(3); k++ {
			b[rng.Intn(len(b))] ^= byte(1 + rng.Intn(255))
		}
		decodePlanesAgrees(t, &d, "mutated", b)
	}
	if !decodePlanesAgrees(t, &d, "valid after failures", good) {
		t.Fatal("decoder poisoned by earlier failures")
	}
}

func TestDecodePlanesReusesBuffers(t *testing.T) {
	b, err := Encode(smoothGrid(t))
	if err != nil {
		t.Fatal(err)
	}
	var d Decoder
	if _, err := d.DecodePlanes(b); err != nil {
		t.Fatal(err)
	}
	// Warm, only flate's small per-block tables remain (~60 KB), not the
	// 3.4 MB inflate buffer and plane slices Decode allocates.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const runs = 5
	for i := 0; i < runs; i++ {
		if _, err := d.DecodePlanes(b); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 256<<10 {
		t.Fatalf("%d bytes allocated per warm DecodePlanes", per)
	}
}

func BenchmarkDecodePlanes(b *testing.B) {
	enc, err := Encode(smoothGrid(b))
	if err != nil {
		b.Fatal(err)
	}
	var d Decoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.DecodePlanes(enc); err != nil {
			b.Fatal(err)
		}
	}
}
