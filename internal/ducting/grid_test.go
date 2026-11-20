package ducting

import (
	"math"
	"testing"
)

func gfsGrid(t testing.TB) *Grid {
	t.Helper()
	g, err := NewGrid("gfs", 0, 0, 90, 0, -0.5, 0.5, 720, 361)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestSetRoundsAndClamps(t *testing.T) {
	g, _ := NewGrid("gfs", 0, 0, 0, 0, 1, 1, 1, 1)
	get := func() [7]int {
		return [7]int{int(g.C[0]), int(g.F[0]), int(g.B[0]), int(g.T[0]), int(g.DM[0]), int(g.G[0]), int(g.N[0])}
	}
	nan := math.NaN()
	for _, tc := range []struct {
		name string
		r    Result
		want [7]int
	}{
		{"round half away", Result{Class: ClassSurface, FminMHz: 433.5, BaseM: 0.4, TopM: 120.5, DeltaM: 1.25, MinGradNkm: -157.5, MeanGradNkm: -40.4},
			[7]int{4, 434, 0, 121, 13, -158, -40}},
		{"F dropped below elev", Result{Class: ClassSuper, FminMHz: 433, MinGradNkm: -90},
			[7]int{2, 0, 0, 0, 0, -90, 0}},
		{"F kept for elev", Result{Class: ClassElev, FminMHz: 900.49, BaseM: 300, TopM: 500},
			[7]int{3, 900, 300, 500, 0, 0, 0}},
		{"upper clamps", Result{Class: ClassElev, FminMHz: 1e9, BaseM: 70000, TopM: 65535.4, DeltaM: 6553.6, MinGradNkm: 40000, MeanGradNkm: 32767.6},
			[7]int{3, 65535, 65535, 65535, 65535, 32767, 32767}},
		{"lower clamps", Result{Class: ClassSurface, FminMHz: -5, BaseM: -1, TopM: -0.6, DeltaM: -3, MinGradNkm: -1e6, MeanGradNkm: -32768.6},
			[7]int{4, 0, 0, 0, 0, -32768, -32768}},
		{"NaN stores 0", Result{Class: ClassSurface, FminMHz: nan, BaseM: nan, TopM: nan, DeltaM: nan, MinGradNkm: nan, MeanGradNkm: nan},
			[7]int{4, 0, 0, 0, 0, 0, 0}},
		{"inf clamps", Result{Class: ClassSurface, FminMHz: math.Inf(1), MinGradNkm: math.Inf(-1), MeanGradNkm: math.Inf(1)},
			[7]int{4, 65535, 0, 0, 0, -32768, 32767}},
		{"DM x10 rounding", Result{Class: ClassSurface, DeltaM: 0.04}, [7]int{4, 0, 0, 0, 0, 0, 0}},
		{"DM x10 rounding up", Result{Class: ClassSurface, DeltaM: 0.05}, [7]int{4, 0, 0, 0, 1, 0, 0}},
	} {
		g.Set(0, Result{Class: ClassSurface, FminMHz: 1, BaseM: 1, TopM: 1, DeltaM: 1, MinGradNkm: 1, MeanGradNkm: 1})
		g.Set(0, tc.r)
		if got := get(); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestLatLon(t *testing.T) {
	g := gfsGrid(t)
	for _, tc := range []struct {
		idx      int
		lat, lon float64
	}{
		{0, 90, 0},
		{359, 90, 179.5},
		{360, 90, -180},
		{719, 90, -0.5},
		{720 + 1, 89.5, 0.5},
		{360*720 + 2, -90, 1},
	} {
		lat, lon := g.LatLon(tc.idx)
		if lat != tc.lat || lon != tc.lon {
			t.Errorf("LatLon(%d) = %v,%v want %v,%v", tc.idx, lat, lon, tc.lat, tc.lon)
		}
	}
	for _, v := range []float64{-540, -180, 180, 359.999, 360, 540, 1e-17, -1e-17} {
		if n := normLon(v); n < -180 || n >= 180 {
			t.Errorf("normLon(%v) = %v", v, n)
		}
	}
}

func TestNearestWrapping(t *testing.T) {
	g := gfsGrid(t)
	rc := func(lat, lon float64) (int, int, bool) {
		idx, ok := g.Nearest(lat, lon)
		return idx / g.Ni, idx % g.Ni, ok
	}
	for _, tc := range []struct {
		lat, lon float64
		r, c     int
	}{
		{90, 0, 0, 0},
		{-90, 0, 360, 0},
		{89.8, 0, 0, 0}, // rounds onto the pole row
		{-89.8, 0, 360, 0},
		{0, 360, 180, 0},
		{0, 359.9, 180, 0}, // nearer column 0 than 719
		{0, 359.6, 180, 719},
		{0, -0.1, 180, 0},
		{0, -0.4, 180, 719},
		{0, 180, 180, 360},
		{0, -180, 180, 360},
		{0, 179.6, 180, 359},
		{0, 179.9, 180, 360},
		{0, -179.9, 180, 360},
		{0, -179.6, 180, 361},
		{0, 720.5, 180, 1},
		{0, -720.5, 180, 719},
		{45.2, -122.6, 90, 720 - 245},
	} {
		r, c, ok := rc(tc.lat, tc.lon)
		if !ok || r != tc.r || c != tc.c {
			t.Errorf("Nearest(%v,%v) = (%d,%d,%v) want (%d,%d)", tc.lat, tc.lon, r, c, ok, tc.r, tc.c)
		}
	}
	for _, p := range [][2]float64{{90.3, 0}, {-90.3, 0}, {math.NaN(), 0}, {0, math.NaN()}, {0, math.Inf(1)}} {
		if _, ok := g.Nearest(p[0], p[1]); ok {
			t.Errorf("Nearest(%v,%v) inside", p[0], p[1])
		}
	}
}

func TestNearestRegional(t *testing.T) {
	// 0..10 N, 230..240 E (i.e. -130..-120), rows south to north.
	g, err := NewGrid("reg", 0, 0, 0, 230, 1, 1, 11, 11)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		lat, lon float64
		r, c     int
		ok       bool
	}{
		{0, -130, 0, 0, true},
		{10, -120, 10, 10, true},
		{5.4, 235.4, 5, 5, true},
		{5, -125, 5, 5, true},
		{5, -130.4, 5, 0, true},
		{5, -119.6, 5, 10, true},
		{5, -130.6, 0, 0, false},
		{5, -119.4, 0, 0, false},
		{-0.6, -125, 0, 0, false},
		{10.6, -125, 0, 0, false},
		{5, 50, 0, 0, false},
	} {
		idx, ok := g.Nearest(tc.lat, tc.lon)
		if ok != tc.ok || ok && (idx/g.Ni != tc.r || idx%g.Ni != tc.c) {
			t.Errorf("Nearest(%v,%v) = (%d,%d,%v) want (%d,%d,%v)", tc.lat, tc.lon, idx/g.Ni, idx%g.Ni, ok, tc.r, tc.c, tc.ok)
		}
	}
}

func TestCarriesLoRa(t *testing.T) {
	for _, tc := range []struct {
		c    uint8
		f    uint16
		freq float64
		want bool
	}{
		{ClassSurface, 800, 869.5, true},
		{ClassElev, 869, 869.5, true},
		{ClassElev, 870, 869.5, false},
		{ClassSurface, 869, 869, true},
		{ClassSurface, 0, 869.5, false},
		{ClassSuper, 100, 869.5, false},
		{ClassNormal, 100, 869.5, false},
		{ClassNoData, 100, 869.5, false},
	} {
		if got := CarriesLoRa(tc.c, tc.f, tc.freq); got != tc.want {
			t.Errorf("CarriesLoRa(%d,%d,%v) = %v", tc.c, tc.f, tc.freq, got)
		}
	}
}
