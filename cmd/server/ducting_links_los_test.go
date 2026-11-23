package main

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	ducting "github.com/meshcore-analyzer/ducting"
)

// Line-of-sight candidates and the continuous refraction metric of
// /api/ducting/links (ducting_links.go). Pubkeys are placeholder hex only.

// linksTerrain is an in-memory ductingTerrain, shaped like the planner
// tests' fakeTerrain: elevation from elev over any box, an error to return,
// and a record of the boxes asked for.
type linksTerrain struct {
	mu      sync.Mutex
	elev    func(lat, lon float64) (float64, bool)
	err     error // returned for every view
	tooMany bool  // non-point boxes answer errTerrainTooManyTiles
	views   []geoBBox
}

func (f *linksTerrain) View(_ context.Context, bb geoBBox) (terrainView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views = append(f.views, bb)
	if f.err != nil {
		return nil, f.err
	}
	if f.tooMany && (bb.S != bb.N || bb.W != bb.E) {
		return nil, errTerrainTooManyTiles
	}
	return linksTerrainView{f.elev}, nil
}

func (f *linksTerrain) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.views)
}

type linksTerrainView struct {
	elev func(lat, lon float64) (float64, bool)
}

func (v linksTerrainView) ElevationAt(lat, lon float64) (float64, bool) { return v.elev(lat, lon) }

func flatTerrain(m float64) func(lat, lon float64) (float64, bool) {
	return func(float64, float64) (float64, bool) { return m, true }
}

// hillTops is 100 m terrain with 600 m summits within 0.005° of the given
// longitudes (the pair's two ends).
func hillTops(lons ...float64) func(lat, lon float64) (float64, bool) {
	return func(_, lon float64) (float64, bool) {
		for _, l := range lons {
			if math.Abs(lon-l) < 0.005 {
				return 600, true
			}
		}
		return 100, true
	}
}

func losIn(t *linksTerrain, ends ...DuctingLinkEnd) ductingLinksInput {
	in := linksIn(ends...)
	in.Terrain, in.KFactor = t, 4.0/3.0
	return in
}

func normalSrc(t *testing.T, vts ...int64) *linksFakeSrc {
	src := newLinksFakeSrc()
	for _, vt := range vts {
		src.add(linksGrid(t, 0, vt, linksAll(linksNormal)))
	}
	return src
}

func linkKeys(r *DuctingLinksResponse) map[string]bool {
	out := map[string]bool{}
	for _, l := range r.Links {
		out[l.A.Pubkey+">"+l.B.Pubkey] = true
	}
	return out
}

// Two ~61 km pairs with 5 m antennas: over flat 100 m ground the k = 4/3
// bulge (~53 m mid-path) cuts the ray (blocked: a candidate); between two
// 600 m summits it does not (line of sight: left out).
func TestDuctingLinksLOSBlockedVsClear(t *testing.T) {
	tt := linksNow.Unix() - 3600
	lonB := 0.85
	pA, pB := linksPK(1), linksPK(2) // summits at lon 0 and 0.85, lat 50
	pC, pD := linksPK(3), linksPK(4) // flat, lat 45 (the summits are at lat 50 only)
	ter := &linksTerrain{elev: func(lat, lon float64) (float64, bool) {
		if lat > 47 {
			return hillTops(0, lonB)(lat, lon)
		}
		return 100, true
	}}
	in := losIn(ter, linksEnd(pA, 50, 0, 5), linksEnd(pB, 50, lonB, 5), linksEnd(pC, 45, 0, 5), linksEnd(pD, 45, 0.78, 5))
	in.Recv = []ductingLinkObs{linksRecv(1, tt, pA, pB), linksRecv(2, tt, pC, pD)}
	if d := haversineKm(50, 0, 50, lonB); d > losMaxLegKm || d <= ductingHorizonKm(5, 5) {
		t.Fatalf("fixture distance %v", d)
	}
	resp := mustBuild(t, in, normalSrc(t, tt))
	if got := linkKeys(resp); len(got) != 1 || !got[pC+">"+pD] {
		t.Fatalf("links = %v, want only the flat (blocked) pair", got)
	}
	if s := resp.Summary; s.LosMethod != "terrain" || s.UnverifiedPairs != 0 || s.Links != 1 || s.Receptions != 1 {
		t.Errorf("summary = %+v", s)
	}
	// The profile view covers both ends (one box per pair).
	if ter.count() != 2 {
		t.Errorf("views = %d", ter.count())
	}

	// The same pairs over smooth earth (no terrain): both are candidates.
	in.Terrain = nil
	resp = mustBuild(t, in, normalSrc(t, tt))
	if len(resp.Links) != 2 || resp.Summary.LosMethod != "smooth-earth" {
		t.Errorf("smooth earth: %d links, %q", len(resp.Links), resp.Summary.LosMethod)
	}
}

// Past losMaxLegKm only the endpoints are read: the horizon at k from
// antenna plus ground height. Low ground: beyond it; very high ground:
// within it.
func TestDuctingLinksLOSOverMaxLeg(t *testing.T) {
	tt := linksNow.Unix() - 3600
	pA, pB := linksPK(1), linksPK(2)
	d := haversineKm(40, 0, 40, 4.7)
	if d <= losMaxLegKm || d > ductingLinksMaxKm {
		t.Fatalf("fixture distance %v", d)
	}
	for _, c := range []struct {
		ground float64
		want   bool
	}{{100, true}, {9000, false}} {
		ter := &linksTerrain{elev: flatTerrain(c.ground)}
		in := losIn(ter, linksEnd(pA, 40, 0, 5), linksEnd(pB, 40, 4.7, 5))
		in.Recv = []ductingLinkObs{linksRecv(1, tt, pA, pB)}
		resp := mustBuild(t, in, normalSrc(t, tt))
		if got := len(resp.Links) == 1; got != c.want {
			t.Errorf("ground %v m: candidate %v, want %v (horizon %.0f km, dist %.0f km)",
				c.ground, got, c.want, ductingHorizonKmK(5+c.ground, 5+c.ground, 4.0/3.0), d)
		}
		for _, bb := range ter.views {
			if bb.S != bb.N || bb.W != bb.E {
				t.Errorf("profile view %+v for a leg over losMaxLegKm", bb)
			}
		}
		if ter.count() != 2 {
			t.Errorf("views = %d, want one per end", ter.count())
		}
	}
	// A leg whose box spans too many tiles falls back to the same rule:
	// the 61 km flat pair that the profile calls blocked is within the
	// endpoint horizon (~84 km at 100 m ground).
	ter := &linksTerrain{elev: flatTerrain(100), tooMany: true}
	in := losIn(ter, linksEnd(pA, 45, 0, 5), linksEnd(pB, 45, 0.78, 5))
	in.Recv = []ductingLinkObs{linksRecv(1, tt, pA, pB)}
	if resp := mustBuild(t, in, normalSrc(t, tt)); len(resp.Links) != 0 || resp.Summary.UnverifiedPairs != 0 {
		t.Errorf("too many tiles: %+v", resp.Summary)
	}
}

// Pairs inside the antenna-height horizon never reach the terrain.
func TestDuctingLinksLOSPrefilter(t *testing.T) {
	tt := linksNow.Unix() - 3600
	pA, pB := linksPK(1), linksPK(2)
	ter := &linksTerrain{elev: flatTerrain(100)}
	in := losIn(ter, linksEnd(pA, 50, 0, 5), linksEnd(pB, 50, 0.2, 5)) // ~14 km < 18 km
	in.Recv = []ductingLinkObs{linksRecv(1, tt, pA, pB)}
	resp := mustBuild(t, in, normalSrc(t, tt))
	if len(resp.Links) != 0 || ter.count() != 0 || resp.Summary.UnverifiedPairs != 0 {
		t.Errorf("links %d views %d summary %+v", len(resp.Links), ter.count(), resp.Summary)
	}
}

// Verdicts survive rebuilds (no second terrain read), A->B and B->A share
// one, a transient failure is not cached and leaves the pair unverified, a
// place without terrain data is cached as unverified, and the cache is
// bounded.
func TestDuctingLinksLOSCache(t *testing.T) {
	tt := linksNow.Unix() - 3600
	pA, pB, pC := linksPK(1), linksPK(2), linksPK(3)
	ter := &linksTerrain{elev: flatTerrain(100)}
	in := losIn(ter, linksEnd(pA, 45, 0, 5), linksEnd(pB, 45, 0.78, 5), linksEnd(pC, 45, 1.6, 5))
	in.LOS = newDuctingLOSCache(10)
	in.Recv = []ductingLinkObs{linksRecv(1, tt, pA, pB), linksRecv(2, tt, pB, pA), linksRecv(3, tt, pB, pC)}
	resp := mustBuild(t, in, normalSrc(t, tt))
	if len(resp.Links) != 3 || ter.count() != 2 || in.LOS.len() != 2 {
		t.Fatalf("links %d views %d cached %d; A->B and B->A must share a verdict", len(resp.Links), ter.count(), in.LOS.len())
	}
	mustBuild(t, in, normalSrc(t, tt))
	if ter.count() != 2 {
		t.Errorf("rebuild read terrain again: %d views", ter.count())
	}
	// A moved end is a new verdict.
	in.Ends[pC] = linksEnd(pC, 45, 1.7, 5)
	mustBuild(t, in, normalSrc(t, tt))
	if ter.count() != 3 {
		t.Errorf("moved end served from the cache")
	}

	// Transient failure: unverified, retried next time.
	busy := &linksTerrain{elev: flatTerrain(100), err: errTerrainBusy}
	in2 := losIn(busy, linksEnd(pA, 45, 0, 5), linksEnd(pB, 45, 0.78, 5))
	in2.LOS = newDuctingLOSCache(10)
	in2.Recv = []ductingLinkObs{linksRecv(1, tt, pA, pB)}
	for i := 1; i <= 2; i++ {
		resp := mustBuild(t, in2, normalSrc(t, tt))
		if len(resp.Links) != 0 || resp.Summary.UnverifiedPairs != 1 || busy.count() != i || in2.LOS.len() != 0 {
			t.Fatalf("busy %d: links %d summary %+v views %d", i, len(resp.Links), resp.Summary, busy.count())
		}
	}
	// No terrain data at all: unverified, and not asked again.
	void := &linksTerrain{elev: func(float64, float64) (float64, bool) { return 0, false }}
	in2.Terrain = void
	for i := 0; i < 2; i++ {
		if resp := mustBuild(t, in2, normalSrc(t, tt)); resp.Summary.UnverifiedPairs != 1 {
			t.Fatalf("void: %+v", resp.Summary)
		}
	}
	if void.count() != 2 { // first rebuild only: the profile view, then the first end's point view
		t.Errorf("void views = %d", void.count())
	}

	// Bound.
	c := newDuctingLOSCache(2)
	for i := 0; i < 3; i++ {
		c.put(ductingLOSKey{a: linksPK(i)}, ductingLOSBeyond)
	}
	if _, ok := c.get(ductingLOSKey{a: linksPK(0)}); ok || c.len() != 2 {
		t.Errorf("cache bound: len %d, oldest kept %v", c.len(), ok)
	}
	if newDuctingLOSCache(ductingLinksLOSMaxCache).max != ductingLinksLOSMaxCache {
		t.Errorf("default bound")
	}
}

// At most ductingLinksLOSChecks new terrain checks per rebuild, longest
// pairs first; the rest are unverified until a later rebuild.
func TestDuctingLinksLOSCheckBudget(t *testing.T) {
	tt := linksNow.Unix() - 3600
	ter := &linksTerrain{elev: flatTerrain(100)}
	in := losIn(ter, linksEnd(lpA, 45, 0, 5))
	in.LOS = newDuctingLOSCache(ductingLinksLOSMaxCache)
	n := ductingLinksLOSChecks + 5
	for i := 0; i < n; i++ {
		pk := linksPK(1000 + i)
		in.Ends[pk] = linksEnd(pk, 45, 0.5+float64(i)*0.001, 5)
		in.Recv = append(in.Recv, linksRecv(i, tt, lpA, pk))
	}
	resp := mustBuild(t, in, normalSrc(t, tt))
	if resp.Summary.UnverifiedPairs != 5 || resp.Summary.Links != ductingLinksLOSChecks || ter.count() != ductingLinksLOSChecks {
		t.Fatalf("summary %+v views %d", resp.Summary, ter.count())
	}
	for _, l := range resp.Links {
		if l.B.Pubkey == linksPK(1000) {
			t.Fatal("the shortest pair was checked before a longer one")
		}
	}
	resp = mustBuild(t, in, normalSrc(t, tt))
	if resp.Summary.UnverifiedPairs != 0 || resp.Summary.Links != n || ter.count() != n {
		t.Errorf("second rebuild: summary %+v views %d", resp.Summary, ter.count())
	}
}

// The continuous metric: per-reception mean dN/dh and enhanced share, and
// the summary means and shares over receptions and the baseline.
func TestDuctingLinksRefractionMetric(t *testing.T) {
	s1, s2, s3 := linksNow.Unix()-3*3600, linksNow.Unix()-2*3600, linksNow.Unix()-3600
	src := newLinksFakeSrc()
	// The class and the mean gradient disagree on purpose: s1 is high by
	// the gradient but normal by class, s2 super by class but not high.
	src.add(linksGrid(t, 0, s1, linksAll(ducting.Result{Class: ducting.ClassNormal, MeanGradNkm: -100})))
	src.add(linksGrid(t, 0, s2, linksAll(ducting.Result{Class: ducting.ClassSuper, MeanGradNkm: -40})))
	src.add(linksGrid(t, 0, s3, linksAll(linksNormal))) // no N values
	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5))
	in.Recv = []ductingLinkObs{
		linksRecv(1, s1, lpA, lpB),
		linksRecv(2, s2, lpA, lpB),
		linksRecv(3, s3, lpA, lpB),
		linksRecv(4, s3+4*3600, lpA, lpB), // no step
	}
	resp := mustBuild(t, in, src)
	r := resp.Links[0].Receptions // newest first: 4, 3, 2, 1
	if r[0].MeanGradNkm != nil || r[0].SuperFrac != -1 || r[0].HighFrac != -1 || r[0].DuctFrac != -1 {
		t.Errorf("no step: %+v", r[0])
	}
	if r[1].MeanGradNkm != nil || r[1].SuperFrac != 0 || r[1].HighFrac != 0 {
		t.Errorf("no N: %+v", r[1])
	}
	if r[2].MeanGradNkm == nil || *r[2].MeanGradNkm != -40 || r[2].SuperFrac != 1 || r[2].HighFrac != 0 {
		t.Errorf("super by class, not high: %+v", r[2])
	}
	if r[3].MeanGradNkm == nil || *r[3].MeanGradNkm != -100 || r[3].SuperFrac != 0 || r[3].HighFrac != 1 || r[3].DuctFrac != 0 {
		t.Errorf("high, normal by class: %+v", r[3])
	}
	s := resp.Summary
	if s.WithGrid != 3 || s.RecvEnhancedFrac != 1.0/3 || s.RecvMeanGradNkm == nil || *s.RecvMeanGradNkm != -70 {
		t.Errorf("receptions: %+v (mean %v)", s, deref(s.RecvMeanGradNkm))
	}
	// Baseline: 1 pair x 3 steps; one high (s1, not the super-class s2); N on two of them.
	if s.BaselineEnhancedFrac != 1.0/3 || s.BaselineMeanGradNkm == nil || *s.BaselineMeanGradNkm != -70 || s.BaselineDuctFrac != 0 {
		t.Errorf("baseline: %+v (mean %v)", s, deref(s.BaselineMeanGradNkm))
	}

	// No data at all: nil means, -1 shares.
	empty := mustBuild(t, linksIn(), src)
	if e := empty.Summary; e.RecvMeanGradNkm != nil || e.BaselineMeanGradNkm != nil || e.RecvEnhancedFrac != -1 || e.BaselineEnhancedFrac != -1 {
		t.Errorf("empty: %+v", e)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// JSON names of the new fields, and the server deciding the method.
func TestDuctingLinksLOSServer(t *testing.T) {
	srv := &Server{cfg: &Config{}}
	if ter, _ := srv.ductingLinksTerrain(); ter != nil || srv.ductingLinksLOSMethod() != "smooth-earth" {
		t.Errorf("LoS disabled: terrain %v", ter)
	}
	srv = &Server{cfg: &Config{LOS: &LOSConfig{Enabled: true, KFactor: 1.2}}}
	if ter, _ := srv.ductingLinksTerrain(); ter != nil {
		t.Errorf("no elevation service: terrain %v", ter)
	}
	srv.elevation = newElevationService(nil)
	if ter, k := srv.ductingLinksTerrain(); ter == nil || k != 1.2 || srv.ductingLinksLOSMethod() != "terrain" {
		t.Errorf("terrain %v k %v", ter, k)
	}

	at := linksNow.Add(-time.Hour)
	srv = linksServer(&Config{}, []nodeInfo{linksNode(lpA, "a", 50, 0), linksNode(lpB, "b", 50, 3)},
		linksAdvert(1, RouteFlood, lpA, "", linksObs(10, lpB, "[]", at)))
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, at.Unix(), linksAll(ducting.Result{Class: ducting.ClassSuper, MeanGradNkm: -90})))
	st := newDuctingLinksState()
	st.now = func() time.Time { return linksNow }
	body := linksGet(st, srv, src, "", 7).Body.String()
	for _, f := range []string{`"losMethod":"smooth-earth"`, `"unverifiedPairs":0`, `"recvMeanGradNkm":-90`, `"baselineMeanGradNkm":-90`,
		`"recvEnhancedFrac":1`, `"baselineEnhancedFrac":1`, `"meanGradNkm":-90`, `"highFrac":1`, `"superFrac":1`} {
		if !strings.Contains(body, f) {
			t.Errorf("body lacks %s: %s", f, body)
		}
	}
	// Pending answers carry the method and -1 shares too.
	body = linksGet(newDuctingLinksState(), srv, newLinksFakeSrc(), "", 7).Body.String()
	if !strings.Contains(body, `"losMethod":"smooth-earth"`) || !strings.Contains(body, `"recvEnhancedFrac":-1`) {
		t.Errorf("pending: %s", body)
	}
}

// BenchmarkDuctingLinksLOSCandidates: classifying 400 pairs of 30-290 km
// cold (every pair a profile read and analyzeLOS; the budget's worst case)
// over resident in-memory terrain, and warm (every verdict cached).
func BenchmarkDuctingLinksLOSCandidates(b *testing.B) {
	ter := &linksTerrain{elev: func(lat, lon float64) (float64, bool) { return 100 + 50*math.Sin(lat*40)*math.Cos(lon*40), true }}
	in := ductingLinksInput{Ends: map[string]DuctingLinkEnd{}, FreqMHz: 868, Terrain: ter, KFactor: 4.0 / 3.0}
	var pairs []*ductingLinkPair
	for i := 0; i < ductingLinksLOSChecks; i++ {
		a, bb := linksPK(10000+i), linksPK(20000+i)
		in.Ends[a] = linksEnd(a, 40+float64(i%20)*0.1, 0, 5)
		in.Ends[bb] = linksEnd(bb, 40+float64(i%20)*0.1, 0.35+float64(i)*0.0084, 5)
		pairs = append(pairs, &ductingLinkPair{a: a, b: bb, distKm: haversineKm(in.Ends[a].Lat, 0, in.Ends[bb].Lat, in.Ends[bb].Lon)})
	}
	for _, mode := range []string{"cold", "warm"} {
		b.Run(mode, func(b *testing.B) {
			in.LOS = newDuctingLOSCache(ductingLinksLOSMaxCache)
			if mode == "warm" {
				in.losCandidates(pairs)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if mode == "cold" {
					b.StopTimer()
					in.LOS = newDuctingLOSCache(ductingLinksLOSMaxCache)
					b.StartTimer()
				}
				in.losCandidates(pairs)
			}
		})
	}
}
