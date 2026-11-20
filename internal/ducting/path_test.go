package ducting

import (
	"math"
	"testing"
)

// bandGrid is GFS 0.5°, all ClassNormal, with a surface duct (fmin 400 MHz)
// between 10 and 20 N and a super-refractive strip from 179 E to 179 W.
func bandGrid(t testing.TB) *Grid {
	t.Helper()
	g := gfsGrid(t)
	for idx := range g.C {
		lat, lon := g.LatLon(idx)
		switch {
		case lat >= 10 && lat <= 20:
			g.Set(idx, Result{Class: ClassSurface, FminMHz: 400})
		case math.Abs(lon) >= 179 || lon == -180:
			g.Set(idx, Result{Class: ClassSuper})
		case lat < -80:
			g.Set(idx, Result{})
		default:
			g.Set(idx, Result{Class: ClassNormal})
		}
	}
	return g
}

func snapHalf(v float64) float64 { return math.Round(v*2) / 2 }

func TestPathStatsDuctBand(t *testing.T) {
	g := bandGrid(t)
	// Along the prime meridian, 0 -> 30 N: 3336 km at 50 km wants 68
	// samples, capped at 64 at lat 30*s/63.
	st := g.PathStats(0, 0, 30, 0, 869.5, 50)
	want := 0
	for s := 0; s < PathMaxSamples; s++ {
		if lat := snapHalf(30 * float64(s) / 63); lat >= 10 && lat <= 20 {
			want++
		}
	}
	if st.Samples != PathMaxSamples || st.Duct != want || st.NoData != 0 || st.Super != 0 {
		t.Fatalf("got %+v, want %d samples with %d duct", st, PathMaxSamples, want)
	}
	// 400 MHz duct does not trap 300 MHz.
	if st := g.PathStats(0, 0, 30, 0, 300, 50); st.Duct != 0 {
		t.Errorf("300 MHz: %+v", st)
	}
	// Reversed direction counts the same.
	if st2 := g.PathStats(30, 0, 0, 0, 869.5, 50); st2 != st {
		t.Errorf("reverse %+v != %+v", st2, st)
	}
}

func TestPathStatsAntimeridian(t *testing.T) {
	g := bandGrid(t)
	// Equator, 175 E -> 175 W the short way: 1112 km at 100 km = 13 samples
	// at lon 175 + 10*s/12; the strip is |lon| >= 179.
	st := g.PathStats(0, 175, 0, -175, 869.5, 100)
	want := 0
	for s := 0; s < 13; s++ {
		lon := snapHalf(175 + 10*float64(s)/12)
		if lon >= 179 && lon <= 181 {
			want++
		}
	}
	if st.Samples != 13 || st.Super != want || st.Duct != 0 || st.NoData != 0 {
		t.Fatalf("got %+v, want 13 samples with %d super", st, want)
	}
}

func TestPathStatsSampleCount(t *testing.T) {
	g := bandGrid(t)
	for _, tc := range []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		spacing                float64
		samples, noData        int
	}{
		{"short", 0, 0, 0, 1, 50, 4, 0},                    // 111 km -> ceil(2.2)+1
		{"one step", 0, 0, 0, 1, 200, 2, 0},                // never fewer than 2
		{"identical", 40, -100, 40, -100, 50, 2, 0},        // degenerate
		{"zero spacing", 0, 0, 0, 1, 0, PathMaxSamples, 0}, // invalid spacing: cap
		{"nan spacing", 0, 0, 0, 1, math.NaN(), PathMaxSamples, 0},
		{"nan end", math.NaN(), 0, 0, 1, 50, 2, 2},   // outside the lattice
		{"antipodal", 0, 0, 0, 180, 1000, 22, 0},     // 20015 km
		{"no data", -85, 0, -85, 1, 50, 2, 2},        // 10 km near the pole
		{"pole to pole", 90, 0, -90, 0, 10000, 4, 1}, // ends at -90: no data
	} {
		st := g.PathStats(tc.lat1, tc.lon1, tc.lat2, tc.lon2, 869.5, tc.spacing)
		if st.Samples != tc.samples || st.NoData != tc.noData {
			t.Errorf("%s: %+v, want %d samples %d no-data", tc.name, st, tc.samples, tc.noData)
		}
	}
}

func TestPathStatsOffLattice(t *testing.T) {
	g, err := NewGrid("reg", 0, 0, 0, 230, 1, 1, 11, 11)
	if err != nil {
		t.Fatal(err)
	}
	fillClass(g, ClassSurface)
	for i := range g.F {
		g.F[i] = 100
	}
	// Starts inside (lon -125), ends far east, off the lattice.
	st := g.PathStats(5, -125, 5, -100, 869.5, 2000)
	if st.Samples != 3 || st.Duct != 1 || st.NoData != 2 {
		t.Fatalf("%+v", st)
	}
}

func BenchmarkPathStats500km(b *testing.B) {
	g := smoothGrid(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.PathStats(45, -122, 41, -118, 915, 10) // ~550 km, 56 samples
	}
}
