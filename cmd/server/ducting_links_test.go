package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ducting "github.com/meshcore-analyzer/ducting"
	"github.com/meshcore-analyzer/users"
)

// Tests for GET /api/ducting/links (ducting_links.go). Pubkeys are
// placeholder hex only (AGENTS.md PII rule).

var linksNow = time.Unix(1_800_000_000, 0).UTC()

func linksPK(n int) string { return fmt.Sprintf("%064x", n) }

// --- fake grid source ---

type linksFakeSrc struct {
	mu        sync.Mutex
	rows      []ductingGridRow
	grids     map[int64]*ducting.Grid
	blobs     map[int64][]byte // when set, Load decodes (benchmark realism)
	rowsErr   error
	loads     int
	loadsByVT map[int64]int
	steps     int
	gate      chan struct{} // when set, Load waits for it
}

func newLinksFakeSrc() *linksFakeSrc {
	return &linksFakeSrc{grids: map[int64]*ducting.Grid{}, loadsByVT: map[int64]int{}}
}

func (f *linksFakeSrc) add(g *ducting.Grid) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grids[g.ValidTime] = g
	f.rows = append(f.rows, ductingGridRow{ValidTime: g.ValidTime, RunTime: g.RunTime})
	sort.Slice(f.rows, func(i, j int) bool { return f.rows[i].ValidTime < f.rows[j].ValidTime })
}

func (f *linksFakeSrc) Rows() ([]ductingGridRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ductingGridRow(nil), f.rows...), f.rowsErr
}

func (f *linksFakeSrc) Step(int64) (*ducting.Grid, error) {
	f.mu.Lock()
	f.steps++
	f.mu.Unlock()
	return nil, errors.New("Step must not be used by the links analysis")
}

func (f *linksFakeSrc) Load(vt int64) (*ducting.Grid, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.loads++
	f.loadsByVT[vt]++
	g, b := f.grids[vt], f.blobs[vt]
	f.mu.Unlock()
	if b != nil {
		return ducting.Decode(b)
	}
	if g == nil {
		return nil, fmt.Errorf("no step %d", vt)
	}
	return g, nil
}

func (f *linksFakeSrc) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

var (
	linksNormal = ducting.Result{Class: ducting.ClassNormal}
	linksDuct   = ducting.Result{Class: ducting.ClassSurface, FminMHz: 400}
	linksSuper  = ducting.Result{Class: ducting.ClassSuper}
	linksNoData = ducting.Result{Class: ducting.ClassNoData}
)

// linksGrid is a 0.5° lattice over 30..65 N, 15 W..25 E, every point set by fill.
func linksGrid(t testing.TB, run, vt int64, fill func(lat, lon float64) ducting.Result) *ducting.Grid {
	t.Helper()
	g, err := ducting.NewGrid("gfs", run, vt, 65, -15, -0.5, 0.5, 81, 71)
	if err != nil {
		t.Fatal(err)
	}
	for i := range g.C {
		lat, lon := g.LatLon(i)
		g.Set(i, fill(lat, lon))
	}
	return g
}

func linksAll(r ducting.Result) func(lat, lon float64) ducting.Result {
	return func(float64, float64) ducting.Result { return r }
}

// linksBand ducts the box around the (50,0)-(50,3) test path, normal elsewhere.
func linksBand(lat, lon float64) ducting.Result {
	if lat >= 49 && lat <= 51 && lon >= -1 && lon <= 4 {
		return linksDuct
	}
	return linksNormal
}

// --- pure-input helpers ---

func linksEnd(pk string, lat, lon, h float64) DuctingLinkEnd {
	return DuctingLinkEnd{Pubkey: pk, Name: "n" + pk[58:], Lat: lat, Lon: lon, HeightM: h}
}

func linksIn(ends ...DuctingLinkEnd) ductingLinksInput {
	in := ductingLinksInput{Ends: map[string]DuctingLinkEnd{}, FreqMHz: 868, Days: 7}
	for _, e := range ends {
		in.Ends[e.Pubkey] = e
	}
	return in
}

func linksRecv(id int, t int64, a, b string) ductingLinkObs {
	return ductingLinkObs{obsID: id, t: t, a: a, b: b, via: ductingViaObserver}
}

var (
	lpA = linksPK(0xa1)
	lpB = linksPK(0xb2)
	lpC = linksPK(0xc3)
	lpD = linksPK(0xd4)
)

func mustBuild(t *testing.T, in ductingLinksInput, src ductingStepSource) *DuctingLinksResponse {
	t.Helper()
	resp, err := buildDuctingLinks(in, src, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestDuctingLinksDuctAlongPathAndStepWindow(t *testing.T) {
	t1 := linksNow.Unix() - 2*86400
	t2 := t1 + 3*3600
	src := newLinksFakeSrc()
	src.add(linksGrid(t, t1-6*3600, t1, linksBand))
	src.add(linksGrid(t, t1-6*3600, t2, linksAll(linksNormal)))

	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5))
	in.Recv = []ductingLinkObs{
		linksRecv(1, t1+1800, lpA, lpB), // T1: ducted
		linksRecv(2, t1-5400, lpA, lpB), // exactly -90 min: T1
		linksRecv(3, t2+5400, lpA, lpB), // exactly +90 min: T2, not ducted
		linksRecv(4, t2+5401, lpA, lpB), // outside the window: no step
	}
	resp := mustBuild(t, in, src)

	if resp.Status != "ok" || resp.FreqMHz != 868 || resp.Days != 7 {
		t.Fatalf("header = %+v", resp)
	}
	s := resp.Summary
	if s.Links != 1 || s.Receptions != 4 || s.WithGrid != 3 || s.DuringDuct != 2 {
		t.Fatalf("summary = %+v", s)
	}
	if s.BaselineDuctFrac != 0.5 { // 1 pair x 2 steps, one ducted
		t.Fatalf("baseline = %v", s.BaselineDuctFrac)
	}
	if len(resp.Links) != 1 {
		t.Fatalf("links = %d", len(resp.Links))
	}
	l := resp.Links[0]
	if l.A.Pubkey != lpA || l.B.Pubkey != lpB || l.Count != 4 {
		t.Fatalf("link = %+v", l)
	}
	if want := haversineKm(50, 0, 50, 3); l.DistKm != want {
		t.Errorf("dist %v want %v", l.DistKm, want)
	}
	if want := 4.12 * 2 * math.Sqrt(5); math.Abs(l.HorizonKm-want) > 1e-9 {
		t.Errorf("horizon %v want %v", l.HorizonKm, want)
	}
	// Newest first: 4, 3, 1, 2.
	wantT := []int64{t2 + 5401, t2 + 5400, t1 + 1800, t1 - 5400}
	wantD := []float64{-1, 0, 1, 1}
	for i, r := range l.Receptions {
		if r.T != wantT[i] || r.DuctFrac != wantD[i] {
			t.Errorf("reception %d = %+v, want t=%d duct=%v", i, r, wantT[i], wantD[i])
		}
		if wantD[i] >= 0 && r.SuperFrac != 0 {
			t.Errorf("reception %d superFrac = %v", i, r.SuperFrac)
		}
	}
	if l.Receptions[0].SuperFrac != -1 {
		t.Errorf("no-step superFrac = %v", l.Receptions[0].SuperFrac)
	}
	if src.loadCount() != 2 || src.loadsByVT[t1] != 1 || src.loadsByVT[t2] != 1 {
		t.Errorf("loads = %v, want each step once", src.loadsByVT)
	}
	if src.steps != 0 {
		t.Errorf("Step called %d times; bulk scans must use Load", src.steps)
	}
}

func TestDuctingLinksSuperAndNoData(t *testing.T) {
	t1 := linksNow.Unix() - 86400
	t2 := t1 + 3600
	src := newLinksFakeSrc()
	src.add(linksGrid(t, t1, t1, linksAll(linksSuper)))
	src.add(linksGrid(t, t1, t2, linksAll(linksNoData)))
	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5))
	in.Recv = []ductingLinkObs{linksRecv(1, t1, lpA, lpB), linksRecv(2, t2, lpA, lpB)}
	resp := mustBuild(t, in, src)

	r := resp.Links[0].Receptions
	if r[0].T != t2 || r[0].DuctFrac != -1 || r[0].SuperFrac != -1 {
		t.Errorf("no-data step: %+v", r[0])
	}
	if r[1].DuctFrac != 0 || r[1].SuperFrac != 1 {
		t.Errorf("super step: %+v", r[1])
	}
	// The no-data step is neither a usable reception nor a baseline combination.
	if s := resp.Summary; s.WithGrid != 1 || s.DuringDuct != 0 || s.BaselineDuctFrac != 0 {
		t.Errorf("summary = %+v", s)
	}
}

func TestDuctingLinksPathFracs(t *testing.T) {
	for _, c := range []struct {
		ps          ducting.PathStats
		duct, super float64
	}{
		{ducting.PathStats{Samples: 10, NoData: 2, Duct: 4, Super: 2}, 0.5, 0.25},
		{ducting.PathStats{Samples: 5, NoData: 5}, -1, -1},
		{ducting.PathStats{}, -1, -1},
	} {
		if d, s := ductingPathFracs(c.ps); d != c.duct || s != c.super {
			t.Errorf("%+v -> %v, %v want %v, %v", c.ps, d, s, c.duct, c.super)
		}
	}
}

func TestDuctingLinksHorizonAndMaxKm(t *testing.T) {
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, linksNow.Unix()-3600, linksAll(linksNormal)))

	d := haversineKm(50, 0, 50, 1)
	hFor := func(horizon float64) float64 { return math.Pow(horizon/(2*4.12), 2) }
	inside := hFor(d * 0.99)  // horizon just under the distance: counts
	outside := hFor(d * 1.01) // horizon just over it: line of sight, dropped
	pIn, pInB := linksPK(1), linksPK(2)
	pOut, pOutB := linksPK(3), linksPK(4)
	// Distance cap: ~999.6 km kept, ~1000.7 km dropped (meridian, 111.19 km/°).
	pNear, pNearB := linksPK(5), linksPK(6)
	pFar, pFarB := linksPK(7), linksPK(8)
	if dn, df := haversineKm(40, 0, 48.99, 0), haversineKm(40, 0, 49.0, 0); !(dn <= ductingLinksMaxKm && df > ductingLinksMaxKm) {
		t.Fatalf("fixture distances %v %v", dn, df)
	}
	in := linksIn(
		linksEnd(pIn, 50, 0, inside), linksEnd(pInB, 50, 1, inside),
		linksEnd(pOut, 50, 0, outside), linksEnd(pOutB, 50, 1, outside),
		linksEnd(pNear, 40, 0, 5), linksEnd(pNearB, 48.99, 0, 5),
		linksEnd(pFar, 40, 0, 5), linksEnd(pFarB, 49.0, 0, 5),
	)
	tt := linksNow.Unix() - 3600
	in.Recv = []ductingLinkObs{
		linksRecv(1, tt, pIn, pInB), linksRecv(2, tt, pOut, pOutB),
		linksRecv(3, tt, pNear, pNearB), linksRecv(4, tt, pFar, pFarB),
		linksRecv(5, tt, pIn, linksPK(99)), // unknown end
		linksRecv(6, tt, pIn, pIn),         // self
	}
	resp := mustBuild(t, in, src)
	got := map[string]bool{}
	for _, l := range resp.Links {
		got[l.A.Pubkey] = true
	}
	if !got[pIn] || got[pOut] || !got[pNear] || got[pFar] || len(resp.Links) != 2 {
		t.Fatalf("links = %v", got)
	}
	if resp.Summary.Links != 2 || resp.Summary.Receptions != 2 {
		t.Errorf("summary = %+v", resp.Summary)
	}
}

func TestDuctingLinksBaselineArithmetic(t *testing.T) {
	// Two pairs, three steps in the 7-day period and one before it.
	old := linksNow.Unix() - 8*86400
	s1, s2, s3 := linksNow.Unix()-3*86400, linksNow.Unix()-2*86400, linksNow.Unix()-86400
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, old, linksAll(linksDuct)))
	src.add(linksGrid(t, 0, s1, linksAll(linksDuct)))   // both pairs ducted
	src.add(linksGrid(t, 0, s2, linksBand))             // only the (50,0)-(50,3) pair
	src.add(linksGrid(t, 0, s3, linksAll(linksNormal))) // neither
	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5), linksEnd(lpC, 40, 10, 5), linksEnd(lpD, 41, 10, 5))
	in.Recv = []ductingLinkObs{linksRecv(1, s2, lpA, lpB), linksRecv(2, s2, lpC, lpD)}
	resp := mustBuild(t, in, src)
	if want := 3.0 / 6.0; resp.Summary.BaselineDuctFrac != want {
		t.Errorf("baseline = %v want %v", resp.Summary.BaselineDuctFrac, want)
	}
	if src.loadsByVT[old] != 0 {
		t.Errorf("step before the period was loaded")
	}
	if src.loadCount() != 3 {
		t.Errorf("loads = %d want 3", src.loadCount())
	}
	if resp.Summary.DuringDuct != 1 || resp.Summary.WithGrid != 2 {
		t.Errorf("summary = %+v", resp.Summary)
	}

	// No pairs: no combinations, no loads.
	src2 := newLinksFakeSrc()
	src2.add(linksGrid(t, 0, s1, linksAll(linksDuct)))
	resp = mustBuild(t, linksIn(), src2)
	if resp.Summary.BaselineDuctFrac != -1 || src2.loadCount() != 0 || resp.Links == nil {
		t.Errorf("empty: %+v loads=%d", resp.Summary, src2.loadCount())
	}
}

func TestDuctingLinksPairCapAndOrder(t *testing.T) {
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, linksNow.Unix()-3600, linksAll(linksNormal)))
	in := linksIn(linksEnd(lpA, 50, 0, 5))
	n := ductingLinksMaxPairs + 5
	for i := 0; i < n; i++ {
		pk := linksPK(1000 + i)
		in.Ends[pk] = linksEnd(pk, 50, 0.5+float64(i)*0.01, 5)
		in.Recv = append(in.Recv, linksRecv(i, linksNow.Unix()-3600, lpA, pk))
	}
	resp := mustBuild(t, in, src)
	if resp.Summary.Links != n || resp.Summary.Receptions != n || len(resp.Links) != ductingLinksMaxPairs {
		t.Fatalf("summary %+v, listed %d", resp.Summary, len(resp.Links))
	}
	for i := 1; i < len(resp.Links); i++ {
		if resp.Links[i].DistKm > resp.Links[i-1].DistKm {
			t.Fatalf("not sorted by distance desc at %d", i)
		}
	}
	if resp.Links[0].B.Pubkey != linksPK(1000+n-1) {
		t.Errorf("longest pair = %s", resp.Links[0].B.Pubkey)
	}
	for _, l := range resp.Links {
		if l.B.Pubkey == linksPK(1000) {
			t.Errorf("shortest pair listed past the cap")
		}
	}
}

func TestDuctingLinksReceptionCapNewestFirst(t *testing.T) {
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, linksNow.Unix()-3600, linksAll(linksNormal)))
	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5))
	snr, rssi := -7.5, -110.0
	for i := 0; i < 60; i++ {
		r := linksRecv(i, linksNow.Unix()-int64(i/2)*60, lpA, lpB) // pairs share a time
		r.snr, r.rssi = &snr, &rssi
		in.Recv = append(in.Recv, r)
	}
	l := mustBuild(t, in, src).Links[0]
	if l.Count != 60 || len(l.Receptions) != ductingLinksMaxRecv {
		t.Fatalf("count %d listed %d", l.Count, len(l.Receptions))
	}
	if l.Receptions[0].T != linksNow.Unix() || l.Receptions[49].T != linksNow.Unix()-24*60 {
		t.Errorf("order: first %d last %d", l.Receptions[0].T, l.Receptions[49].T)
	}
	for i := 1; i < len(l.Receptions); i++ {
		if l.Receptions[i].T > l.Receptions[i-1].T {
			t.Fatalf("not newest first at %d", i)
		}
	}
	if l.Receptions[0].SNR == nil || *l.Receptions[0].SNR != snr || *l.Receptions[0].RSSI != rssi {
		t.Errorf("signal = %+v", l.Receptions[0])
	}
}

func TestDuctingLinksMemo(t *testing.T) {
	// A forecast step after now is outside the baseline, so a warm memo
	// needs no Load at all.
	vt := linksNow.Unix() + 3600
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 1, vt, linksBand))
	memo := newDuctingLinksMemo(10)
	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5))
	in.Recv = []ductingLinkObs{linksRecv(1, linksNow.Unix(), lpA, lpB)}
	in.Memo = memo

	first := mustBuild(t, in, src)
	second := mustBuild(t, in, src)
	if src.loadCount() != 1 {
		t.Fatalf("loads = %d, want 1 (second build from the memo)", src.loadCount())
	}
	if first.Links[0].Receptions[0].DuctFrac != 1 || second.Links[0].Receptions[0].DuctFrac != 1 {
		t.Errorf("memo result differs: %+v %+v", first.Links[0].Receptions[0], second.Links[0].Receptions[0])
	}
	// A moved node is a different path: miss.
	in.Ends[lpB] = linksEnd(lpB, 50, 3.5, 5)
	mustBuild(t, in, src)
	if src.loadCount() != 2 {
		t.Errorf("moved node served from the memo")
	}
	// A newer run of the same valid time is a miss too.
	src2 := newLinksFakeSrc()
	src2.add(linksGrid(t, 2, vt, linksAll(linksNormal)))
	in.Ends[lpB] = linksEnd(lpB, 50, 3, 5)
	if r := mustBuild(t, in, src2).Links[0].Receptions[0]; r.DuctFrac != 0 || src2.loadCount() != 1 {
		t.Errorf("new run: %+v loads=%d", r, src2.loadCount())
	}
}

func TestDuctingLinksMemoBound(t *testing.T) {
	m := newDuctingLinksMemo(3)
	for i := 0; i < 5; i++ {
		m.put(ductingLinksMemoKey{obsID: i}, ductingLinksMemoVal{duct: float64(i)})
	}
	if m.len() != 3 {
		t.Fatalf("len = %d", m.len())
	}
	for i := 0; i < 5; i++ {
		_, ok := m.get(ductingLinksMemoKey{obsID: i})
		if ok != (i >= 2) {
			t.Errorf("key %d present=%v; oldest must go first", i, ok)
		}
	}
	m.put(ductingLinksMemoKey{obsID: 4}, ductingLinksMemoVal{duct: 9}) // update: no eviction
	if v, _ := m.get(ductingLinksMemoKey{obsID: 2}); v.duct != 2 || m.len() != 3 {
		t.Errorf("update evicted")
	}
	if newDuctingLinksMemo(ductingLinksMaxMemo).max != ductingLinksMaxMemo {
		t.Errorf("default bound")
	}
}

func TestDuctingLinksUnreadableStep(t *testing.T) {
	t1 := linksNow.Unix() - 3600
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, t1, linksAll(linksDuct)))
	src.rows = append(src.rows, ductingGridRow{ValidTime: t1 - 3*3600}) // row without a grid
	sort.Slice(src.rows, func(i, j int) bool { return src.rows[i].ValidTime < src.rows[j].ValidTime })
	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5))
	in.Recv = []ductingLinkObs{linksRecv(1, t1, lpA, lpB), linksRecv(2, t1-3*3600, lpA, lpB)}
	resp := mustBuild(t, in, src)
	if s := resp.Summary; s.WithGrid != 1 || s.DuringDuct != 1 || s.BaselineDuctFrac != 1 {
		t.Errorf("summary = %+v", s)
	}
}

// --- gathering from a Server ---

func linksAdvert(id, route int, origin, decodedExtra string, obs ...*StoreObs) *StoreTx {
	tx := dhTx(route, PayloadADVERT, `{"type":"ADVERT","pubKey":"`+origin+`"`+decodedExtra+`}`)
	tx.ID = id
	tx.Observations = obs
	return tx
}

func linksObs(id int, observer, path string, at time.Time) *StoreObs {
	snr := 5.0
	return &StoreObs{ID: id, ObserverID: observer, PathJSON: path, Timestamp: at.UTC().Format(time.RFC3339), SNR: &snr}
}

// linksServer is a store holding txs (in first-seen order, as s.packets is)
// indexed by payload type, over the given node cache.
func linksServer(cfg *Config, nodes []nodeInfo, txs ...*StoreTx) *Server {
	st := &PacketStore{byPayloadType: map[int][]*StoreTx{}, packets: txs}
	for _, tx := range txs {
		pt := PayloadADVERT
		if tx.PayloadType != nil {
			pt = *tx.PayloadType
		}
		st.byPayloadType[pt] = append(st.byPayloadType[pt], tx)
	}
	st.nodeCache = nodes
	st.nodePM = buildPrefixMap(nodes)
	st.nodeCacheTime = time.Now()
	for _, tx := range txs {
		for _, o := range tx.Observations {
			st.totalObs++
			if o.ID > st.maxObsID {
				st.maxObsID = o.ID
			}
		}
	}
	return &Server{cfg: cfg, store: st}
}

func linksNode(pk, name string, lat, lon float64) nodeInfo {
	return nodeInfo{PublicKey: pk, Name: name, Lat: lat, Lon: lon, HasGPS: !(lat == 0 && lon == 0)}
}

func TestDuctingLinksGatherEvidenceAndPrivacy(t *testing.T) {
	adv := linksPK(0x0a)        // advertiser, visible
	obs := linksPK(0x0b)        // observer node, visible
	blocked := linksPK(0x0c)    // NodeBlacklist
	hiddenName := linksPK(0x0d) // hiddenNamePrefixes
	noFix := linksPK(0x0e)      // no position
	blObserver := linksPK(0x0f) // observerBlacklist, visible node
	notANode := linksPK(0x10)   // observer that is no node
	upperObs := linksPK(0xab11) // observer id reported in upper case
	nodes := []nodeInfo{
		linksNode(adv, "adv", 50, 0), linksNode(obs, "obs", 50, 3),
		linksNode(blocked, "blocked", 50, 4), linksNode(hiddenName, "Hidden x", 50, 5),
		linksNode(noFix, "nofix", 0, 0), linksNode(blObserver, "blobs", 50, 6),
		linksNode(upperObs, "upper", 50, 7),
	}
	cfg := &Config{NodeBlacklist: []string{blocked}, HiddenNamePrefixes: []string{"Hidden"}, ObserverBlacklist: []string{blObserver}}
	at := linksNow.Add(-time.Hour)
	own := linksObs(14, obs, "[]", at)
	own.Direction = "tx"
	srv := linksServer(cfg, nodes,
		linksAdvert(1, RouteFlood, adv, "",
			linksObs(10, obs, "[]", at),        // included: unchecked signature
			linksObs(11, obs, `["aa"]`, at),    // relayed: excluded
			linksObs(12, blObserver, "[]", at), // blacklisted observer
			linksObs(13, notANode, "[]", at),   // observer not a node
			own,                                // own transmission
			linksObs(15, obs, "[]", linksNow.Add(-8*24*time.Hour)), // before the period
			linksObs(16, adv, "[]", at),                            // heard itself
			linksObs(17, strings.ToUpper(upperObs), "[]", at),      // upper-case id: included
		),
		linksAdvert(2, RouteFlood, blocked, "", linksObs(20, obs, "[]", at)),
		linksAdvert(3, RouteFlood, hiddenName, "", linksObs(30, obs, "[]", at)),
		linksAdvert(4, RouteFlood, noFix, "", linksObs(40, obs, "[]", at)),
		linksAdvert(5, RouteFlood, adv, `,"signatureValid":false`, linksObs(50, obs, "[]", at)),
		linksAdvert(6, RouteFlood, adv, `,"signatureValid":true`, linksObs(60, obs, "[]", at)),
		linksAdvert(7, RouteDirect, adv, "", linksObs(70, obs, "", at), linksObs(71, obs, `["bb"]`, at)), // zero-hop direct
		linksAdvert(8, RouteFlood, obs, "", linksObs(80, blocked, "[]", at)),                             // hidden observer
	)
	in, err := srv.ductingLinksInputFor(7, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, r := range in.Recv {
		got = append(got, r.obsID)
		if r.a != adv {
			t.Errorf("obs %d attributed to %s", r.obsID, r.a)
		}
		if r.snr == nil || *r.snr != 5 {
			t.Errorf("obs %d snr not copied", r.obsID)
		}
	}
	sort.Ints(got)
	want := []int{10, 17, 60, 70}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("receptions %v want %v", got, want)
	}
	for _, pk := range []string{blocked, hiddenName, noFix} {
		if _, ok := in.Ends[pk]; ok {
			t.Errorf("%s is an end", pk)
		}
	}
	if in.FreqMHz != ductingLinksDefaultFreqMHz || in.Days != 7 {
		t.Errorf("freq/days = %v/%d", in.FreqMHz, in.Days)
	}
}

func TestDuctingLinksHeightsAndFreq(t *testing.T) {
	f := newAuthFixture(t)
	alice := f.registerAndActivate(t, "alice@example.org", "Alice", pw)
	adv, obs := linksPK(0x21), linksPK(0x22)
	h := 30.0
	if _, err := f.st.SaveRepeaterInfo(alice.me.ID, adv, users.RepeaterEdit{HeightM: &h}); err != nil {
		t.Fatal(err)
	}
	srv := linksServer(&Config{LOS: &LOSConfig{Enabled: true, FrequencyMHz: 915, AntennaHeightM: 8}},
		[]nodeInfo{linksNode(adv, "a", 50, 0), linksNode(obs, "b", 50, 3)})
	srv.auth = f.srv.auth
	in, err := srv.ductingLinksInputFor(7, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	if e := in.Ends[adv]; !e.HeightKnown || e.HeightM != 30 {
		t.Errorf("known end = %+v", e)
	}
	if e := in.Ends[obs]; e.HeightKnown || e.HeightM != 8 {
		t.Errorf("default end = %+v", e)
	}
	if in.FreqMHz != 915 {
		t.Errorf("freq = %v", in.FreqMHz)
	}

	// Without user management or LoS: the LoS default height, 868 MHz.
	in, err = linksServer(&Config{}, []nodeInfo{linksNode(adv, "a", 50, 0)}).ductingLinksInputFor(7, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	if e := in.Ends[adv]; e.HeightKnown || e.HeightM != defaultLOSAntennaM || in.FreqMHz != 868 {
		t.Errorf("defaults: %+v freq %v", e, in.FreqMHz)
	}
	// LoS configured but disabled: its frequency is not used.
	if f := ductingLinksFreq(&Config{LOS: &LOSConfig{FrequencyMHz: 433}}); f != 868 {
		t.Errorf("disabled los freq = %v", f)
	}
}

// --- HTTP ---

func linksGet(st *ductingLinksState, s *Server, src ductingStepSource, query string, historyDays int) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	st.serve(w, httptest.NewRequest("GET", "/api/ducting/links"+query, nil), s, src, historyDays)
	return w
}

func linksDecode(t *testing.T, w *httptest.ResponseRecorder) DuctingLinksResponse {
	t.Helper()
	var r DuctingLinksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return r
}

func TestDuctingLinksServeEndToEnd(t *testing.T) {
	adv, obs := linksPK(0x31), linksPK(0x32)
	at := linksNow.Add(-time.Hour)
	srv := linksServer(&Config{}, []nodeInfo{linksNode(adv, "a", 50, 0), linksNode(obs, "b", 50, 3)},
		linksAdvert(1, RouteFlood, adv, "", linksObs(10, obs, "[]", at)))
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, at.Unix(), linksBand))
	st := newDuctingLinksState()
	st.now = func() time.Time { return linksNow }

	w := linksGet(st, srv, src, "", 7)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=60" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("code %d headers %v", w.Code, w.Header())
	}
	r := linksDecode(t, w)
	if r.Status != "ok" || r.Days != 7 || len(r.Links) != 1 || r.Links[0].Receptions[0].DuctFrac != 1 || r.Summary.DuringDuct != 1 {
		t.Fatalf("resp = %+v", r)
	}
}

func TestDuctingLinksDaysValidation(t *testing.T) {
	st := newDuctingLinksState()
	src := newLinksFakeSrc()
	srv := &Server{cfg: &Config{}}
	for _, q := range []string{"?days=0", "?days=-1", "?days=8", "?days=abc", "?days=1.5", "?days=%20"} {
		if w := linksGet(st, srv, src, q, 7); w.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d", q, w.Code)
		}
	}
	for _, c := range []struct {
		q    string
		want int
	}{{"", 7}, {"?days=1", 1}, {"?days=7", 7}} {
		w := linksGet(st, srv, src, c.q, 7)
		if r := linksDecode(t, w); w.Code != 200 || r.Days != c.want || r.Status != "pending" {
			t.Errorf("%q: code %d resp %+v", c.q, w.Code, r)
		}
	}
}

func TestDuctingLinksPendingAndError(t *testing.T) {
	st := newDuctingLinksState()
	gathers := 0
	st.gather = func(*Server, int, time.Time) (ductingLinksInput, error) { gathers++; return ductingLinksInput{}, nil }
	src := newLinksFakeSrc()
	srv := &Server{cfg: &Config{}}
	r := linksDecode(t, linksGet(st, srv, src, "", 7))
	if r.Status != "pending" || r.Links == nil || r.Summary.BaselineDuctFrac != -1 || r.FreqMHz != 868 || gathers != 0 {
		t.Errorf("pending = %+v gathers=%d", r, gathers)
	}

	src.rowsErr = errors.New("database is locked at /secret/path")
	w := linksGet(st, srv, src, "", 7)
	r = linksDecode(t, w)
	if w.Code != 200 || r.Status != "error" || r.Error == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("error = %d %+v %v", w.Code, r, w.Header())
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "locked") {
		t.Errorf("error leaks internals: %s", w.Body.String())
	}

	// A gather failure is an error answer and is not cached.
	src.rowsErr = nil
	src.add(linksGrid(t, 0, linksNow.Unix(), linksAll(linksNormal)))
	st.now = func() time.Time { return linksNow }
	st.gather = func(*Server, int, time.Time) (ductingLinksInput, error) {
		gathers++
		return ductingLinksInput{}, errors.New("repeater heights: disk I/O error")
	}
	for i := 0; i < 2; i++ {
		if r := linksDecode(t, linksGet(st, srv, src, "", 7)); r.Status != "error" {
			t.Errorf("gather error: %+v", r)
		}
	}
	if gathers != 2 {
		t.Errorf("gathers = %d; errors must not be cached", gathers)
	}
}

func TestDuctingLinksCacheAndSingleflight(t *testing.T) {
	t1, t2 := linksNow.Unix()-2*3600, linksNow.Unix()-3600
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, t1, linksBand))
	src.add(linksGrid(t, 0, t2, linksAll(linksNormal)))
	src.gate = make(chan struct{})

	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5))
	in.Recv = []ductingLinkObs{linksRecv(1, t1, lpA, lpB), linksRecv(2, t2, lpA, lpB)}
	var mu sync.Mutex
	gathers := 0
	st := newDuctingLinksState()
	clock := linksNow
	st.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	st.gather = func(*Server, int, time.Time) (ductingLinksInput, error) {
		mu.Lock()
		gathers++
		mu.Unlock()
		cp := in
		cp.Ends = map[string]DuctingLinkEnd{lpA: in.Ends[lpA], lpB: in.Ends[lpB]}
		return cp, nil
	}
	srv := &Server{cfg: &Config{}}

	const n = 8
	var wg sync.WaitGroup
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bodies[i] = linksGet(st, srv, src, "", 7).Body.String()
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let the requests queue on the one rebuild
	close(src.gate)
	wg.Wait()
	for i := 1; i < n; i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("concurrent answers differ")
		}
	}
	if src.loadCount() != 2 || gathers != 1 {
		t.Fatalf("concurrent: loads %d gathers %d, want 2 and 1", src.loadCount(), gathers)
	}

	// Within the TTL: served from the cache.
	mu.Lock()
	clock = linksNow.Add((ductingLinksCacheTTL - 1) * time.Second)
	mu.Unlock()
	linksGet(st, srv, src, "", 7)
	if src.loadCount() != 2 || gathers != 1 {
		t.Errorf("cached: loads %d gathers %d", src.loadCount(), gathers)
	}
	// Another days value is another entry.
	linksGet(st, srv, src, "?days=3", 7)
	if gathers != 2 {
		t.Errorf("days=3 shared the days=7 entry")
	}
	// A new grid run changes the key: rebuilt at once.
	src.mu.Lock()
	src.rows[1].RunTime = 99
	src.mu.Unlock()
	linksGet(st, srv, src, "", 7)
	if gathers != 3 {
		t.Errorf("new run did not rebuild")
	}
	// Past the TTL: rebuilt. The memo spares the path work, not the baseline loads.
	mu.Lock()
	clock = clock.Add(ductingLinksCacheTTL * time.Second)
	mu.Unlock()
	before := src.loadCount()
	linksGet(st, srv, src, "", 7)
	if gathers != 4 || src.loadCount() == before {
		t.Errorf("expired: gathers %d loads %d->%d", gathers, before, src.loadCount())
	}
	if st.memo.len() == 0 {
		t.Errorf("memo empty after rebuilds")
	}
}

func TestDuctingLinksCacheKeyStoreGeneration(t *testing.T) {
	adv, obs := linksPK(0x41), linksPK(0x42)
	at := linksNow.Add(-time.Hour)
	srv := linksServer(&Config{}, []nodeInfo{linksNode(adv, "a", 50, 0), linksNode(obs, "b", 50, 3)},
		linksAdvert(1, RouteFlood, adv, "", linksObs(10, obs, "[]", at)))
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, at.Unix(), linksAll(linksNormal)))
	st := newDuctingLinksState()
	st.now = func() time.Time { return linksNow }
	if r := linksDecode(t, linksGet(st, srv, src, "", 7)); r.Summary.Receptions != 1 {
		t.Fatalf("first = %+v", r.Summary)
	}
	// A new observation raises maxObsID: the cached answer is not reused.
	tx := srv.store.byPayloadType[PayloadADVERT][0]
	srv.store.mu.Lock()
	tx.Observations = append(tx.Observations, linksObs(11, obs, "[]", at.Add(time.Minute)))
	srv.store.maxObsID, srv.store.totalObs = 11, 2
	srv.store.mu.Unlock()
	if r := linksDecode(t, linksGet(st, srv, src, "", 7)); r.Summary.Receptions != 2 {
		t.Errorf("after new observation = %+v", r.Summary)
	}
	// A visibility rule change too.
	srv.cfg.SetNodeBlacklist([]string{adv})
	if r := linksDecode(t, linksGet(st, srv, src, "", 7)); r.Summary.Receptions != 0 {
		t.Errorf("after blacklisting = %+v", r.Summary)
	}
}

// BenchmarkDuctingLinksBuild: ~2,000 receptions over 300 pairs and 56 global
// 0.5° steps, each decoded from its stored blob as the real source does.
// Cold (no memo): every step is decoded once and every reception and
// (pair, step) path sampled.
func BenchmarkDuctingLinksBuild(b *testing.B) {
	src := newLinksFakeSrc()
	src.blobs = map[int64][]byte{}
	start := linksNow.Unix() - 7*86400
	var times []int64
	for s := 0; s < 56; s++ {
		vt := start + int64(s)*3*3600
		g, err := ducting.NewGrid("gfs", vt, vt, 90, 0, -0.5, 0.5, 720, 361)
		if err != nil {
			b.Fatal(err)
		}
		for i := range g.C {
			lat, lon := g.LatLon(i)
			switch {
			case (int(lat*2)+int(lon*2)+s)%5 == 0:
				g.Set(i, linksDuct)
			case (int(lat)+s)%3 == 0:
				g.Set(i, linksSuper)
			default:
				g.Set(i, linksNormal)
			}
		}
		blob, err := ducting.Encode(g)
		if err != nil {
			b.Fatal(err)
		}
		src.blobs[vt] = blob
		src.rows = append(src.rows, ductingGridRow{ValidTime: vt, RunTime: vt})
		times = append(times, vt)
	}
	in := ductingLinksInput{Ends: map[string]DuctingLinkEnd{}, FreqMHz: 868, Days: 7}
	for i := 0; i < 300; i++ {
		a, bb := linksPK(10000+i), linksPK(20000+i)
		lat := 35 + float64(i%25)
		in.Ends[a] = linksEnd(a, lat, float64(i%40)-10, 5)
		in.Ends[bb] = linksEnd(bb, lat+0.3, float64(i%40)-8, 5)
		for k := 0; k < 7; k++ {
			in.Recv = append(in.Recv, linksRecv(i*7+k, times[(i*7+k)%len(times)]+600, a, bb))
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := buildDuctingLinks(in, src, linksNow)
		if err != nil || resp.Summary.Links != 300 || resp.Summary.Receptions != 2100 {
			b.Fatalf("resp %+v err %v", resp.Summary, err)
		}
	}
}
