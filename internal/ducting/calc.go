package ducting

// Tropospheric ducting physics: one vertical profile in, one Result out.
// Pure functions, no I/O. Formulas and limits are in the package doc and types.go.

import (
	"math"
	"slices"
)

// vapourPressureHPa returns the water-vapour partial pressure (hPa) for an
// air temperature tC (°C) and relative humidity rhPct (0..100).
func vapourPressureHPa(tC, rhPct float64) float64 {
	return rhPct / 100 * 6.1121 * math.Exp(17.502*tC/(240.97+tC))
}

// Refractivity returns the radio refractivity N (ITU-R P.453) for pressure
// pHPa, temperature tC (°C) and relative humidity rhPct.
func Refractivity(pHPa, tC, rhPct float64) float64 {
	t := tC + 273.15
	e := vapourPressureHPa(tC, rhPct)
	return 77.6 / t * (pHPa + 4810*e/t)
}

// ModifiedRefractivity returns M = N + 0.157*h for h metres above ground.
func ModifiedRefractivity(n, hAGL float64) float64 {
	return n + 0.157*hAGL
}

// finite reports whether v is neither NaN nor infinite.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// saneLevel reports whether p, t and rh are finite and inside the
// accepted physical ranges (RH is clamped later, so only finiteness matters).
func saneLevel(pHPa, tC, rhPct float64) bool {
	return finite(pHPa) && finite(tC) && finite(rhPct) &&
		pHPa >= 100 && pHPa <= 1100 && tC >= -90 && tC <= 60
}

// shortest returns the smallest of the given lengths. (The package's
// test files shadow the builtin min, so it is not used here.)
func shortest(lens ...int) int {
	s := lens[0]
	for _, l := range lens[1:] {
		if l < s {
			s = l
		}
	}
	return s
}

// clampRH limits a relative humidity to 0..100.
func clampRH(rh float64) float64 {
	return math.Min(100, math.Max(0, rh))
}

// Profile builds the ascending profile from a surface point (2 m values)
// and pressure-level data. Slices may differ in length (the shortest wins)
// and may hold NaN for missing values; unusable levels are skipped. Levels at
// or below ground, at or below the surface point, or above MaxHeightAGL
// are dropped. When surfPHPa is not finite the pressure-below-ground check is
// skipped and only the height check applies.
func Profile(elevation, surfPHPa, t2mC, rh2mPct float64, levelsHPa, tempsC, rhsPct, geoHeightsM []float64) []Level {
	n := shortest(len(levelsHPa), len(tempsC), len(rhsPct), len(geoHeightsM))
	return profileInto(make([]Level, 0, n+1), elevation, surfPHPa, t2mC, rh2mPct, levelsHPa, tempsC, rhsPct, geoHeightsM)
}

// profileInto is Profile appending to out (len 0, any capacity).
func profileInto(out []Level, elevation, surfPHPa, t2mC, rh2mPct float64, levelsHPa, tempsC, rhsPct, geoHeightsM []float64) []Level {
	n := shortest(len(levelsHPa), len(tempsC), len(rhsPct), len(geoHeightsM))
	if saneLevel(surfPHPa, t2mC, rh2mPct) {
		out = append(out, Level{
			HeightAGL:   SurfaceHeightM,
			PressureHPa: surfPHPa,
			TempC:       t2mC,
			RHPct:       clampRH(rh2mPct),
		})
	}
	surfOK := finite(surfPHPa)
	for i := 0; i < n; i++ {
		p, t, rh, z := levelsHPa[i], tempsC[i], rhsPct[i], geoHeightsM[i]
		if !saneLevel(p, t, rh) || !finite(z) || !finite(elevation) {
			continue
		}
		if surfOK && p >= surfPHPa {
			continue
		}
		h := z - elevation
		if h <= SurfaceHeightM || h > MaxHeightAGL {
			continue
		}
		out = append(out, Level{HeightAGL: h, PressureHPa: p, TempC: t, RHPct: clampRH(rh)})
	}
	// Stable so that, among equal heights, the surface point (appended first)
	// is the one kept by the de-duplication below.
	slices.SortStableFunc(out, func(a, b Level) int {
		switch {
		case a.HeightAGL < b.HeightAGL:
			return -1
		case a.HeightAGL > b.HeightAGL:
			return 1
		}
		return 0
	})
	dedup := out[:0]
	for i, l := range out {
		if i > 0 && l.HeightAGL == dedup[len(dedup)-1].HeightAGL {
			continue
		}
		dedup = append(dedup, l)
	}
	return dedup
}

// Analyze computes N and M for each level of an ascending profile and
// classifies it (see analyzeM).
func Analyze(profile []Level) Result {
	n := len(profile)
	return analyzeInto(profile, make([]float64, n), make([]float64, n), make([]float64, n))
}

// Scratch owns the buffers Profile and Analyze would otherwise allocate per
// call, so a loop over many points (the global grid job) allocates nothing
// once the buffers have grown. Not safe for concurrent use: one per goroutine.
// Results are identical to the package-level Profile and Analyze.
type Scratch struct {
	levels  []Level
	h, m, n []float64
}

// Profile is the package-level Profile writing into the scratch buffer. The
// returned slice is valid until the next Profile call on s.
func (s *Scratch) Profile(elevation, surfPHPa, t2mC, rh2mPct float64, levelsHPa, tempsC, rhsPct, geoHeightsM []float64) []Level {
	n := shortest(len(levelsHPa), len(tempsC), len(rhsPct), len(geoHeightsM))
	s.levels = slices.Grow(s.levels[:0], n+1)
	s.levels = profileInto(s.levels, elevation, surfPHPa, t2mC, rh2mPct, levelsHPa, tempsC, rhsPct, geoHeightsM)
	return s.levels
}

// Analyze is the package-level Analyze using the scratch buffers.
func (s *Scratch) Analyze(profile []Level) Result {
	n := len(profile)
	s.h = slices.Grow(s.h[:0], n)[:n]
	s.m = slices.Grow(s.m[:0], n)[:n]
	s.n = slices.Grow(s.n[:0], n)[:n]
	return analyzeInto(profile, s.h, s.m, s.n)
}

// analyzeInto fills h, m, n (each len(profile)) and classifies the profile.
func analyzeInto(profile []Level, h, m, n []float64) Result {
	for i, l := range profile {
		h[i] = l.HeightAGL
		n[i] = Refractivity(l.PressureHPa, l.TempC, l.RHPct)
		m[i] = ModifiedRefractivity(n[i], l.HeightAGL)
	}
	return analyzeM(h, m, n)
}

// meanMinLayerM is the shallowest layer meanGradient still averages
// over when the profile stops below MeanLayerM (high terrain: the models'
// top level, 700 hPa, is only ~1.4 km above Denver).
const meanMinLayerM = 1000.0

// meanGradient returns (N at MeanLayerM - N at the first point) /
// MeanLayerM in N/km, with N at MeanLayerM linearly interpolated
// between the two points that bracket it. When none do but the highest point
// is at least meanMinLayerM above ground, it returns the mean over that
// shallower depth instead: (N at the top - N at the first point) / its
// height. 0 when the profile is shallower still. h ascends; h[0] is the 2 m
// surface point.
func meanGradient(h, n []float64) float64 {
	top := -1
	for i := 0; i+1 < len(h); i++ {
		if top < 0 || h[i+1] > h[top] {
			top = i + 1
		}
		if h[i] > MeanLayerM || h[i+1] < MeanLayerM || h[i+1] <= h[i] {
			continue
		}
		n1500 := n[i] + (n[i+1]-n[i])*(MeanLayerM-h[i])/(h[i+1]-h[i])
		return (n1500 - n[0]) / MeanLayerM * 1000
	}
	if top < 0 || h[top] < meanMinLayerM {
		return 0
	}
	return (n[top] - n[0]) / h[top] * 1000
}

// analyzeM finds trapping layers in an M profile (heights ascending) and
// returns the strongest duct (lowest fmin) plus the most negative dN/dh.
func analyzeM(h, m, n []float64) Result {
	cnt := shortest(len(h), len(m), len(n))
	if cnt < 2 {
		return Result{Class: ClassNoData}
	}

	var res Result
	haveGrad := false
	for i := 0; i+1 < cnt; i++ {
		dh := h[i+1] - h[i]
		if dh <= 0 {
			continue
		}
		g := (n[i+1] - n[i]) / (dh / 1000)
		if !haveGrad || g < res.MinGradNkm {
			res.MinGradNkm = g
			haveGrad = true
		}
	}

	res.MeanGradNkm = meanGradient(h[:cnt], n[:cnt])

	found, surface := false, false
	var bestF, bestBase, bestTop, bestDM float64
	for a := 0; a+1 < cnt; {
		if !(m[a+1] < m[a]) {
			a++
			continue
		}
		b := a + 1
		for b+1 < cnt && m[b+1] < m[b] {
			b++
		}
		mTop, top := m[b], h[b]

		j := -1
		for k := a - 1; k >= 0; k-- {
			if m[k] <= mTop {
				j = k
				break
			}
		}
		base, from := 0.0, 0
		if j >= 0 {
			// m[j] <= mTop < m[j+1] since j is the highest such index.
			base = h[j] + (h[j+1]-h[j])*(mTop-m[j])/(m[j+1]-m[j])
			from = j
		}
		mMax := m[from]
		for k := from + 1; k <= b; k++ {
			mMax = math.Max(mMax, m[k])
		}
		d, dm := top-base, mMax-mTop
		if d > 0 && dm > 0 {
			// One constant for surface and elevated ducts: see FminConstMHz.
			f := FminConstMHz / (d * math.Sqrt(dm))
			if !found || f < bestF || (f == bestF && base < bestBase) {
				found, surface = true, j < 0
				bestF, bestBase, bestTop, bestDM = f, base, top, dm
			}
		}
		a = b
	}

	switch {
	case found && surface:
		res.Class = ClassSurface
	case found:
		res.Class = ClassElev
	case haveGrad && res.MinGradNkm <= SuperGradientNk:
		res.Class = ClassSuper
	default:
		res.Class = ClassNormal
	}
	if found {
		res.FminMHz, res.BaseM, res.TopM, res.DeltaM = bestF, bestBase, bestTop, bestDM
	}
	return res
}
