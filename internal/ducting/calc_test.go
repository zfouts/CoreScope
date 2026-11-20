package ducting

import (
	"math"
	"testing"
)

// Expected values below were computed independently (python, same formulas
// as types.go and calc.go), not by calling the code under test.

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f ±%g", name, got, want, tol)
	}
}

func TestRefractivityKnownAnswers(t *testing.T) {
	approx(t, "Refractivity(1013.25,15,50)", Refractivity(1013.25, 15, 50), 311.19, 0.05)
	approx(t, "vapourPressureHPa(15,100)", vapourPressureHPa(15, 100), 17.046, 0.005)
	approx(t, "vapourPressureHPa(15,0)", vapourPressureHPa(15, 0), 0, 1e-12)
	approx(t, "ModifiedRefractivity(300,1000)", ModifiedRefractivity(300, 1000), 457, 1e-9)
}

func TestAnalyzeMKnownAnswers(t *testing.T) {
	cases := []struct {
		name  string
		h, m  []float64
		class uint8
		base  float64
		top   float64
		dm    float64
		fmin  float64
		ftol  float64
	}{
		{
			name: "surface-based",
			h:    []float64{0, 100, 200, 300}, m: []float64{350, 340, 345, 380},
			class: ClassSurface, base: 0, top: 100, dm: 10, fmin: 379.47, ftol: 0.1,
		},
		{
			name: "elevated",
			h:    []float64{0, 500, 700, 900, 1200}, m: []float64{330, 400, 380, 395, 440},
			class: ClassElev, base: 357.14, top: 700, dm: 20, fmin: 78.26, ftol: 0.05,
		},
		{
			// Run 1 surface-based (fmin 536.66), run 2 elevated (fmin 58.42): run 2 wins.
			name: "two runs, later stronger",
			h:    []float64{0, 100, 200, 300, 600, 700}, m: []float64{350, 345, 360, 400, 370, 420},
			class: ClassElev, base: 225, top: 600, dm: 30, fmin: 58.42, ftol: 0.05,
		},
		{
			// Run 1 surface-based (fmin 219.09), run 2 elevated (fmin 357.77): run 1 wins.
			name: "two runs, earlier stronger",
			h:    []float64{0, 100, 200, 300, 400}, m: []float64{400, 370, 380, 390, 385},
			class: ClassSurface, base: 0, top: 100, dm: 30, fmin: 219.09, ftol: 0.05,
		},
		{
			// Both runs d=100, ΔM=10 (fmin 379.47): tie goes to the lower bottom.
			name: "tie -> lower bottom",
			h:    []float64{0, 100, 200, 290}, m: []float64{350, 340, 440, 430},
			class: ClassSurface, base: 0, top: 100, dm: 10, fmin: 379.47, ftol: 0.1,
		},
		{
			// Elevated layer whose M crosses Mtop exactly at a level.
			name: "elevated, crossing at a level",
			h:    []float64{0, 200, 400, 600}, m: []float64{300, 320, 340, 320},
			class: ClassElev, base: 200, top: 600, dm: 20, fmin: 67.08, ftol: 0.05,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := make([]float64, len(c.m))
			for i := range c.m {
				n[i] = c.m[i] - 0.157*c.h[i]
			}
			r := analyzeM(c.h, c.m, n)
			if r.Class != c.class {
				t.Fatalf("Class = %d, want %d (%+v)", r.Class, c.class, r)
			}
			approx(t, "BaseM", r.BaseM, c.base, 0.05)
			approx(t, "TopM", r.TopM, c.top, 1e-9)
			approx(t, "DeltaM", r.DeltaM, c.dm, 1e-9)
			approx(t, "FminMHz", r.FminMHz, c.fmin, c.ftol)
		})
	}
}

func TestAnalyzeMNoDuct(t *testing.T) {
	t.Run("super-refractive", func(t *testing.T) {
		// dN/dh = -100 N/km everywhere, dM/dh = +57 M/km: bends but does not trap.
		h := []float64{0, 200, 400, 600}
		n := []float64{350, 330, 310, 290}
		m := []float64{350, 361.4, 372.8, 384.2}
		r := analyzeM(h, m, n)
		if r.Class != ClassSuper {
			t.Fatalf("Class = %d, want %d", r.Class, ClassSuper)
		}
		approx(t, "MinGradNkm", r.MinGradNkm, -100, 1e-9)
		if r.FminMHz != 0 || r.BaseM != 0 || r.TopM != 0 || r.DeltaM != 0 {
			t.Errorf("duct fields not zero: %+v", r)
		}
	})
	t.Run("dM/dh exactly zero is not a duct", func(t *testing.T) {
		// dN/dh = -157 N/km over the lowest layer: M constant there.
		h := []float64{0, 1000, 2000}
		n := []float64{350, 193, 160}
		m := []float64{350, 350, 474}
		r := analyzeM(h, m, n)
		if r.Class != ClassSuper {
			t.Fatalf("Class = %d, want %d (%+v)", r.Class, ClassSuper, r)
		}
		if r.FminMHz != 0 {
			t.Errorf("FminMHz = %v, want 0", r.FminMHz)
		}
		approx(t, "MinGradNkm", r.MinGradNkm, -157, 1e-9)
	})
	t.Run("fewer than 2 levels", func(t *testing.T) {
		for _, h := range [][]float64{nil, {0}} {
			r := analyzeM(h, h, h)
			if r != (Result{Class: ClassNoData}) {
				t.Errorf("len %d: got %+v, want NoData zero result", len(h), r)
			}
		}
		if r := Analyze([]Level{{HeightAGL: 2, PressureHPa: 1000, TempC: 10, RHPct: 50}}); r.Class != ClassNoData {
			t.Errorf("Analyze 1 level: Class = %d, want 0", r.Class)
		}
	})
}

// ISA standard atmosphere at 100 m elevation, RH 40 %. Python gives
// min dN/dh = -45.398 N/km (between the 2 m point and the 1000 hPa level)
// and every other layer between -26.6 and -36.5 N/km.
func TestStandardAtmosphereIsNormal(t *testing.T) {
	isaT := func(z float64) float64 { return 15 - 6.5*z/1000 }
	isaZ := func(p float64) float64 { return (1 - math.Pow(p/1013.25, 1/5.25588)) / 2.25577e-5 }
	const elev = 100.0
	ps := 1013.25 * math.Pow(1-2.25577e-5*elev, 5.25588)
	levels := []float64{1000, 975, 950, 925, 900, 850, 800, 700}
	var temps, rhs, zs []float64
	for _, p := range levels {
		z := isaZ(p)
		zs = append(zs, z)
		temps = append(temps, isaT(z))
		rhs = append(rhs, 40)
	}
	prof := Profile(elev, ps, isaT(elev), 40, levels, temps, rhs, zs)
	if len(prof) != 9 {
		t.Fatalf("profile has %d levels, want 9", len(prof))
	}
	r := Analyze(prof)
	if r.Class != ClassNormal {
		t.Fatalf("Class = %d, want %d (%+v)", r.Class, ClassNormal, r)
	}
	approx(t, "MinGradNkm (≈-40)", r.MinGradNkm, -40, 10)
	approx(t, "MinGradNkm (python)", r.MinGradNkm, -45.398, 0.01)
	if r.FminMHz != 0 {
		t.Errorf("FminMHz = %v, want 0", r.FminMHz)
	}
}

// A strong humidity lapse at the surface traps through Analyze end to end.
func TestAnalyzeDuctingSurfaceDuct(t *testing.T) {
	prof := []Level{
		{HeightAGL: 2, PressureHPa: 1013, TempC: 25, RHPct: 90},
		{HeightAGL: 100, PressureHPa: 1001.5, TempC: 27, RHPct: 20},
		{HeightAGL: 500, PressureHPa: 956.5, TempC: 24, RHPct: 20},
	}
	r := Analyze(prof)
	if r.Class != ClassSurface {
		t.Fatalf("Class = %d, want %d (%+v)", r.Class, ClassSurface, r)
	}
	// Python: M = 383.652 (2 m), 304.165 (100 m), 353.508 (500 m).
	approx(t, "BaseM", r.BaseM, 0, 1e-9)
	approx(t, "TopM", r.TopM, 100, 1e-9)
	approx(t, "DeltaM", r.DeltaM, 79.487, 0.005)
	approx(t, "FminMHz", r.FminMHz, 134.60, 0.05)
}

func TestProfile(t *testing.T) {
	nan := math.NaN()
	t.Run("filters and sorts", func(t *testing.T) {
		// elevation 200 m, surface 990 hPa.
		levels := []float64{850, 1000, 925, 950, 700, 900, 875}
		temps := []float64{8, 20, 12, nan, -5, 70, 9}
		rhs := []float64{60, 50, 120, 50, 40, 50, -5}
		zs := []float64{1450, 110, 760, 540, 3300, 990, 1220}
		p := Profile(200, 990, 18, 55, levels, temps, rhs, zs)
		// 1000 hPa: below ground, dropped. 950: NaN T, skipped. 700: 3100 m AGL,
		// dropped. 900: T 70 °C, rejected. Remaining: surface, 925, 875, 850.
		wantH := []float64{2, 560, 1020, 1250}
		wantP := []float64{990, 925, 875, 850}
		wantRH := []float64{55, 100, 0, 60}
		if len(p) != len(wantH) {
			t.Fatalf("got %d levels %+v, want %d", len(p), p, len(wantH))
		}
		for i := range wantH {
			if p[i].HeightAGL != wantH[i] || p[i].PressureHPa != wantP[i] || p[i].RHPct != wantRH[i] {
				t.Errorf("level %d = %+v, want h=%v p=%v rh=%v", i, p[i], wantH[i], wantP[i], wantRH[i])
			}
		}
		if p[0].TempC != 18 {
			t.Errorf("surface TempC = %v, want 18", p[0].TempC)
		}
	})
	t.Run("mismatched lengths use the shortest", func(t *testing.T) {
		p := Profile(0, 1013, 15, 50,
			[]float64{1000, 950, 900},
			[]float64{14, 11},
			[]float64{50, 50, 50, 50},
			[]float64{110, 540, 990})
		if len(p) != 3 || p[2].PressureHPa != 950 {
			t.Fatalf("got %+v, want surface + 1000 + 950", p)
		}
		if got := Profile(0, 1013, 15, 50, nil, nil, nil, nil); len(got) != 1 {
			t.Fatalf("nil slices: got %+v, want surface only", got)
		}
	})
	t.Run("duplicate heights de-duplicated", func(t *testing.T) {
		p := Profile(0, 1013, 15, 50,
			[]float64{1000, 999, 950},
			[]float64{14, 13, 11},
			[]float64{50, 50, 50},
			[]float64{110, 110, 540})
		if len(p) != 3 || p[1].PressureHPa != 1000 || p[2].HeightAGL != 540 {
			t.Fatalf("got %+v, want surface, 1000@110, 950@540", p)
		}
	})
	t.Run("bad surface point omitted", func(t *testing.T) {
		levels := []float64{1000, 950}
		temps := []float64{14, 11}
		rhs := []float64{50, 50}
		zs := []float64{110, 540}
		for _, s := range []struct{ p, t, rh float64 }{{1013, nan, 50}, {1013, 15, nan}, {1013, 95, 50}, {1200, 15, 50}} {
			p := Profile(0, s.p, s.t, s.rh, levels, temps, rhs, zs)
			if len(p) == 0 || p[0].HeightAGL == SurfaceHeightM {
				t.Errorf("surface %+v: got %+v, want no surface point", s, p)
			}
		}
		// Surface pressure missing: levels still kept on the height check.
		if p := Profile(0, nan, 15, 50, levels, temps, rhs, zs); len(p) != 2 {
			t.Errorf("NaN surface pressure: got %+v, want 2 levels", p)
		}
	})
	t.Run("level at the surface point height dropped", func(t *testing.T) {
		p := Profile(100, 1013, 15, 50, []float64{1000}, []float64{14}, []float64{50}, []float64{102})
		if len(p) != 1 {
			t.Fatalf("got %+v, want surface only", p)
		}
	})
}

// Mean gradient over the lowest 1.5 km. Python, ITU-R P.453 N:
//
//	N(2 m,    1000 hPa, 15 C, 50 %) = 307.6179
//	N(500 m,   950 hPa, 12 C, 55 %) = 293.9286  (not a bracketing level)
//	N(1200 m,  870 hPa,  6 C, 60 %) = 268.7227
//	N(1800 m,  820 hPa,  3 C, 70 %) = 256.3955
//
// 1500 m lies halfway between 1200 and 1800 m:
// N(1500) = 268.7227 + (256.3955-268.7227)*300/600 = 262.5591
// MeanGradNkm = (262.5591 - 307.6179) / 1500 * 1000 = -30.04 N/km.
func TestMeanGradientKnownAnswer(t *testing.T) {
	prof := []Level{
		{HeightAGL: 2, PressureHPa: 1000, TempC: 15, RHPct: 50},
		{HeightAGL: 500, PressureHPa: 950, TempC: 12, RHPct: 55},
		{HeightAGL: 1200, PressureHPa: 870, TempC: 6, RHPct: 60},
		{HeightAGL: 1800, PressureHPa: 820, TempC: 3, RHPct: 70},
	}
	r := Analyze(prof)
	approx(t, "MeanGradNkm", r.MeanGradNkm, -30.04, 0.01)
	// MinGradNkm is the steepest consecutive pair, unchanged:
	// (268.7227-293.9286)/0.7 = -36.01, (256.3955-268.7227)/0.6 = -20.55,
	// (293.9286-307.6179)/0.498 = -27.49.
	approx(t, "MinGradNkm", r.MinGradNkm, -36.01, 0.01)
}

func TestMeanGradientExactLevel(t *testing.T) {
	// A level exactly at 1500 m is used as is (no interpolation needed).
	prof := []Level{
		{HeightAGL: 2, PressureHPa: 1000, TempC: 15, RHPct: 50},
		{HeightAGL: 1500, PressureHPa: 850, TempC: 5, RHPct: 60},
	}
	want := (Refractivity(850, 5, 60) - Refractivity(1000, 15, 50)) / 1.5
	approx(t, "MeanGradNkm", Analyze(prof).MeanGradNkm, want, 1e-9)
}

func TestMeanGradientTopBelowLayer(t *testing.T) {
	prof := []Level{
		{HeightAGL: 2, PressureHPa: 1000, TempC: 15, RHPct: 50},
		{HeightAGL: 500, PressureHPa: 950, TempC: 12, RHPct: 55},
		{HeightAGL: 900, PressureHPa: 905, TempC: 9, RHPct: 60},
	}
	r := Analyze(prof)
	if r.MeanGradNkm != 0 {
		t.Errorf("MeanGradNkm = %v, want 0 (top below 1000 m)", r.MeanGradNkm)
	}
	if r.MinGradNkm == 0 {
		t.Error("MinGradNkm should still be computed")
	}
	if g := Analyze(nil).MeanGradNkm; g != 0 {
		t.Errorf("no data: MeanGradNkm = %v, want 0", g)
	}
}

func TestMeanGradientStandardAtmosphere(t *testing.T) {
	isaT := func(z float64) float64 { return 15 - 6.5*z/1000 }
	isaZ := func(p float64) float64 { return (1 - math.Pow(p/1013.25, 1/5.25588)) / 2.25577e-5 }
	const elev = 100.0
	ps := 1013.25 * math.Pow(1-2.25577e-5*elev, 5.25588)
	levels := []float64{1000, 925, 850, 800, 700}
	var temps, rhs, zs []float64
	for _, p := range levels {
		z := isaZ(p)
		zs = append(zs, z)
		temps = append(temps, isaT(z))
		rhs = append(rhs, 40)
	}
	r := Analyze(Profile(elev, ps, isaT(elev), 40, levels, temps, rhs, zs))
	if r.MeanGradNkm < -50 || r.MeanGradNkm > -30 {
		t.Errorf("MeanGradNkm = %.2f, want about -40 (-50..-30)", r.MeanGradNkm)
	}
}

// High terrain: the models stop at 700 hPa, ~1,400 m above a Denver-like
// surface, so no two points bracket 1500 m. The mean is taken over the
// depth there is: (N(top) - N(2 m)) / h_top.
func TestMeanGradientHighTerrain(t *testing.T) {
	prof := []Level{
		{HeightAGL: 2, PressureHPa: 835, TempC: 20, RHPct: 30},
		{HeightAGL: 400, PressureHPa: 800, TempC: 17, RHPct: 30},
		{HeightAGL: 1400, PressureHPa: 700, TempC: 8, RHPct: 35},
	}
	want := (Refractivity(700, 8, 35) - Refractivity(835, 20, 30)) / 1400 * 1000
	r := Analyze(prof)
	if want >= 0 || want < -100 {
		t.Fatalf("test profile gives an implausible %v N/km", want)
	}
	approx(t, "MeanGradNkm", r.MeanGradNkm, want, 1e-9)

	// Just below 1500 m: still the available depth, not 0.
	prof[2].HeightAGL = 1499
	want = (Refractivity(700, 8, 35) - Refractivity(835, 20, 30)) / 1499 * 1000
	approx(t, "MeanGradNkm 1499 m", Analyze(prof).MeanGradNkm, want, 1e-9)

	// Exactly 1000 m is enough; just under is not.
	prof[2].HeightAGL = 1000
	want = (Refractivity(700, 8, 35) - Refractivity(835, 20, 30)) / 1000 * 1000
	approx(t, "MeanGradNkm 1000 m", Analyze(prof).MeanGradNkm, want, 1e-9)
	prof[2].HeightAGL = 999
	if g := Analyze(prof).MeanGradNkm; g != 0 {
		t.Errorf("top 999 m: MeanGradNkm = %v, want 0", g)
	}
}

// Mexico City: ground ~2,240 m, surface pressure ~775 hPa, and 700 hPa (the
// models' top level) only ~860 m above it. Through Profile with the GFS
// levels, everything at or below 775 hPa is dropped, the profile stops under
// the 1 km floor, and the cell has no field value (n = 0); the duct analysis
// still runs on the 860 m there are. A Denver-like site (ground ~1,600 m,
// 700 hPa ~1,450 m up) gets a value.
func TestMeanGradientMexicoCityHasNoFieldValue(t *testing.T) {
	// The server's GFS level list (ductingLevels(700) in cmd/server): 1000..700 hPa every 25.
	levelsHPa := []float64{1000, 975, 950, 925, 900, 875, 850, 825, 800, 775, 750, 725, 700}
	// A standard-ish column: geopotential height of each level (m above sea
	// level), temperature and humidity; levels under ground are present, as
	// the API returns them, and must be ignored.
	geo := map[float64]float64{1000: 110, 975: 330, 950: 550, 925: 770, 900: 1000, 875: 1230, 850: 1460,
		825: 1700, 800: 1950, 775: 2210, 750: 2480, 725: 2790, 700: 3100}
	col := func(elev, surfP float64) Result {
		temps := make([]float64, len(levelsHPa))
		rhs := make([]float64, len(levelsHPa))
		geos := make([]float64, len(levelsHPa))
		for i, p := range levelsHPa {
			geos[i] = geo[p]
			temps[i] = 24 - 6.5*geo[p]/1000
			rhs[i] = 45
		}
		prof := Profile(elev, surfP, 24-6.5*elev/1000, 45, levelsHPa, temps, rhs, geos)
		return Analyze(prof)
	}
	mx := col(2240, 775)
	if mx.Class == ClassNoData {
		t.Fatalf("Mexico City: no data at all; want a profile to analyse")
	}
	if mx.MeanGradNkm != 0 {
		t.Errorf("Mexico City: MeanGradNkm %v, want 0 (700 hPa is only 860 m up, under the 1 km floor)", mx.MeanGradNkm)
	}
	den := col(1600, 840)
	if den.MeanGradNkm >= 0 || den.MeanGradNkm < -100 {
		t.Errorf("Denver: MeanGradNkm %v, want a plausible negative value", den.MeanGradNkm)
	}
}
