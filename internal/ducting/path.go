package ducting

import "math"

// PathMaxSamples caps the samples PathStats takes along one path.
const PathMaxSamples = 64

const earthRadiusKm = 6371.0

// PathStats samples the great circle from (lat1, lon1) to (lat2, lon2) at
// n = max(2, ceil(dist/spacingKm)+1) evenly spaced points, both ends
// included, capped at PathMaxSamples, and counts them per PathStats. A
// non-positive or non-finite spacing takes PathMaxSamples samples; a
// non-finite end point gives samples outside the lattice (NoData).
func (g *Grid) PathStats(lat1, lon1, lat2, lon2, freqMHz, spacingKm float64) PathStats {
	p1 := unitVec(lat1, lon1)
	p2 := unitVec(lat2, lon2)
	cosD := math.Max(-1, math.Min(1, dot(p1, p2)))
	delta := math.Acos(cosD) // central angle, radians

	n := 2
	if nf := math.Ceil(delta*earthRadiusKm/spacingKm) + 1; nf > PathMaxSamples || !(spacingKm > 0) || math.IsInf(spacingKm, 0) {
		n = PathMaxSamples
	} else if nf > 2 {
		n = int(nf)
	}

	// p(t) = cos(t*delta) p1 + sin(t*delta) u, with u the unit vector
	// orthogonal to p1 in the plane of p1 and p2. Stable for short paths;
	// for (near) antipodal ends any great circle through both will do.
	u := sub(p2, scale(p1, cosD))
	if l := norm(u); l > 1e-12 {
		u = scale(u, 1/l)
	} else if delta > 1 {
		u = orthogonal(p1)
	}

	var st PathStats
	var sumN float64
	for s := 0; s < n; s++ {
		a := delta * float64(s) / float64(n-1)
		p := add(scale(p1, math.Cos(a)), scale(u, math.Sin(a)))
		lat := math.Asin(math.Max(-1, math.Min(1, p[2]))) * 180 / math.Pi
		lon := math.Atan2(p[1], p[0]) * 180 / math.Pi
		st.Samples++
		idx, ok := g.Nearest(lat, lon)
		if !ok || g.C[idx] == ClassNoData {
			st.NoData++
			continue
		}
		if CarriesLoRa(g.C[idx], g.F[idx], freqMHz) {
			st.Duct++
		}
		if g.C[idx] == ClassSuper {
			st.Super++
		}
		if g.C[idx] >= ClassSuper {
			st.SuperOrDuct++
		}
		if idx < len(g.N) && g.N[idx] != 0 {
			sumN += float64(g.N[idx])
			st.NCount++
			if float64(g.N[idx]) <= HighGradientNk {
				st.Enhanced++
			}
		}
	}
	if st.NCount > 0 {
		st.MeanN = sumN / float64(st.NCount)
	}
	return st
}

type vec3 [3]float64

func unitVec(lat, lon float64) vec3 {
	phi, lam := lat*math.Pi/180, lon*math.Pi/180
	return vec3{math.Cos(phi) * math.Cos(lam), math.Cos(phi) * math.Sin(lam), math.Sin(phi)}
}

func dot(a, b vec3) float64        { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func add(a, b vec3) vec3           { return vec3{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func sub(a, b vec3) vec3           { return vec3{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func scale(a vec3, k float64) vec3 { return vec3{a[0] * k, a[1] * k, a[2] * k} }
func norm(a vec3) float64          { return math.Sqrt(dot(a, a)) }

// orthogonal returns a unit vector orthogonal to the unit vector p.
func orthogonal(p vec3) vec3 {
	axis := vec3{0, 0, 1}
	if math.Abs(p[2]) > 0.9 {
		axis = vec3{1, 0, 0}
	}
	o := sub(axis, scale(p, dot(axis, p)))
	return scale(o, 1/norm(o))
}
