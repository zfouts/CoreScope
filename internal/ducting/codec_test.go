package ducting

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// randomGrid fills every field with random values, classes included.
func randomGrid(t testing.TB, ni, nj int, seed int64) *Grid {
	t.Helper()
	g, err := NewGrid("gfs-025", 1700000000, 1700010800, 90, 0, -180/float64(nj-1), 360/float64(ni), ni, nj)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	for i := range g.C {
		g.C[i] = uint8(rng.Intn(int(ClassSurface) + 1))
		g.F[i] = uint16(rng.Intn(65536))
		g.B[i] = uint16(rng.Intn(65536))
		g.T[i] = uint16(rng.Intn(65536))
		g.DM[i] = uint16(rng.Intn(65536))
		g.G[i] = int16(rng.Intn(65536) - 32768)
		g.N[i] = int16(rng.Intn(65536) - 32768)
	}
	return g
}

// smoothGrid is a GFS 0.5° sized grid with spatially smooth fields, so its
// compression ratio resembles real output.
func smoothGrid(t testing.TB) *Grid {
	t.Helper()
	g, err := NewGrid("gfs", 1700000000, 1700010800, 90, 0, -0.5, 0.5, 720, 361)
	if err != nil {
		t.Fatal(err)
	}
	for idx := range g.C {
		lat, lon := g.LatLon(idx)
		phi, lam := lat*math.Pi/180, lon*math.Pi/180
		w := math.Sin(3*lam)*math.Cos(2*phi) + 0.5*math.Sin(5*phi+lam)
		r := Result{Class: ClassNormal, MinGradNkm: -40 + 60*w, MeanGradNkm: -40 + 15*w}
		switch {
		case math.Abs(lat) > 85:
			r = Result{}
		case w > 0.9:
			r.Class, r.FminMHz, r.BaseM, r.TopM, r.DeltaM = ClassSurface, 300+400*(1.4-w), 0, 200+100*w, 10*w
		case w > 0.7:
			r.Class, r.FminMHz, r.BaseM, r.TopM, r.DeltaM = ClassElev, 600+800*(1-w), 400, 700, 5*w
		case w > 0.4:
			r.Class = ClassSuper
		}
		g.Set(idx, r)
	}
	return g
}

func TestCodecRoundTrip(t *testing.T) {
	for _, tc := range []struct{ ni, nj int }{{1, 1}, {7, 3}, {720, 361}} {
		g := randomGrid(t, tc.ni, max(tc.nj, 2), int64(tc.ni))
		if tc.ni == 1 {
			g, _ = NewGrid("a", -5, 7, 10, -20, 1, 2, 1, 1)
			g.C[0], g.F[0], g.B[0], g.T[0], g.DM[0], g.G[0], g.N[0] = 4, 65535, 1, 2, 3, -32768, 32767
		}
		b, err := Encode(g)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatalf("%dx%d: %v", tc.ni, tc.nj, err)
		}
		if !reflect.DeepEqual(got, g) {
			t.Fatalf("%dx%d: round trip differs", tc.ni, tc.nj)
		}
	}
	g := smoothGrid(t)
	b, err := Encode(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil || !reflect.DeepEqual(got, g) {
		t.Fatalf("smooth grid round trip: %v", err)
	}
}

func TestEncodeHeaderLayout(t *testing.T) {
	g, _ := NewGrid("gfs", 11, 22, 90, 0, -0.5, 0.5, 4, 2)
	b, err := Encode(g)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:4]) != "CSDG" || b[4] != 1 || b[5] != 3 || string(b[6:9]) != "gfs" {
		t.Fatalf("header prefix %q", b[:9])
	}
	h := b[9:]
	le := binary.LittleEndian
	if le.Uint32(h) != 4 || le.Uint32(h[4:]) != 2 ||
		math.Float64frombits(le.Uint64(h[8:])) != 90 || math.Float64frombits(le.Uint64(h[16:])) != 0 ||
		math.Float64frombits(le.Uint64(h[24:])) != -0.5 || math.Float64frombits(le.Uint64(h[32:])) != 0.5 ||
		le.Uint64(h[40:]) != 11 || le.Uint64(h[48:]) != 22 {
		t.Fatalf("header fields wrong")
	}
	raw, err := readAllFlate(h[56:])
	if err != nil || len(raw) != 4*2*13 {
		t.Fatalf("payload %d bytes, %v", len(raw), err)
	}
}

func readAllFlate(b []byte) ([]byte, error) {
	var out bytes.Buffer
	_, err := out.ReadFrom(flate.NewReader(bytes.NewReader(b)))
	return out.Bytes(), err
}

func TestNewGridValidation(t *testing.T) {
	ok := func(model string, lat0, lon0, dLat, dLon float64, ni, nj int) error {
		_, err := NewGrid(model, 0, 0, lat0, lon0, dLat, dLon, ni, nj)
		return err
	}
	if err := ok("gfs", 90, 0, -0.5, 0.5, 720, 361); err != nil {
		t.Fatalf("GFS 0.5: %v", err)
	}
	if err := ok("gfs-025", 90, 0, -0.25, 0.25, 1440, 721); err != nil {
		t.Fatalf("GFS 0.25: %v", err)
	}
	nan, inf := math.NaN(), math.Inf(1)
	for name, err := range map[string]error{
		"empty model":   ok("", 90, 0, -0.5, 0.5, 2, 2),
		"long model":    ok(strings.Repeat("a", 33), 90, 0, -0.5, 0.5, 2, 2),
		"upper model":   ok("GFS", 90, 0, -0.5, 0.5, 2, 2),
		"model space":   ok("g fs", 90, 0, -0.5, 0.5, 2, 2),
		"ni 0":          ok("gfs", 90, 0, -0.5, 0.5, 0, 2),
		"nj -1":         ok("gfs", 90, 0, -0.5, 0.5, 2, -1),
		"too many":      ok("gfs", 90, 0, -0.1, 0.1, 1441, 721),
		"nan lat0":      ok("gfs", nan, 0, -0.5, 0.5, 2, 2),
		"inf lon0":      ok("gfs", 90, inf, -0.5, 0.5, 2, 2),
		"nan dlat":      ok("gfs", 90, 0, nan, 0.5, 2, 2),
		"inf dlon":      ok("gfs", 90, 0, -0.5, inf, 2, 2),
		"dlon 0":        ok("gfs", 90, 0, -0.5, 0, 2, 2),
		"dlon negative": ok("gfs", 90, 0, -0.5, -0.5, 2, 2),
		"dlat 0":        ok("gfs", 90, 0, 0, 0.5, 2, 2),
		"rows past -90": ok("gfs", 90, 0, -0.5, 0.5, 2, 362),
		"lat0 past 90":  ok("gfs", 91, 0, -0.5, 0.5, 2, 2),
		"lon span":      ok("gfs", 90, 0, -0.5, 0.5, 721, 2),
		"lon0 range":    ok("gfs", 90, 361, -0.5, 0.5, 2, 2),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// ni*nj computed without overflow
	if _, err := NewGrid("gfs", 0, 0, 0, 0, 1, 1, math.MaxInt32, math.MaxInt32); err == nil {
		t.Error("huge dims accepted")
	}
}

func TestEncodeRejectsBadGrid(t *testing.T) {
	if _, err := Encode(nil); err == nil {
		t.Error("nil accepted")
	}
	g, _ := NewGrid("gfs", 0, 0, 90, 0, -0.5, 0.5, 3, 3)
	g.N = g.N[:8]
	if _, err := Encode(g); err == nil {
		t.Error("short slice accepted")
	}
	g, _ = NewGrid("gfs", 0, 0, 90, 0, -0.5, 0.5, 3, 3)
	g.C[4] = 5
	if _, err := Encode(g); err == nil {
		t.Error("class 5 accepted")
	}
}

// encodeRaw builds an encoded grid from parts, so Decode can be fed
// headers and payloads Encode would never write.
func encodeRaw(model string, ni, nj uint32, lat0, lon0, dLat, dLon float64, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString(GridMagic)
	b.WriteByte(GridVersion)
	b.WriteByte(byte(len(model)))
	b.WriteString(model)
	var h [56]byte
	le := binary.LittleEndian
	le.PutUint32(h[0:], ni)
	le.PutUint32(h[4:], nj)
	le.PutUint64(h[8:], math.Float64bits(lat0))
	le.PutUint64(h[16:], math.Float64bits(lon0))
	le.PutUint64(h[24:], math.Float64bits(dLat))
	le.PutUint64(h[32:], math.Float64bits(dLon))
	b.Write(h[:])
	b.Write(payload)
	return b.Bytes()
}

func deflate(raw []byte) []byte {
	var b bytes.Buffer
	w, _ := flate.NewWriter(&b, flate.BestSpeed)
	w.Write(raw)
	w.Close()
	return b.Bytes()
}

func TestDecodeFailures(t *testing.T) {
	good, err := Encode(randomGrid(t, 6, 4, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(good); err != nil {
		t.Fatalf("good input: %v", err)
	}
	mut := func(f func(b []byte) []byte) []byte {
		return f(append([]byte(nil), good...))
	}
	okPayload := deflate(make([]byte, 6*4*13))
	nan := math.NaN()
	cases := map[string][]byte{
		"empty":            nil,
		"short":            good[:5],
		"bad magic":        mut(func(b []byte) []byte { b[0] = 'X'; return b }),
		"bad version":      mut(func(b []byte) []byte { b[4] = 2; return b }),
		"model len 0":      encodeRaw("", 6, 4, 90, 0, -0.5, 0.5, okPayload),
		"model chars":      encodeRaw("GFS!", 6, 4, 90, 0, -0.5, 0.5, okPayload),
		"header truncated": good[:20],
		"model len past":   mut(func(b []byte) []byte { b[5] = 255; return b[:100] }),
		"ni 0":             encodeRaw("gfs", 0, 4, 90, 0, -0.5, 0.5, okPayload),
		"dims over limit":  encodeRaw("gfs", 1441, 722, 90, 0, -0.1, 0.1, okPayload),
		"dims overflow":    encodeRaw("gfs", math.MaxUint32, math.MaxUint32, 90, 0, -0.5, 0.5, okPayload),
		"nan lat0":         encodeRaw("gfs", 6, 4, nan, 0, -0.5, 0.5, okPayload),
		"inf dlon":         encodeRaw("gfs", 6, 4, 90, 0, -0.5, math.Inf(1), okPayload),
		"nan dlat":         encodeRaw("gfs", 6, 4, 90, 0, nan, 0.5, okPayload),
		"dlat 0":           encodeRaw("gfs", 6, 4, 90, 0, 0, 0.5, okPayload),
		"dlon negative":    encodeRaw("gfs", 6, 4, 90, 0, -0.5, -0.5, okPayload),
		"no payload":       encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, nil),
		"truncated stream": good[:len(good)-5],
		"short inflate":    encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(make([]byte, 6*4*13-1))),
		"long inflate":     encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(make([]byte, 6*4*13+1))),
		"bomb":             encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(make([]byte, 64<<20))),
		"trailing garbage": append(append([]byte(nil), good...), 0, 1, 2),
		"not deflate":      encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, bytes.Repeat([]byte{0xff}, 40)),
		"class 7":          encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(append([]byte{7}, make([]byte, 6*4*13-1)...))),
		"over 8 MiB":       append(append([]byte(nil), good...), make([]byte, 8<<20)...),
	}
	for name, b := range cases {
		g, err := Decode(b)
		if err == nil || g != nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
}

func FuzzDecode(f *testing.F) {
	small, err := Encode(randomGrid(f, 6, 4, 2))
	if err != nil {
		f.Fatal(err)
	}
	g, _ := NewGrid("gfs", 1, 2, 90, 0, -0.5, 0.5, 720, 361)
	for i := range g.C {
		g.C[i] = uint8(i/7) % 5
		g.N[i] = int16(i / 1000)
	}
	big, err := Encode(g)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(small)
	f.Add(big)
	f.Add(encodeRaw("gfs", 6, 4, 90, 0, -0.5, 0.5, deflate(make([]byte, 6*4*13))))
	f.Fuzz(func(t *testing.T, b []byte) {
		g, err := Decode(b)
		if err != nil {
			return
		}
		// A grid that decodes is fully usable.
		_ = g.Window(-90, -180, 90, 180, 500)
		_ = g.PathStats(10, 170, -10, -170, 869.5, 50)
		re, err := Encode(g)
		if err != nil {
			t.Fatalf("decoded grid does not re-encode: %v", err)
		}
		g2, err := Decode(re)
		if err != nil || !reflect.DeepEqual(g, g2) {
			t.Fatalf("re-encoded grid differs: %v", err)
		}
	})
}

func BenchmarkDecode(b *testing.B) {
	g := smoothGrid(b)
	enc, err := Encode(g)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(enc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Decode(enc); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(enc)), "enc-bytes")
}

func BenchmarkEncode(b *testing.B) {
	g := smoothGrid(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Encode(g); err != nil {
			b.Fatal(err)
		}
	}
}
