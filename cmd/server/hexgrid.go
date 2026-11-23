package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Pure-Go hexagonal binning for RX coverage display. We deliberately avoid the
// CGO-based uber/h3-go. (That predates the SQLite driver move to cgo; this
// stays pure Go because it needs no C library, not because cgo is unavailable.)
// Points are
// projected to Web Mercator and snapped to a pointy-top hex grid whose size
// depends on the display resolution. Cell ids are "res:q:r" (axial coords).
// At city/region scale this looks like H3/mapme.sh coverage without any deps.

const hexEarthRadius = 6378137.0 // Web Mercator sphere radius (m)

// hexTargetPx is the desired on-screen hex size (point-to-point height) in CSS
// pixels. mercUPPZ0 is Web Mercator units per pixel at zoom 0 (world span / 256);
// Leaflet halves it each zoom level, independent of latitude. Sizing the hex in
// these units therefore renders it at a constant ~hexTargetPx at every zoom — the
// old fixed-meter buckets looked like specks when zoomed out (issue: hexes too small).
const hexTargetPx = 28.0
const mercUPPZ0 = 156543.03392

func hexMercator(lat, lon float64) (float64, float64) {
	x := hexEarthRadius * lon * math.Pi / 180
	y := hexEarthRadius * math.Log(math.Tan(math.Pi/4+lat*math.Pi/360))
	return x, y
}

func hexInvMercator(x, y float64) (lat, lon float64) {
	lon = x / hexEarthRadius * 180 / math.Pi
	lat = (2*math.Atan(math.Exp(y/hexEarthRadius)) - math.Pi/2) * 180 / math.Pi
	return lat, lon
}

// hexSizeForRes is the hex circumradius (center→corner) in Web Mercator units for a
// display resolution. Resolution equals the Leaflet zoom level (see zoomToHexRes), so
// the size scales as 2^-zoom and the hex keeps a constant ~hexTargetPx on-screen size
// regardless of zoom. hexCellAt (binning) and hexBoundary (drawing) both read this, so
// they stay consistent for a given cell id.
func hexSizeForRes(res int) float64 {
	return (hexTargetPx / 2) * mercUPPZ0 / math.Pow(2, float64(res))
}

// hexMaxLat is the Web Mercator latitude limit. The projection (hexMercator)
// diverges toward ±90° — tan(π/4 + lat·π/360) → ∞ — so points beyond this would
// produce NaN cell rings via hexInvMercator. Coverage is therefore only defined
// within ±hexMaxLat; polar submissions are clamped to the edge (#17).
const hexMaxLat = 85.05112878

// hexCellAt returns a stable cell id ("res:q:r") for the lat/lon at res. Latitude
// is clamped to ±hexMaxLat so near-polar points bin to the edge instead of
// producing NaN geometry.
func hexCellAt(lat, lon float64, res int) string {
	if lat > hexMaxLat {
		lat = hexMaxLat
	} else if lat < -hexMaxLat {
		lat = -hexMaxLat
	}
	size := hexSizeForRes(res)
	x, y := hexMercator(lat, lon)
	q := (math.Sqrt(3)/3*x - 1.0/3*y) / size
	r := (2.0 / 3 * y) / size
	qi, ri := hexRound(q, r)
	return fmt.Sprintf("%d:%d:%d", res, qi, ri)
}

// hexRound rounds fractional axial coords to the nearest hex via cube rounding.
func hexRound(q, r float64) (int, int) {
	x, z := q, r
	y := -x - z
	rx, ry, rz := math.Round(x), math.Round(y), math.Round(z)
	dx, dy, dz := math.Abs(rx-x), math.Abs(ry-y), math.Abs(rz-z)
	switch {
	case dx > dy && dx > dz:
		rx = -ry - rz
	case dy > dz:
		ry = -rx - rz
	default:
		rz = -rx - ry
	}
	return int(rx), int(rz)
}

// hexBoundary returns the cell's 6 corners as a closed [lon,lat] ring (GeoJSON
// order), or nil if the cell id is malformed.
func hexBoundary(cellID string) [][2]float64 {
	res, q, r, ok := parseHexCell(cellID)
	if !ok {
		return nil
	}
	size := hexSizeForRes(res)
	cx := size * (math.Sqrt(3)*float64(q) + math.Sqrt(3)/2*float64(r))
	cy := size * (1.5 * float64(r))
	ring := make([][2]float64, 0, 7)
	for i := 0; i < 6; i++ {
		ang := math.Pi / 180 * float64(60*i-30)
		lat, lon := hexInvMercator(cx+size*math.Cos(ang), cy+size*math.Sin(ang))
		ring = append(ring, [2]float64{lon, lat})
	}
	ring = append(ring, ring[0]) // close the ring
	return ring
}

func parseHexCell(id string) (res, q, r int, ok bool) {
	p := strings.Split(id, ":")
	if len(p) != 3 {
		return 0, 0, 0, false
	}
	a, e1 := strconv.Atoi(p[0])
	b, e2 := strconv.Atoi(p[1])
	c, e3 := strconv.Atoi(p[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	return a, b, c, true
}

// ─── Location privacy: companion GPS fixes as cells ─────────────────────────
//
// With location privacy on (location_privacy.go), a companion GPS fix
// (client_receptions, client_rf_samples) is never used as it is: every route
// built on those tables first replaces each fix by the centre of its privacy
// cell (fixCellCentre) and only then filters, bins and measures. Everything
// such a route serves is therefore a function of cell centres, whatever the
// request (bbox, zoom), so no combination of requests places a fix better
// than its cell. The display hexes are binned from those centres and are
// themselves no smaller than the radius (maxHexResForRadius).
//
// The privacy grid is public and deterministic: rows fixCellFrac·R high,
// and per row a whole number of columns at least fixCellFrac·R wide at the
// row's poleward edge (so the antimeridian is never inside a cell). A cell is
// thus at least 2R × 2R on the ground.

// fixCellFrac is the privacy cell side as a multiple of the radius.
const fixCellFrac = 2.0

// fixCellMaxLat keeps the column width finite next to the poles.
const fixCellMaxLat = 89.9

// fixCellCols is the number of privacy cell columns of a row whose poleward
// edge is at latitude edge, for cells of side sideM metres.
func fixCellCols(edge, sideM float64) float64 {
	edge = math.Min(math.Abs(edge), fixCellMaxLat)
	return math.Max(1, math.Floor(360*math.Cos(edge*math.Pi/180)*locMetresPerDeg/sideM))
}

// fixCellCentre returns the centre of the privacy cell that holds the fix
// (lat, lon) for the radius radiusM; the fix itself when radiusM <= 0 (privacy
// off). Pure maths, no allocation: it runs once per row of a scan.
func fixCellCentre(radiusM, lat, lon float64) (float64, float64) {
	if radiusM <= 0 || math.IsNaN(lat) || math.IsNaN(lon) || math.IsInf(lat, 0) || math.IsInf(lon, 0) {
		return lat, lon
	}
	side := fixCellFrac * radiusM
	h := side / locMetresPerDeg
	lat = math.Max(-90, math.Min(90, lat))
	lon = wrapLon(lon)
	row := math.Floor(lat / h)
	latC := math.Max(-90, math.Min(90, (row+0.5)*h))
	ncol := fixCellCols(math.Max(math.Abs(row*h), math.Abs((row+1)*h)), side)
	dLon := 360 / ncol
	col := math.Min(math.Floor((lon+180)/dLon), ncol-1)
	return latC, -180 + (col+0.5)*dLon
}

// widenForFixCells grows b so it holds every fix whose privacy cell centre
// lies in b: the SQL prefilter still uses the (lat, lon) index, and
// binFixesInBBox then keeps exactly the fixes whose centre is inside b.
func (b bbox) widenForFixCells(radiusM float64) bbox {
	if radiusM <= 0 {
		return b
	}
	side := fixCellFrac * radiusM
	h := side / locMetresPerDeg
	// Columns only get wider poleward: the width at two rows beyond the
	// bbox bounds every cell whose centre is inside it.
	edge := math.Max(math.Abs(b.MinLat), math.Abs(b.MaxLat)) + 2*h
	dLon := 360 / fixCellCols(edge, side)
	return bbox{MinLat: b.MinLat - h, MaxLat: b.MaxLat + h, MinLon: b.MinLon - dLon, MaxLon: b.MaxLon + dLon}
}

// contains reports whether (lat, lon) lies in b, bounds included (the SQL
// BETWEEN semantics).
func (b bbox) contains(lat, lon float64) bool {
	return lat >= b.MinLat && lat <= b.MaxLat && lon >= b.MinLon && lon <= b.MaxLon
}

// binFixesInBBox replaces, in place, every fix of rows by its privacy cell
// centre (pos returns pointers to a row's lat and lon) and keeps the rows
// whose centre lies in b. O(n), no allocation: the result reuses rows.
func binFixesInBBox[T any](rows []T, radiusM float64, b bbox, pos func(*T) (lat, lon *float64)) []T {
	out := rows[:0]
	for i := range rows {
		la, lo := pos(&rows[i])
		*la, *lo = fixCellCentre(radiusM, *la, *lo)
		if b.contains(*la, *lo) {
			out = append(out, rows[i])
		}
	}
	return out
}

// maxHexResForRadius is the finest display resolution whose hexes, anywhere
// within one cell of latitude lat (poleward), have a ground circumradius of at
// least radiusM. Web Mercator shrinks a hex on the ground by cos(latitude),
// so the bound is taken at the poleward edge, and ground metres are those of
// the earthRadiusKm sphere every CoreScope distance uses. 0 when even that is
// too fine.
func maxHexResForRadius(radiusM, lat float64) int {
	a := math.Min(math.Abs(lat), hexMaxLat)
	const ground = earthRadiusKm * 1000 / hexEarthRadius // metres per Mercator unit at the equator
	for res := 18; res > 0; res-- {
		size := hexSizeForRes(res)
		// A Mercator unit spans at most 1/hexEarthRadius rad of latitude.
		edge := math.Min(hexMaxLat, a+size/hexEarthRadius*180/math.Pi)
		if size*ground*math.Cos(edge*math.Pi/180) >= radiusM {
			return res
		}
	}
	return 0
}
