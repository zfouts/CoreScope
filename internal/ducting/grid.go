package ducting

import "math"

// Set stores r at point idx, rounded and clamped to the stored widths.
// F is stored only for a duct (Class >= ClassElev). NaN stores 0.
func (g *Grid) Set(idx int, r Result) {
	g.C[idx] = r.Class
	if r.Class >= ClassElev {
		g.F[idx] = roundU16(r.FminMHz)
	} else {
		g.F[idx] = 0
	}
	g.B[idx] = roundU16(r.BaseM)
	g.T[idx] = roundU16(r.TopM)
	g.DM[idx] = roundU16(r.DeltaM * 10)
	g.G[idx] = roundI16(r.MinGradNkm)
	g.N[idx] = roundI16(r.MeanGradNkm)
}

func roundU16(v float64) uint16 {
	v = math.Round(v)
	switch {
	case !(v > 0): // also NaN
		return 0
	case v >= math.MaxUint16:
		return math.MaxUint16
	}
	return uint16(v)
}

func roundI16(v float64) int16 {
	v = math.Round(v)
	switch {
	case math.IsNaN(v):
		return 0
	case v <= math.MinInt16:
		return math.MinInt16
	case v >= math.MaxInt16:
		return math.MaxInt16
	}
	return int16(v)
}

// normLon maps a longitude into [-180, 180).
func normLon(lon float64) float64 {
	v := math.Mod(lon+180, 360)
	if v < 0 {
		v += 360
	}
	if v >= 360 {
		v -= 360
	}
	return v - 180
}

// LatLon is the position of point idx, longitude normalised to [-180, 180).
func (g *Grid) LatLon(idx int) (lat, lon float64) {
	r, c := idx/g.Ni, idx%g.Ni
	return g.Lat0 + float64(r)*g.DLat, normLon(g.Lon0 + float64(c)*g.DLon)
}

// wraps reports whether the columns cover the whole circle, so the column
// after the last is the first again.
func (g *Grid) wraps() bool {
	return math.Abs(float64(g.Ni)*g.DLon-360) <= lonSpanTol
}

// Nearest returns the index of the lattice point nearest to (lat, lon).
// A point belongs to the lattice when it is within half a step of its rows
// (and, for a lattice that does not wrap, of its columns); otherwise, or for
// a non-finite input, it reports false. Longitude is taken modulo 360, so
// any convention (0..360 or -180..180) matches the lattice's.
func (g *Grid) Nearest(lat, lon float64) (int, bool) {
	if !isFinite(lat) || !isFinite(lon) {
		return 0, false
	}
	fr := (lat - g.Lat0) / g.DLat
	if !(fr >= -0.5 && fr <= float64(g.Nj)-0.5) {
		return 0, false
	}
	r := int(math.Round(fr))
	r = max(0, min(r, g.Nj-1))

	// Offset east of the first column, in [-DLon/2, 360-DLon/2).
	half := g.DLon / 2
	d := math.Mod(lon-g.Lon0+half, 360)
	if d < 0 {
		d += 360
	}
	fc := (d - half) / g.DLon
	c := int(math.Round(fc))
	if g.wraps() {
		c = ((c % g.Ni) + g.Ni) % g.Ni
	} else {
		if !(fc <= float64(g.Ni)-0.5) {
			return 0, false
		}
		c = max(0, min(c, g.Ni-1))
	}
	return r*g.Ni + c, true
}

// CarriesLoRa reports whether a point with this class and fmin traps a
// signal at freqMHz: a duct whose lowest trapped frequency is at or below it.
func CarriesLoRa(class uint8, fminMHz uint16, freqMHz float64) bool {
	return class >= ClassElev && fminMHz > 0 && float64(fminMHz) <= freqMHz
}
