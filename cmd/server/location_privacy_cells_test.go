package main

// Location privacy for companion GPS fixes (hexgrid.go: fixCellCentre,
// maxHexResForRadius): every route built on client_receptions /
// client_rf_samples serves cells no smaller than the radius, filters on cell
// centres and measures from them.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

const cellTestRadiusM = 1000.0

func cellTestFuzzer(t *testing.T, radiusM float64) *locationFuzzer {
	t.Helper()
	f, err := newLocationFuzzer(&LocationPrivacyConfig{Enabled: true, RadiusM: radiusM, Secret: strings.Repeat("cells-test-secret-", 3)})
	if err != nil || f == nil {
		t.Fatalf("newLocationFuzzer: %v", err)
	}
	return f
}

// cellTestServer is a coverage server (RX coverage and RF samples on) over
// the gaps fixture schema, with location privacy at radiusM (0 = off).
func cellTestServer(t *testing.T, radiusM float64) *Server {
	t.Helper()
	cfg := &Config{
		ClientRxCoverage: &ClientRxCoverageConfig{Enabled: true},
		ClientRfSamples:  &ClientRfSamplesConfig{Enabled: true},
	}
	if radiusM > 0 {
		cfg.setLocationFuzzer(cellTestFuzzer(t, radiusM))
	}
	return &Server{db: seedGapsDB(t), cfg: cfg}
}

func farthestVertexM(ring [][2]float64, lat, lon float64) float64 {
	far := 0.0
	for _, v := range ring {
		far = math.Max(far, distanceM(lat, lon, v[1], v[0]))
	}
	return far
}

func TestHexResForRadius(t *testing.T) {
	for _, tc := range []struct {
		radiusM, lat float64
		want         int
	}{
		{1000, 0, 11},    // 1069 m circumradius at the equator
		{1000, 37.4, 10}, // res 11 is 849 m on the ground here
		{1000, 63, 9},    // res 10 is under 1 km north of ~62°
		{100, 0, 14},
		{50000, 45, 4},
	} {
		if got := maxHexResForRadius(tc.radiusM, tc.lat); got != tc.want {
			t.Errorf("maxHexResForRadius(%v, %v) = %d, want %d", tc.radiusM, tc.lat, got, tc.want)
		}
	}
	// Whatever the latitude, a fix binned at the capped resolution has a
	// vertex at least the radius away: its cell cannot place it closer.
	rng := rand.New(rand.NewSource(7))
	for _, r := range []float64{100, 500, 1000, 5000, 50000} {
		for i := 0; i < 400; i++ {
			lat := (rng.Float64()*2 - 1) * 85
			lon := rng.Float64()*360 - 180
			res := maxHexResForRadius(r, lat)
			if far := farthestVertexM(hexBoundary(hexCellAt(lat, lon, res)), lat, lon); far < r {
				t.Fatalf("r=%v lat=%.4f: res %d cell's farthest vertex %.0f m < radius", r, lat, res, far)
			}
			// And it is the finest such resolution, up to the poleward margin.
			if res < 17 && hexSizeForRes(res+2)*math.Cos(lat*math.Pi/180) >= r {
				t.Fatalf("r=%v lat=%.4f: res %d is coarser than needed", r, lat, res)
			}
		}
	}
}

func TestFixCellCentre(t *testing.T) {
	if la, lo := fixCellCentre(0, 37.445987, -122.161728); la != 37.445987 || lo != -122.161728 {
		t.Fatalf("radius 0 must be the identity, got %v,%v", la, lo)
	}
	rng := rand.New(rand.NewSource(11))
	for _, r := range []float64{100, 1000, 50000} {
		h := fixCellFrac * r / locMetresPerDeg
		for i := 0; i < 2000; i++ {
			lat := (rng.Float64()*2 - 1) * 89
			lon := rng.Float64()*360 - 180
			cl, co := fixCellCentre(r, lat, lon)
			if cl == 0 && co == 0 {
				t.Fatalf("centre (0,0) reads as no fix")
			}
			if co < -180 || co >= 180 || math.Abs(cl-lat) > h/2+1e-9 {
				t.Fatalf("r=%v %v,%v → centre %v,%v outside the row", r, lat, lon, cl, co)
			}
			// Deterministic, and the same centre for every point of the cell.
			if a, b := fixCellCentre(r, cl, co); a != cl || b != co {
				t.Fatalf("centre %v,%v does not map to itself (%v,%v)", cl, co, a, b)
			}
			// The cell is at least 2R wide on the ground, also at its
			// poleward edge: the point's column neighbours are ≥ 2R apart.
			row := math.Floor(lat / h)
			edge := math.Max(math.Abs(row*h), math.Abs((row+1)*h))
			if ncol := fixCellCols(edge, fixCellFrac*r); ncol > 1 {
				wM := 360 / ncol * locMetresPerDeg * math.Cos(math.Min(edge, fixCellMaxLat)*math.Pi/180)
				if wM < fixCellFrac*r-1e-6 {
					t.Fatalf("r=%v lat=%v: cell %.1f m wide < %v", r, lat, wM, fixCellFrac*r)
				}
			}
		}
	}
	// The antimeridian is a column edge: both sides stay in [-180, 180).
	for _, lon := range []float64{-180, 179.999999, 180, 540.5} {
		if _, co := fixCellCentre(1000, 10, lon); co < -180 || co >= 180 {
			t.Errorf("lon %v → centre lon %v", lon, co)
		}
	}
}

// The widened SQL prefilter followed by binFixesInBBox keeps exactly the fixes
// whose cell centre lies in the bbox: none is lost at an edge.
func TestBinFixesInBBoxMatchesCentreRule(t *testing.T) {
	type fix struct{ Lat, Lon float64 }
	rng := rand.New(rand.NewSource(3))
	for _, base := range [][2]float64{{37.4, -122.2}, {69.6, 18.9}, {-33.9, 151.2}, {0.01, 179.98}} {
		var all []fix
		for i := 0; i < 3000; i++ {
			all = append(all, fix{base[0] + (rng.Float64()-0.5)*0.4, base[1] + (rng.Float64()-0.5)*0.4})
		}
		for k := 0; k < 50; k++ {
			s, w := base[0]+(rng.Float64()-0.5)*0.3, base[1]+(rng.Float64()-0.5)*0.3
			b := bbox{MinLat: s, MinLon: w, MaxLat: s + rng.Float64()*0.05, MaxLon: w + rng.Float64()*0.05}
			want := 0
			for _, f := range all {
				if b.contains(fixCellCentre(cellTestRadiusM, f.Lat, f.Lon)) {
					want++
				}
			}
			qb := b.widenForFixCells(cellTestRadiusM)
			var pre []fix
			for _, f := range all {
				if qb.contains(f.Lat, f.Lon) {
					pre = append(pre, f)
				}
			}
			got := binFixesInBBox(pre, cellTestRadiusM, b, func(f *fix) (*float64, *float64) { return &f.Lat, &f.Lon })
			if len(got) != want {
				t.Fatalf("base %v bbox %+v: %d fixes kept, want %d", base, b, len(got), want)
			}
			for _, f := range got {
				if c, o := fixCellCentre(cellTestRadiusM, f.Lat, f.Lon); c != f.Lat || o != f.Lon {
					t.Fatalf("kept row %v is not a cell centre", f)
				}
			}
		}
	}
	if b := (bbox{1, 2, 3, 4}); b.widenForFixCells(0) != b {
		t.Error("radius 0 must leave the bbox as it is")
	}
}

// R9/R10 on /api/rx-coverage (with gaps) and /api/rf-noise: the bbox filters
// cell centres, cells are capped to the radius and drawn around the centre.
func TestRxCoveragePrivacyBinsBeforeBBox(t *testing.T) {
	srv := cellTestServer(t, cellTestRadiusM)
	now := time.Now().UTC().Format(time.RFC3339)
	fixLat, fixLon := 37.445987, -122.161728
	insRx(t, srv.db, "compa", "aabbcc", now, fixLat, fixLon)
	insTrack(t, srv.db, "compa", now, fixLat, fixLon, 0)
	gapLat, gapLon := 37.60, -122.40
	insTrack(t, srv.db, "compa", now, gapLat, gapLon, 0)
	mustExecDB(t, srv.db, `UPDATE client_rf_samples SET noise_floor = -110`)
	cLat, cLon := fixCellCentre(cellTestRadiusM, fixLat, fixLon)
	if distanceM(fixLat, fixLon, cLat, cLon) < 1 {
		t.Fatal("test fix sits on its cell centre; pick another")
	}
	box := func(lat, lon, d float64) string {
		return fmt.Sprintf("%.7f,%.7f,%.7f,%.7f", lat-d, lon-d, lat+d, lon+d)
	}
	// A box around the fix that misses the centre, and one around the centre
	// that misses the fix.
	d := math.Min(math.Abs(fixLat-cLat), math.Abs(fixLon-cLon)) / 2
	if d < 1e-6 {
		d = 1e-6
	}
	maxRes := maxHexResForRadius(cellTestRadiusM, 37.8)

	features := func(path string) (n int, polys [][][2]float64, cells []string) {
		t.Helper()
		if strings.HasPrefix(path, "/api/rf-noise") {
			rr := serveRfNoise(srv, path)
			var fc RfNoiseFeatureCollection
			if err := json.Unmarshal(rr.Body.Bytes(), &fc); err != nil || rr.Code != 200 {
				t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
			}
			for _, f := range fc.Features {
				polys, cells = append(polys, f.Geometry.Coordinates[0]), append(cells, f.Properties.Cell)
			}
			return len(fc.Features), polys, cells
		}
		fc := decodeCoverage(t, serveCoverage(srv, path))
		for _, f := range fc.Features {
			polys, cells = append(polys, f.Geometry.Coordinates[0]), append(cells, f.Properties.Cell)
		}
		for _, g := range fc.Gaps {
			polys, cells = append(polys, g.Geometry.Coordinates[0]), append(cells, g.Properties.Cell)
		}
		return len(fc.Features) + len(fc.Gaps), polys, cells
	}
	for _, route := range []string{"/api/rx-coverage?gaps=1&z=18&bbox=", "/api/rf-noise?z=18&bbox="} {
		if n, _, _ := features(route + box(fixLat, fixLon, d)); n != 0 {
			t.Errorf("%s: a box around the fix but not its cell centre returned %d features", route, n)
		}
		if n, _, _ := features(route + box(cLat, cLon, d)); n != 1 {
			t.Errorf("%s: a box around the cell centre but not the fix returned %d features, want 1", route, n)
		}
		n, polys, cells := features(route + "37.2,-122.6,37.8,-121.7")
		if n != 2 {
			t.Fatalf("%s: wide box: %d features, want 2 (the fix and the gap)", route, n)
		}
		for i, ring := range polys {
			res, _, _, _ := parseHexCell(cells[i])
			if res > maxRes {
				t.Errorf("%s: cell %s finer than the cap %d", route, cells[i], maxRes)
			}
			if far := math.Min(farthestVertexM(ring, fixLat, fixLon), farthestVertexM(ring, gapLat, gapLon)); far < cellTestRadiusM {
				t.Errorf("%s: cell %s places a fix within %.0f m", route, cells[i], far)
			}
		}
	}

	// The per-node route filters the same way.
	if rows, err := srv.queryCoverageRows(nodePK, bbox{fixLat - d, fixLon - d, fixLat + d, fixLon + d}); err != nil || len(rows) != 0 {
		t.Errorf("per-node: box around the fix: %d rows (%v), want 0", len(rows), err)
	}
	rows, err := srv.queryCoverageRows(nodePK, bbox{cLat - d, cLon - d, cLat + d, cLon + d})
	if err != nil || len(rows) != 1 || rows[0].Lat != cLat || rows[0].Lon != cLon {
		t.Errorf("per-node: box around the centre: %+v (%v), want the centre", rows, err)
	}
}

// With privacy off the bbox still applies to raw fixes and z=18 is served.
func TestRxCoveragePrivacyOffKeepsRawFixes(t *testing.T) {
	srv := cellTestServer(t, 0)
	insRx(t, srv.db, "compa", "aabbcc", time.Now().UTC().Format(time.RFC3339), 37.445987, -122.161728)
	fc := decodeCoverage(t, serveCoverage(srv, "/api/rx-coverage?z=18&bbox=37.445986,-122.161729,37.445988,-122.161727"))
	if len(fc.Features) != 1 || !strings.HasPrefix(fc.Features[0].Properties.Cell, "18:") {
		t.Fatalf("privacy off: %+v", fc.Features)
	}
}

func TestMobileReceptionsSummaryPrivacyMeasuresFromCells(t *testing.T) {
	srv := cellTestServer(t, cellTestRadiusM)
	mustExecDB(t, srv.db, `INSERT INTO client_receptions (rx_pubkey,heard_key,heard_keylen,snr,lat,lon,rx_at,ingested_at,src) VALUES
		('rx1','`+rxSumPK[:16]+`',8,1,51.123456,4.234567,'2026-11-10T11:00:00Z','x','discover')`)
	lat, lon := 51.0, 4.0
	got := srv.mobileReceptionsSummary(rxSumPK, "2026-11-05T00:00:00Z", &lat, &lon, rxSumPM())
	if got == nil || got.MaxDistanceKm == nil {
		t.Fatalf("summary = %+v", got)
	}
	cl, co := fixCellCentre(cellTestRadiusM, 51.123456, 4.234567)
	if want := haversineKm(lat, lon, cl, co); *got.MaxDistanceKm != want {
		t.Fatalf("max distance %v, want %v (to the cell centre)", *got.MaxDistanceKm, want)
	}
}

// Two fixes 300 m apart in one privacy cell: different leaderboard cells
// without privacy, one with it.
func TestRxLeaderboardPrivacyCells(t *testing.T) {
	lat1, lon1 := 51.05, 3.72
	lat2, lon2 := lat1, lon1+0.0043 // ~300 m east
	a1, b1 := fixCellCentre(cellTestRadiusM, lat1, lon1)
	if a2, b2 := fixCellCentre(cellTestRadiusM, lat2, lon2); a1 != a2 || b1 != b2 {
		t.Fatal("test fixes are not in one privacy cell; pick others")
	}
	for _, tc := range []struct {
		radius float64
		cells  int
	}{{0, 2}, {cellTestRadiusM, 1}} {
		srv := cellTestServer(t, tc.radius)
		now := time.Now().UTC().Format(time.RFC3339)
		insRx(t, srv.db, "compa", "aabb01", now, lat1, lon1)
		insRx(t, srv.db, "compa", "aabb02", now, lat2, lon2)
		obs, err := srv.rxLeaderboard(context.Background(), 7, 10)
		if err != nil || len(obs) != 1 || obs[0].Cells != tc.cells {
			t.Errorf("radius %v: %+v (%v), want %d cells", tc.radius, obs, err, tc.cells)
		}
	}
}

// R11: the estimate is served as Fuzz(node, estimate), radius grown by R.
func TestReportedNodesEstimateFuzzed(t *testing.T) {
	exact := map[string]PositionEstimate{}
	for _, n := range getReported(t, seedReported(t)).Nodes {
		if n.Estimate != nil {
			exact[n.Pubkey] = *n.Estimate
		}
	}
	srv := seedReported(t)
	fz := cellTestFuzzer(t, cellTestRadiusM)
	srv.cfg.setLocationFuzzer(fz)
	n := 0
	for _, nd := range getReported(t, srv).Nodes {
		if nd.Estimate == nil {
			continue
		}
		n++
		e := exact[nd.Pubkey]
		wLat, wLon := fz.Fuzz(nd.Pubkey, e.Lat, e.Lon)
		if nd.Estimate.Lat != wLat || nd.Estimate.Lon != wLon || nd.Estimate.RadiusM != e.RadiusM+cellTestRadiusM {
			t.Errorf("%s: estimate %+v, want %v,%v radius %v", nd.Pubkey, nd.Estimate, wLat, wLon, e.RadiusM+cellTestRadiusM)
		}
		if nd.Estimate.Lat == e.Lat && nd.Estimate.Lon == e.Lon {
			t.Errorf("%s: estimate served unfuzzed", nd.Pubkey)
		}
	}
	if n == 0 {
		t.Fatal("no estimate in the fixture")
	}
	var nilEst *PositionEstimate
	fuzzEstimate(fz, repA, nilEst) // no estimate: no-op, no panic
	e := PositionEstimate{Lat: 1, Lon: 2, RadiusM: 3}
	fuzzEstimate(nil, repA, &e)
	if e != (PositionEstimate{Lat: 1, Lon: 2, RadiusM: 3}) {
		t.Errorf("privacy off changed the estimate: %+v", e)
	}
}
