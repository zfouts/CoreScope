package main

// GET /api/ducting/links: observed long links against the stored ducting
// grid.
//
// The question it answers: when the mesh heard a node directly over more
// than the smooth-earth radio horizon, did the model show a duct along that
// path at that time, and how often are the same paths ducted at all?
//
// Evidence is certain only, from two sources:
//
//   - via "observer": an observation of an ADVERT whose path is empty
//     (directHeardNode's zero-hop case: a flood with no hops, or a
//     direct-route zero-hop advert), so the observer heard the advertiser's
//     own transmission and the advert names it. The observer must be a node.
//   - via "relay": consecutive hops of a flood path, each of which only one
//     node can be (the #2260 liveness rule: the observation's path is bound
//     to its own flood frame, relayHopHexLen, and the hop prefix resolves to
//     exactly one path-capable node and no retired one, uniqueHopPrefix).
//     path[i] transmitted and path[i+1] heard it; an ADVERT also gives
//     origin -> path[0]. Direct routes carry the remaining route, never the
//     travelled one, so they give no pairs. One reception per (transmission,
//     pair), at the transmission's earliest observation, without SNR/RSSI
//     (those belong to the last hop's link to the observer).
//
// Excluded everywhere: the observer's own transmissions (direction "tx"),
// transmissions whose advert signature failed, blacklisted observers, and
// either end hidden by the /api/nodes visibility rule or without a position
// fix. Positions are the nodes' current ones, never old advert payloads.
//
// Cost: the candidate observations are copied under the store's read lock
// (only the fields needed, ductingScanChunk transmissions per hold; relay
// hops are resolved between holds), then all grid work runs without it. Each stored
// step is decoded at most once per rebuild through ductingStepSource.Load
// (or LoadPlanes, which decodes only the planes sampled; never the LRU behind
// Step), one at a time. Answers are cached per days
// for ductingLinksCacheTTL, concurrent requests share one rebuild, rebuilds
// run one at a time, and per-reception results are memoised across rebuilds
// in a bounded FIFO.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ducting "github.com/meshcore-analyzer/ducting"
	"golang.org/x/sync/singleflight"
)

// ductingLinksDefaultFreqMHz is the duct-test frequency when route LoS (and
// with it los.frequencyMhz) is not enabled.
const ductingLinksDefaultFreqMHz = 868.0

// Reception sources (DuctingReception.Via).
const (
	ductingViaObserver = "observer" // zero-hop advert heard by an observer that is a node
	ductingViaRelay    = "relay"    // consecutive certain hops of a flood path
)

// ductingLinkObs is one candidate reception, copied out of the store.
type ductingLinkObs struct {
	obsID int    // identity within via: StoreObs.ID (observer), StoreTx.ID (relay)
	t     int64  // reception time, unix seconds
	a, b  string // lowercase pubkeys: transmitter, receiver
	via   string // ductingViaObserver or ductingViaRelay
	snr   *float64
	rssi  *float64
}

// ductingLinksInput is everything buildDuctingLinks needs from the server,
// so the analysis is testable without a store or a Server.
type ductingLinksInput struct {
	Recv    []ductingLinkObs
	Ends    map[string]DuctingLinkEnd // visible nodes with a fix, by lowercase pubkey; heights resolved
	FreqMHz float64
	Days    int
	Memo    *ductingLinksMemo // nil: no memo
	// HistoryDays is the stored history; it sizes the receptions listed per
	// pair (ductingLinksRecvPerDay a day). 0: Days.
	HistoryDays int

	// Terrain is the line-of-sight terrain; nil: smooth-earth rule (antenna
	// heights only, k = 4/3). KFactor is the resolved los.kFactor. LOS keeps
	// verdicts across rebuilds (nil: none kept).
	Terrain ductingTerrain
	KFactor float64
	LOS     *ductingLOSCache
}

// ductingLinksFreq is the duct-test frequency: los.frequencyMhz (resolved,
// as /api/config/client sends it) when LoS is enabled, else 868 MHz.
func ductingLinksFreq(cfg *Config) float64 {
	if cfg != nil && cfg.LOS.enabled() {
		r, _ := cfg.LOS.resolved()
		return r.FrequencyMHz
	}
	return ductingLinksDefaultFreqMHz
}

// --- bounded caches ---

// ductingFIFO is a bounded map with first-in-first-out eviction, safe for
// concurrent use. A nil *ductingFIFO stores nothing.
type ductingFIFO[K comparable, V any] struct {
	mu   sync.Mutex
	m    map[K]V
	ring []K // insertion order; slot next is the oldest once full
	next int
	max  int
}

func newDuctingFIFO[K comparable, V any](max int) *ductingFIFO[K, V] {
	if max < 1 {
		max = 1
	}
	return &ductingFIFO[K, V]{m: make(map[K]V, min(max, 1024)), max: max}
}

func (m *ductingFIFO[K, V]) get(k K) (V, bool) {
	if m == nil {
		var zero V
		return zero, false
	}
	m.mu.Lock()
	v, ok := m.m[k]
	m.mu.Unlock()
	return v, ok
}

func (m *ductingFIFO[K, V]) put(k K, v V) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[k]; ok {
		m.m[k] = v
		return
	}
	if len(m.ring) < m.max {
		m.ring = append(m.ring, k)
	} else {
		delete(m.m, m.ring[m.next])
		m.ring[m.next] = k
		m.next = (m.next + 1) % m.max
	}
	m.m[k] = v
}

func (m *ductingFIFO[K, V]) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

// ductingLinksMemoKey identifies one reception's path result. Beyond the
// observation and the step's run, it holds everything else the result (and
// the pair's line-of-sight verdict) depends on, so a moved node, a new
// height, a re-run step or another frequency or k is a miss rather than a
// stale hit.
type ductingLinksMemoKey struct {
	obsID              int
	via                string
	a, b               string
	validTime, runTime int64
	latA, lonA         float64
	latB, lonB         float64
	hA, hB             float64
	k                  float64
	freq               float64
}

// ductingLinksMemo remembers per-reception path results across rebuilds.
type ductingLinksMemo = ductingFIFO[ductingLinksMemoKey, ductingPathMetric]

func newDuctingLinksMemo(max int) *ductingLinksMemo {
	return newDuctingFIFO[ductingLinksMemoKey, ductingPathMetric](max)
}

// --- line of sight ---

// Line-of-sight methods (DuctingLinksSummary.LosMethod).
const (
	ductingLOSTerrain     = "terrain"
	ductingLOSSmoothEarth = "smooth-earth"
)

const (
	ductingLinksLOSMaxCache = 20000            // verdicts kept across rebuilds
	ductingLinksLOSChecks   = 400              // new terrain checks per rebuild, longest pairs first
	ductingLinksLOSWait     = srtmFetchTimeout // a rebuild's whole terrain budget
)

// ductingTerrain is the terrain source the line-of-sight check reads
// (*srtmSource, shared with route LoS and the planner).
type ductingTerrain interface {
	View(ctx context.Context, bb geoBBox) (terrainView, error)
}

// ductingLOS is a pair's line-of-sight verdict.
type ductingLOS uint8

const (
	ductingLOSUnknown ductingLOS = iota // no terrain data where it was needed
	ductingLOSBeyond                    // no geometric line of sight: a candidate
	ductingLOSWithin                    // line of sight: not a candidate
)

// ductingLOSKey is everything a verdict depends on, with the ends in pubkey
// order so A->B and B->A share it.
type ductingLOSKey struct {
	a, b       string
	latA, lonA float64
	latB, lonB float64
	hA, hB     float64
	k, freq    float64
}

type ductingLOSCache = ductingFIFO[ductingLOSKey, ductingLOS]

func newDuctingLOSCache(max int) *ductingLOSCache {
	return newDuctingFIFO[ductingLOSKey, ductingLOS](max)
}

// ductingHorizonKm is the smooth-earth radio horizon between two antennas at
// k = 4/3, heights in metres (DuctingLink.HorizonKm).
func ductingHorizonKm(hA, hB float64) float64 {
	return 4.12 * (math.Sqrt(math.Max(0, hA)) + math.Sqrt(math.Max(0, hB)))
}

// ductingHorizonKmK is the smooth-earth radio horizon at effective earth
// radius factor k: sqrt(2kR h1) + sqrt(2kR h2), heights in metres.
func ductingHorizonKmK(hA, hB, k float64) float64 {
	r := 2 * k * earthRadiusKm / 1000
	return math.Sqrt(r*math.Max(0, hA)) + math.Sqrt(r*math.Max(0, hB))
}

// ductingLOSVerdict decides one pair over terrain. cache is false for a
// transient failure (budget, timeout, tiles not loadable now), which leaves
// the pair unverified until a later rebuild.
//
// Up to losMaxLegKm: the leg's SRTM profile (sampleLegProfile, as route LoS
// and the planner read it) through analyzeLOS at k; beyond line of sight iff
// its verdict is "blocked", i.e. terrain plus the k-earth bulge rises above
// the straight ray somewhere (clearance < 0, losVerdict). "marginal" (inside
// the first Fresnel zone but not cut) is line of sight. Longer legs, legs
// over more tiles than one view may load, and profiles that cross a tile
// without data use the endpoint rule: the smooth-earth horizon at k from
// antenna height plus ground elevation above sea level at each end, which
// overstates the horizon and so errs towards "within".
func ductingLOSVerdict(ctx context.Context, t ductingTerrain, a, b DuctingLinkEnd, distKm, k, freq float64) (ductingLOS, bool) {
	pa, pb := geoPoint{Lat: a.Lat, Lon: a.Lon}, geoPoint{Lat: b.Lat, Lon: b.Lon}
	if distKm <= losMaxLegKm {
		ends := plannerBBoxAdd(geoBBox{S: pa.Lat, W: pa.Lon, N: pa.Lat, E: pa.Lon}, pb)
		v, err := t.View(ctx, plannerProfileView(ends, plannerProfilePad(losMaxLegKm)))
		switch {
		case err == nil:
			if elev, ok := sampleLegProfile(v, pa, pb); ok {
				r, ok := analyzeLOS(elev, distKm, losParams{FreqMHz: freq, KFactor: k, AntennaAM: a.HeightM, AntennaBM: b.HeightM})
				if !ok {
					return ductingLOSUnknown, true
				}
				if r.Verdict == "blocked" {
					return ductingLOSBeyond, true
				}
				return ductingLOSWithin, true
			}
		case errors.Is(err, errTerrainTooManyTiles):
		default:
			return ductingLOSUnknown, false
		}
	}
	var ground [2]float64
	for i, p := range [2]geoPoint{pa, pb} {
		v, err := t.View(ctx, geoBBox{S: p.Lat, W: p.Lon, N: p.Lat, E: p.Lon})
		if err != nil {
			return ductingLOSUnknown, false
		}
		e, ok := v.ElevationAt(p.Lat, p.Lon)
		if !ok {
			return ductingLOSUnknown, true
		}
		ground[i] = math.Max(0, e)
	}
	if distKm > ductingHorizonKmK(a.HeightM+ground[0], b.HeightM+ground[1], k) {
		return ductingLOSBeyond, true
	}
	return ductingLOSWithin, true
}

// ductingLinksK is the effective earth radius factor of the line-of-sight
// test: the resolved los.kFactor with terrain, 4/3 without.
func (in *ductingLinksInput) losK() float64 {
	if in.Terrain != nil && in.KFactor > 0 {
		return in.KFactor
	}
	return defaultLOSKFactor
}

func (in *ductingLinksInput) losMethod() string {
	if in.Terrain != nil {
		return ductingLOSTerrain
	}
	return ductingLOSSmoothEarth
}

// losCandidates keeps the pairs beyond line of sight. pairs are already past
// the antenna-height smooth-earth horizon at k and sorted longest first, so
// the per-rebuild check budget goes to the longest. Without terrain every
// such pair is a candidate (the smooth-earth rule). Runs outside any store
// lock.
func (in *ductingLinksInput) losCandidates(pairs []*ductingLinkPair) (out []*ductingLinkPair, unverified int) {
	if in.Terrain == nil {
		return pairs, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), ductingLinksLOSWait)
	defer cancel()
	k, checks := in.losK(), 0
	out = make([]*ductingLinkPair, 0, len(pairs))
	for _, p := range pairs {
		ea, eb := in.Ends[p.a], in.Ends[p.b]
		key := ductingLOSKey{a: p.a, b: p.b, latA: ea.Lat, lonA: ea.Lon, latB: eb.Lat, lonB: eb.Lon, hA: ea.HeightM, hB: eb.HeightM, k: k, freq: in.FreqMHz}
		if key.a > key.b {
			key.a, key.b = key.b, key.a
			key.latA, key.lonA, key.latB, key.lonB = key.latB, key.lonB, key.latA, key.lonA
			key.hA, key.hB = key.hB, key.hA
		}
		v, ok := in.LOS.get(key)
		if !ok && checks < ductingLinksLOSChecks && ctx.Err() == nil {
			checks++
			var cache bool
			if v, cache = ductingLOSVerdict(ctx, in.Terrain, ea, eb, p.distKm, k, in.FreqMHz); cache {
				in.LOS.put(key, v)
			}
			ok = true
		}
		switch {
		case !ok || v == ductingLOSUnknown:
			unverified++
		case v == ductingLOSBeyond:
			out = append(out, p)
		}
	}
	return out, unverified
}

// --- analysis ---

type ductingLinkPair struct {
	a, b    string
	distKm  float64
	horizon float64
	recv    []int // indices into in.Recv
}

// ductingPathMetric is one path at one step: the shares of the samples with
// data that carry LoRa in a duct (duct), are super-refractive or ducting by
// class (super) and are in the map's High band or stronger (high); -1 for
// all three when no sample had data. hasN is false when no sample had a
// mean dN/dh.
type ductingPathMetric struct {
	duct, super, high float64
	meanN             float64
	hasN              bool
}

var ductingNoMetric = ductingPathMetric{duct: -1, super: -1, high: -1}

func ductingPathMetricOf(ps ducting.PathStats) ductingPathMetric {
	n := ps.Samples - ps.NoData
	if n <= 0 {
		return ductingNoMetric
	}
	m := ductingPathMetric{
		duct:  float64(ps.Duct) / float64(n),
		super: float64(ps.SuperOrDuct) / float64(n),
		high:  float64(ps.Enhanced) / float64(n),
	}
	if ps.NCount > 0 {
		m.meanN, m.hasN = ps.MeanN, true
	}
	return m
}

// ductingTally accumulates shares and a mean over path metrics with data.
type ductingTally struct {
	n, duct, enh int
	nN           int
	sumN         float64
}

func (t *ductingTally) add(m ductingPathMetric) {
	if m.duct < 0 {
		return
	}
	t.n++
	if m.duct >= ductingLinksDuctFrac {
		t.duct++
	}
	if m.high >= ductingLinksDuctFrac {
		t.enh++
	}
	if m.hasN {
		t.nN++
		t.sumN += m.meanN
	}
}

func (t *ductingTally) frac(k int) float64 {
	if t.n == 0 {
		return -1
	}
	return float64(k) / float64(t.n)
}

func (t *ductingTally) meanN() *float64 {
	if t.nN == 0 {
		return nil
	}
	v := t.sumN / float64(t.nN)
	return &v
}

// ductingNearestRow returns the index of the row whose ValidTime is nearest t
// within ±ductingLinksStepWindowS, or -1. rows ascend by ValidTime; on a tie
// the earlier row wins.
func ductingNearestRow(rows []ductingGridRow, t int64) int {
	i := sort.Search(len(rows), func(i int) bool { return rows[i].ValidTime >= t })
	best, bestD := -1, int64(ductingLinksStepWindowS)+1
	for _, j := range [2]int{i - 1, i} {
		if j < 0 || j >= len(rows) {
			continue
		}
		d := rows[j].ValidTime - t
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = j, d
		}
	}
	return best
}

// ductingLinksEmptySummary is the summary of an answer without data.
func ductingLinksEmptySummary(losMethod string) DuctingLinksSummary {
	return DuctingLinksSummary{BaselineDuctFrac: -1, RecvEnhancedFrac: -1, BaselineEnhancedFrac: -1, LosMethod: losMethod}
}

// buildDuctingLinks is the analysis over copied inputs. It never touches the
// store; the grid is read through src.Load (or LoadPlanes) only, one step at
// a time, and terrain through in.Terrain.
func buildDuctingLinks(in ductingLinksInput, src ductingStepSource, now time.Time) (*DuctingLinksResponse, error) {
	resp := &DuctingLinksResponse{
		Status:      "ok",
		FreqMHz:     in.FreqMHz,
		Days:        in.Days,
		GeneratedAt: now.Unix(),
		Summary:     ductingLinksEmptySummary(in.losMethod()),
		Links:       []DuctingLink{},
	}
	rows, err := src.Rows()
	if err != nil {
		return nil, fmt.Errorf("grid rows: %w", err)
	}
	if len(rows) == 0 {
		resp.Status = "pending"
		return resp, nil
	}
	if !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].ValidTime < rows[j].ValidTime }) {
		rows = append([]ductingGridRow(nil), rows...) // Rows is shared; never sort it in place
		sort.Slice(rows, func(i, j int) bool { return rows[i].ValidTime < rows[j].ValidTime })
	}

	// Pairs: one per (transmitter, receiver) within ductingLinksMaxKm and
	// past the antenna-height smooth-earth horizon at k (a pair inside it is
	// never beyond line of sight, so it needs no terrain).
	k := in.losK()
	type pairKey struct{ a, b string }
	byPair := make(map[pairKey]*ductingLinkPair)
	var pairs []*ductingLinkPair
	for i := range in.Recv {
		r := &in.Recv[i]
		pk := pairKey{r.a, r.b}
		p, seen := byPair[pk]
		if !seen {
			ea, okA := in.Ends[r.a]
			eb, okB := in.Ends[r.b]
			if okA && okB && r.a != r.b {
				d := haversineKm(ea.Lat, ea.Lon, eb.Lat, eb.Lon)
				if d > ductingHorizonKmK(ea.HeightM, eb.HeightM, k) && d <= ductingLinksMaxKm {
					p = &ductingLinkPair{a: r.a, b: r.b, distKm: d, horizon: ductingHorizonKm(ea.HeightM, eb.HeightM)}
					pairs = append(pairs, p)
				}
			}
			byPair[pk] = p // nil remembers a rejected pair
		}
		if p != nil {
			p.recv = append(p.recv, i)
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].distKm != pairs[j].distKm {
			return pairs[i].distKm > pairs[j].distKm
		}
		if pairs[i].a != pairs[j].a {
			return pairs[i].a < pairs[j].a
		}
		return pairs[i].b < pairs[j].b
	})
	pairs, resp.Summary.UnverifiedPairs = in.losCandidates(pairs)
	resp.Summary.Links = len(pairs)
	listed := pairs
	if len(listed) > ductingLinksMaxPairs {
		listed = listed[:ductingLinksMaxPairs]
	}
	// Every statistic below is over the listed pairs only, so the receptions
	// and the baseline describe the same paths.
	resp.Summary.ListedPairs = len(listed)

	// Per-reception step, from the memo where possible; the rest are grouped
	// by the row they need.
	metric := make([]ductingPathMetric, len(in.Recv))
	pairOf := make([]*ductingLinkPair, len(in.Recv))
	rowOf := make([]int, len(in.Recv))
	need := make(map[int][]int) // row index -> receptions still to compute
	for _, p := range listed {
		for _, i := range p.recv {
			resp.Summary.Receptions++
			metric[i] = ductingNoMetric
			pairOf[i] = p
			row := ductingNearestRow(rows, in.Recv[i].t)
			rowOf[i] = row
			if row < 0 {
				continue
			}
			if v, ok := in.Memo.get(in.memoKey(p, &in.Recv[i], rows[row], k)); ok {
				metric[i] = v
				continue
			}
			need[row] = append(need[row], i)
		}
	}

	// Baseline rows: every stored step valid in the period.
	fromU, nowU := now.Unix()-int64(in.Days)*86400, now.Unix()
	isBase := func(row int) bool { vt := rows[row].ValidTime; return vt >= fromU && vt <= nowU }

	steps := make([]int, 0, len(need))
	for row := range need {
		steps = append(steps, row)
	}
	if len(listed) > 0 {
		for row := range rows {
			if _, ok := need[row]; !ok && isBase(row) {
				steps = append(steps, row)
			}
		}
	}
	sort.Ints(steps)

	// Steps are visited one at a time and only sampled along paths, so a
	// source that can decode just the C, F and N planes into one reused
	// buffer does (2.4 MB and four planes less garbage per step).
	dec := ducting.Decoder{N: true}
	planes, _ := src.(ductingPlaneLoader)
	var base ductingTally
	for _, row := range steps {
		var g *ducting.Grid
		var err error
		if planes != nil {
			g, err = planes.LoadPlanes(rows[row].ValidTime, &dec)
		} else {
			g, err = src.Load(rows[row].ValidTime)
		}
		if err == nil && g == nil {
			err = errors.New("no grid")
		}
		if err != nil {
			// One unreadable step must not take the endpoint down: its
			// receptions stay "no stored step" and it leaves the baseline.
			log.Printf("[ducting] links: step %d unreadable, skipped: %v", rows[row].ValidTime, err)
			continue
		}
		for _, i := range need[row] {
			p := pairOf[i]
			ea, eb := in.Ends[p.a], in.Ends[p.b]
			metric[i] = ductingPathMetricOf(g.PathStats(ea.Lat, ea.Lon, eb.Lat, eb.Lon, in.FreqMHz, ductingLinksPathKm))
			in.Memo.put(in.memoKey(p, &in.Recv[i], rows[row], k), metric[i])
		}
		if len(listed) > 0 && isBase(row) {
			for _, p := range listed {
				ea, eb := in.Ends[p.a], in.Ends[p.b]
				// No data along the path at this step: not a combination.
				base.add(ductingPathMetricOf(g.PathStats(ea.Lat, ea.Lon, eb.Lat, eb.Lon, in.FreqMHz, ductingLinksPathKm)))
			}
		}
		// g goes out of scope here (or is reused by dec): at most one step is held.
	}
	resp.Summary.BaselineDuctFrac = base.frac(base.duct)
	resp.Summary.BaselineEnhancedFrac = base.frac(base.enh)
	resp.Summary.BaselineMeanGradNkm = base.meanN()

	var recv ductingTally
	for _, p := range listed {
		for _, i := range p.recv {
			recv.add(metric[i])
		}
	}
	resp.Summary.WithGrid = recv.n
	resp.Summary.DuringDuct = recv.duct
	resp.Summary.RecvEnhancedFrac = recv.frac(recv.enh)
	resp.Summary.RecvMeanGradNkm = recv.meanN()

	// Receptions listed per pair: thinned by time, not by count, so a busy
	// pair still has receptions across the whole period when the map's time
	// slider moves back. Newest first, the newest in each bucket: the stored
	// step the reception maps to, or for one without a step its
	// ductingLinksRecvBucketS UTC bucket; at most ductingLinksRecvPerDay per
	// day of history.
	maxRecv := in.HistoryDays
	if maxRecv < in.Days {
		maxRecv = in.Days
	}
	maxRecv *= ductingLinksRecvPerDay
	resp.Links = make([]DuctingLink, 0, len(listed))
	for _, p := range listed {
		idx := append([]int(nil), p.recv...)
		sort.Slice(idx, func(x, y int) bool {
			rx, ry := &in.Recv[idx[x]], &in.Recv[idx[y]]
			if rx.t != ry.t {
				return rx.t > ry.t
			}
			if rx.via != ry.via {
				return rx.via < ry.via
			}
			return rx.obsID > ry.obsID
		})
		seen := make(map[int64]struct{}, min(len(idx), maxRecv))
		kept := idx[:0]
		for _, i := range idx {
			if len(kept) >= maxRecv {
				break
			}
			bucket := in.Recv[i].t - ((in.Recv[i].t%ductingLinksRecvBucketS)+ductingLinksRecvBucketS)%ductingLinksRecvBucketS
			if row := rowOf[i]; row >= 0 {
				bucket = rows[row].ValidTime
			}
			if _, dup := seen[bucket]; dup {
				continue
			}
			seen[bucket] = struct{}{}
			kept = append(kept, i)
		}
		idx = kept
		recs := make([]DuctingReception, len(idx))
		for j, i := range idx {
			r, m := &in.Recv[i], metric[i]
			recs[j] = DuctingReception{T: r.t, Via: r.via, SNR: r.snr, RSSI: r.rssi, DuctFrac: m.duct, SuperFrac: m.super, HighFrac: m.high}
			if m.hasN {
				v := m.meanN
				recs[j].MeanGradNkm = &v
			}
		}
		resp.Links = append(resp.Links, DuctingLink{
			A: in.Ends[p.a], B: in.Ends[p.b],
			DistKm: p.distKm, HorizonKm: p.horizon,
			Count: len(p.recv), Receptions: recs,
		})
	}
	return resp, nil
}

func (in *ductingLinksInput) memoKey(p *ductingLinkPair, r *ductingLinkObs, row ductingGridRow, k float64) ductingLinksMemoKey {
	ea, eb := in.Ends[p.a], in.Ends[p.b]
	return ductingLinksMemoKey{
		obsID: r.obsID, via: r.via, a: p.a, b: p.b, validTime: row.ValidTime, runTime: row.RunTime,
		latA: ea.Lat, lonA: ea.Lon, latB: eb.Lat, lonB: eb.Lon, hA: ea.HeightM, hB: eb.HeightM, k: k, freq: in.FreqMHz,
	}
}

// --- gathering from the server ---

// ductingLinkHeights returns the antenna heights users entered for their
// repeaters (users.db), by lowercase pubkey; nil without user management.
func (s *Server) ductingLinkHeights() (map[string]float64, error) {
	if s.auth == nil || s.auth.st == nil {
		return nil, nil
	}
	infos, err := s.auth.st.ListRepeaterInfo()
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(infos))
	for _, ri := range infos {
		if ri.HeightM == nil {
			continue
		}
		if h := *ri.HeightM; h >= 0 && !math.IsInf(h, 0) {
			out[strings.ToLower(ri.Pubkey)] = h
		}
	}
	return out, nil
}

// ductingLinkEnds is every node /api/nodes would list that has a position
// fix, with its antenna height, by lowercase pubkey.
func (s *Server) ductingLinkEnds(nodes []nodeInfo) (map[string]DuctingLinkEnd, error) {
	heights, err := s.ductingLinkHeights()
	if err != nil {
		return nil, fmt.Errorf("repeater heights: %w", err)
	}
	var los *LOSConfig
	if s.cfg != nil {
		los = s.cfg.LOS
	}
	radio, _ := los.resolved()
	visible := s.visibleNodeInfo()
	ends := make(map[string]DuctingLinkEnd, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if !n.HasGPS || !(n.Lat >= -90 && n.Lat <= 90 && n.Lon >= -180 && n.Lon <= 180) || !visible(n) {
			continue
		}
		pk := strings.ToLower(n.PublicKey)
		h, known := heights[pk]
		if !known {
			h = radio.AntennaHeightM
		}
		ends[pk] = DuctingLinkEnd{Pubkey: pk, Name: n.Name, Lat: n.Lat, Lon: n.Lon, HeightM: h, HeightKnown: known}
	}
	return ends, nil
}

// ductingScanChunk is how many transmissions a links scan visits per hold of
// the store's read lock: ~0.5 ms in the worst case (every observation a
// reception), so a queued ingest writer never waits for a whole-store scan.
const ductingScanChunk = 256

// ductingScanChunks calls visit for every transmission of list() (read under
// the store's read lock), holding the lock for at most ductingScanChunk of
// them at a time, and calls flush (if not nil) after each hold, unlocked. A
// concurrent eviction compacts the list in place between chunks; the scan
// then resumes after the last transmission it saw, or, if that one was
// evicted, at the same index (which may skip a few). Eviction lowers
// totalObs, which changes the cache key of ductingLinksState, so the next
// request rebuilds from a settled store.
func (s *PacketStore) ductingScanChunks(list func() []*StoreTx, visit func(*StoreTx), flush func()) {
	var last *StoreTx
	for pos := 0; ; {
		s.mu.RLock()
		l := list()
		if last != nil && (pos > len(l) || l[pos-1] != last) {
			for i, tx := range l { // moved by an eviction: find where we were
				if tx == last {
					pos = i + 1
					break
				}
			}
		}
		if pos >= len(l) {
			s.mu.RUnlock()
			return
		}
		end := min(pos+ductingScanChunk, len(l))
		for _, tx := range l[pos:end] {
			visit(tx)
		}
		last = l[end-1]
		pos = end
		s.mu.RUnlock()
		if flush != nil {
			flush()
		}
	}
}

// ductingLinkReceptions copies the certain zero-hop advert receptions in
// [from, to] whose two ends are both in ends (via "observer"), scanning the
// ADVERT list in chunks (ductingScanChunks).
func (s *PacketStore) ductingLinkReceptions(ends map[string]DuctingLinkEnd, cfg *Config, from, to int64) []ductingLinkObs {
	var out []ductingLinkObs
	s.ductingScanChunks(func() []*StoreTx { return s.byPayloadType[PayloadADVERT] }, func(tx *StoreTx) {
		out = ductingAdvertReceptions(out, tx, ends, cfg, from, to)
	}, nil)
	return out
}

// ductingAdvertReceptions appends tx's qualifying receptions to out. The
// caller holds the store's read lock.
func ductingAdvertReceptions(out []ductingLinkObs, tx *StoreTx, ends map[string]DuctingLinkEnd, cfg *Config, from, to int64) []ductingLinkObs {
	sigChecked := false
	for _, obs := range tx.Observations {
		if obs.isOwnTransmission() || lastPathHop(obs.PathJSON) != "" {
			continue
		}
		b := strings.ToLower(obs.ObserverID)
		if _, ok := ends[b]; !ok || cfg.IsObserverBlacklisted(obs.ObserverID) {
			continue
		}
		ts, ok := obs.ParsedTime()
		if !ok || ts.Unix() < from || ts.Unix() > to {
			continue
		}
		// Empty path, so directHeardNode resolves only its zero-hop
		// case (the advert's own pubkey); no prefix map is consulted.
		a := directHeardNode(tx, obs, nil)
		if a == "" || a == b {
			continue
		}
		if _, ok := ends[a]; !ok {
			continue
		}
		if !sigChecked {
			if tx.signatureFailed() {
				break
			}
			sigChecked = true
		}
		out = append(out, ductingLinkObs{
			obsID: obs.ID, t: ts.Unix(), a: a, b: b, via: ductingViaObserver,
			snr: copyFloatPtr(obs.SNR), rssi: copyFloatPtr(obs.RSSI),
		})
	}
	return out
}

// ductingRelayPath is one distinct observed path of a transmission, with the
// hop width that binds it to its own flood frame (StoreObs.relayHopHexLen).
type ductingRelayPath struct {
	path   string // path_json as stored (shared, not copied)
	hexLen int
}

// ductingRelayTx is what the relay scan copies of one flood transmission
// under the lock: everything pair resolution needs, nothing else.
type ductingRelayTx struct {
	id     int
	t      int64  // earliest eligible observation, unix seconds
	origin string // advert pubkey (lowercase); "" when not an ADVERT
	paths  []ductingRelayPath
}

// ductingRelayCopy appends tx to buf when it is a flood transmission in
// [from, to] with at least one certain-width path. The caller holds the
// store's read lock. A failed advert signature drops the whole transmission,
// as liveness does (noteCertainRelays, certainFor).
func ductingRelayCopy(buf []ductingRelayTx, tx *StoreTx, cfg *Config, from, to int64) []ductingRelayTx {
	if tx.RouteType == nil || (*tx.RouteType != RouteFlood && *tx.RouteType != RouteTransportFlood) {
		return buf
	}
	var paths []ductingRelayPath
	first := int64(math.MaxInt64)
	for _, obs := range tx.Observations {
		if obs == nil || obs.isOwnTransmission() || cfg.IsObserverBlacklisted(obs.ObserverID) {
			continue
		}
		if ts, ok := obs.ParsedTime(); ok && ts.Unix() < first {
			first = ts.Unix()
		}
		// Zero for an empty path, a path that does not match its frame, a
		// TRACE or a legacy row without raw_hex: no certain hop at all.
		n := int(obs.relayHopHexLen)
		if n == 0 {
			continue
		}
		dup := false
		for _, p := range paths {
			if p.hexLen == n && p.path == obs.PathJSON {
				dup = true
				break
			}
		}
		if !dup {
			paths = append(paths, ductingRelayPath{path: obs.PathJSON, hexLen: n})
		}
	}
	if len(paths) == 0 || first < from || first > to || tx.signatureFailed() {
		return buf
	}
	return append(buf, ductingRelayTx{id: tx.ID, t: first, origin: advertOriginPubkey(tx), paths: paths})
}

// ductingHopCacheMax bounds the per-rebuild hop resolution cache.
const ductingHopCacheMax = 1 << 16

// ductingHopResolver resolves path hops to the one node they can only be,
// by the certain-liveness rule (#2260), caching per rebuild.
type ductingHopResolver struct {
	pm    *prefixMap
	cache map[string]string
}

// node returns the lowercase pubkey of the only node hop (n hex characters
// wide) can be, or "".
func (r *ductingHopResolver) node(hop string, n int) string {
	if len(hop) != n || r.pm == nil {
		return ""
	}
	if pk, ok := r.cache[hop]; ok {
		return pk
	}
	pk := ""
	if c := r.pm.relayCandidates(hop); len(c) == 1 {
		if cand := strings.ToLower(c[0].PublicKey); uniqueHopPrefix(cand, n, r.pm) {
			pk = cand
		}
	}
	if len(r.cache) < ductingHopCacheMax {
		r.cache[hop] = pk
	}
	return pk
}

// ductingRelayReceptions appends the certain pairs of one copied
// transmission, each (transmitter, receiver) once.
func ductingRelayReceptions(out []ductingLinkObs, rt *ductingRelayTx, res *ductingHopResolver, ends map[string]DuctingLinkEnd) []ductingLinkObs {
	start := len(out)
	add := func(a, b string) {
		if a == "" || b == "" || a == b {
			return
		}
		if _, ok := ends[a]; !ok {
			return
		}
		if _, ok := ends[b]; !ok {
			return
		}
		for _, o := range out[start:] {
			if o.a == a && o.b == b {
				return
			}
		}
		out = append(out, ductingLinkObs{obsID: rt.id, t: rt.t, a: a, b: b, via: ductingViaRelay})
	}
	for _, p := range rt.paths {
		prev := rt.origin // the originator transmitted to path[0]
		for pos := 0; ; {
			hop, next, ok := nextPathHop(p.path, pos)
			if !ok {
				break
			}
			pos = next
			cur := res.node(hop, p.hexLen)
			add(prev, cur) // an uncertain hop ("") drops only its own pairs
			prev = cur
		}
	}
	return out
}

// ductingRelayLinkReceptions scans every transmission in chunks, copying the
// flood paths under the read lock and resolving them between holds.
func (s *PacketStore) ductingRelayLinkReceptions(ends map[string]DuctingLinkEnd, pm *prefixMap, cfg *Config, from, to int64) []ductingLinkObs {
	var out []ductingLinkObs
	if pm == nil {
		return out
	}
	res := &ductingHopResolver{pm: pm, cache: make(map[string]string)}
	var buf []ductingRelayTx
	s.ductingScanChunks(func() []*StoreTx { return s.packets }, func(tx *StoreTx) {
		buf = ductingRelayCopy(buf, tx, cfg, from, to)
	}, func() {
		for i := range buf {
			out = ductingRelayReceptions(out, &buf[i], res, ends)
		}
		clear(buf)
		buf = buf[:0]
	})
	return out
}

func copyFloatPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// ductingLinksInputFor gathers the input for days ending at now.
func (s *Server) ductingLinksInputFor(days int, now time.Time) (ductingLinksInput, error) {
	in := ductingLinksInput{Days: days, FreqMHz: ductingLinksFreq(s.cfg)}
	if s.store == nil {
		return in, errors.New("no packet store")
	}
	nodes, pm := s.store.getCachedNodesAndPM()
	ends, err := s.ductingLinkEnds(nodes)
	if err != nil {
		return in, err
	}
	in.Ends = ends
	from, to := now.Unix()-int64(days)*86400, now.Unix()
	in.Recv = s.store.ductingLinkReceptions(ends, s.cfg, from, to)
	in.Recv = append(in.Recv, s.store.ductingRelayLinkReceptions(ends, pm, s.cfg, from, to)...)
	in.Terrain, in.KFactor = s.ductingLinksTerrain()
	return in, nil
}

// ductingLinksTerrain is the terrain source of the line-of-sight test (the
// one route LoS and the planner share) and the resolved los.kFactor, or nil
// when LoS terrain is not enabled.
func (s *Server) ductingLinksTerrain() (ductingTerrain, float64) {
	if s.cfg == nil || !s.cfg.LOS.enabled() || s.elevation == nil || s.elevation.src == nil {
		return nil, 0
	}
	r, _ := s.cfg.LOS.resolved()
	return s.elevation.src, r.KFactor
}

func (s *Server) ductingLinksLOSMethod() string {
	if t, _ := s.ductingLinksTerrain(); t != nil {
		return ductingLOSTerrain
	}
	return ductingLOSSmoothEarth
}

// --- HTTP, cache, singleflight ---

// ductingLinksBuildsPerMin is each client's budget of requests that start a
// rebuild (a first build for a key, or a background refresh of a stale one).
const ductingLinksBuildsPerMin = 10

// ductingLinksKey is what a cached answer is valid for: the grid generation
// (the newest run_time plus the row count and the last valid time: a new
// step or a pruned one), the visibility generation (nodeVisibilityGen: a
// newly hidden node never stays in a served answer) and whether terrain is
// available. New observations are not in it: on a live mesh they arrive
// every few seconds, and an exact match on them would rebuild for nearly
// every request. They, node positions, repeater heights and rewritten
// signature flags reach the answer through the stale-while-revalidate
// refresh after ductingLinksCacheTTL.
type ductingLinksKey struct {
	days      int
	runTime   int64
	rowCount  int
	lastValid int64
	vis       string
	losMethod string
}

func (k ductingLinksKey) String() string {
	return fmt.Sprintf("%d|%d|%d|%d|%s|%s", k.days, k.runTime, k.rowCount, k.lastValid, k.vis, k.losMethod)
}

type ductingLinksCached struct {
	key  ductingLinksKey
	at   time.Time
	body []byte
}

// errDuctingLinksLimited refuses a request that would start a build when the
// client's budget is spent; wait is when the next token comes.
type errDuctingLinksLimited struct{ wait time.Duration }

func (e errDuctingLinksLimited) Error() string { return "long-link rebuild rate limited" }

// ductingLinksState caches /api/ducting/links answers (one per days value,
// so at most ductingHistoryDaysMax entries) and serves them
// stale-while-revalidate: an answer for the same key is reused while it is
// younger than ductingLinksCacheTTL; an older one is served at once while
// one background rebuild replaces it. Only the first build for a key makes
// its caller wait, and concurrent callers share it (singleflight). Rebuilds
// run one at a time, each is charged to the client whose request started it,
// and the state keeps the per-reception memo and the line-of-sight verdicts.
type ductingLinksState struct {
	sf      singleflight.Group
	buildMu sync.Mutex     // one rebuild at a time, whatever the key
	bg      sync.WaitGroup // running background refreshes (tests wait on it)

	mu       sync.Mutex
	cache    map[int]*ductingLinksCached
	inflight map[string]bool // keys being built: joining one charges nothing

	memo *ductingLinksMemo
	los  *ductingLOSCache
	now  func() time.Time // tests replace it

	// limit is the per-client build budget, made from the server's trusted
	// proxies on first use unless already set (tests). nil: no limit.
	limit     *clientLimiter
	limitOnce sync.Once

	// gather collects the input; tests replace it. nil: Server.ductingLinksInputFor.
	gather func(s *Server, days int, now time.Time) (ductingLinksInput, error)
}

func newDuctingLinksState() *ductingLinksState {
	return &ductingLinksState{
		cache:    make(map[int]*ductingLinksCached),
		inflight: make(map[string]bool),
		memo:     newDuctingLinksMemo(ductingLinksMaxMemo),
		los:      newDuctingLOSCache(ductingLinksLOSMaxCache),
		now:      time.Now,
	}
}

func (st *ductingLinksState) limiter(s *Server) *clientLimiter {
	st.limitOnce.Do(func() {
		if st.limit == nil && s != nil && s.cfg != nil {
			st.limit = &clientLimiter{
				ipr: &wsLimiter{state: map[string]*wsIPState{},
					trustedProxies: parseCIDRList(s.cfg.WSTrustedProxies(), "webSocket.trustedProxies")},
				rl:  newRateLimiter(ductingLinksBuildsPerMin, time.Minute),
				tag: "ducting", what: "long-link rebuild",
			}
		}
	})
	return st.limit
}

// lookup returns the cached answer for k, if any, and whether it is still
// fresh.
func (st *ductingLinksState) lookup(k ductingLinksKey, now time.Time) (body []byte, fresh bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c := st.cache[k.days]
	if c == nil || c.key != k {
		return nil, false
	}
	return c.body, now.Sub(c.at) < ductingLinksCacheTTL*time.Second && !now.Before(c.at)
}

// claim marks k as being built by this request and charges the client, or
// reports that a build is already running (nothing charged). A refused
// charge leaves nothing marked.
func (st *ductingLinksState) claim(r *http.Request, s *Server, k ductingLinksKey) (started bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.inflight[k.String()] {
		return false, nil
	}
	if k.rowCount > 0 { // a pending answer costs nothing to build
		if ok, wait := st.limiter(s).take(r); !ok {
			return false, errDuctingLinksLimited{wait}
		}
	}
	st.inflight[k.String()] = true
	return true, nil
}

// serve answers GET /api/ducting/links?days=N. historyDays is the stored
// history (already clamped to 1..ductingHistoryDaysMax by the caller): days
// defaults to it and must be an integer in 1..historyDays.
func (st *ductingLinksState) serve(w http.ResponseWriter, r *http.Request, s *Server, src ductingStepSource, historyDays int) {
	days := historyDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > historyDays {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("days must be an integer from 1 to %d", historyDays))
			return
		}
		days = n
	}
	body, err := st.get(r, s, src, days, historyDays)
	var lim errDuctingLinksLimited
	switch {
	case errors.As(err, &lim):
		writeRateLimited(w, lim.wait)
		return
	case err != nil:
		log.Printf("[ducting] links: %v", err)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, DuctingLinksResponse{
			Status: "error", Error: "long-link analysis is unavailable", FreqMHz: ductingLinksFreq(s.cfg),
			Days: days, GeneratedAt: st.now().Unix(), Summary: ductingLinksEmptySummary(s.ductingLinksLOSMethod()), Links: []DuctingLink{},
		})
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSONBytes(w, body)
}

// get returns the encoded answer for days: the cached one (starting one
// background refresh when it is stale), or the first build for its key.
func (st *ductingLinksState) get(r *http.Request, s *Server, src ductingStepSource, days, historyDays int) ([]byte, error) {
	k, err := st.key(s, src, days)
	if err != nil {
		return nil, err
	}
	if b, fresh := st.lookup(k, st.now()); b != nil {
		if !fresh {
			st.refresh(r, s, src, k, historyDays)
		}
		return b, nil
	}
	started, err := st.claim(r, s, k)
	if err != nil {
		return nil, err
	}
	v, err, _ := st.sf.Do(k.String(), func() (interface{}, error) {
		return st.build(s, src, k, historyDays)
	})
	if started {
		st.release(k)
	}
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

func (st *ductingLinksState) release(k ductingLinksKey) {
	st.mu.Lock()
	delete(st.inflight, k.String())
	st.mu.Unlock()
}

// refresh starts one background rebuild of a stale answer, unless one is
// running or the client's budget is spent (the stale answer is served
// either way).
func (st *ductingLinksState) refresh(r *http.Request, s *Server, src ductingStepSource, k ductingLinksKey, historyDays int) {
	if started, err := st.claim(r, s, k); !started || err != nil {
		return
	}
	st.bg.Add(1)
	go func() {
		defer st.bg.Done()
		defer st.release(k)
		if _, err, _ := st.sf.Do(k.String(), func() (interface{}, error) {
			return st.build(s, src, k, historyDays)
		}); err != nil {
			log.Printf("[ducting] links: background rebuild: %v", err)
		}
	}()
}

// build computes and caches the answer for k, one build at a time.
func (st *ductingLinksState) build(s *Server, src ductingStepSource, k ductingLinksKey, historyDays int) ([]byte, error) {
	st.buildMu.Lock()
	defer st.buildMu.Unlock()
	now := st.now()
	if b, fresh := st.lookup(k, now); b != nil && fresh {
		return b, nil // built while this caller waited for buildMu
	}
	var resp *DuctingLinksResponse
	if k.rowCount == 0 {
		// Nothing stored yet: skip the store scan entirely.
		resp = &DuctingLinksResponse{
			Status: "pending", FreqMHz: ductingLinksFreq(s.cfg), Days: k.days, GeneratedAt: now.Unix(),
			Summary: ductingLinksEmptySummary(k.losMethod), Links: []DuctingLink{},
		}
	} else {
		gather := st.gather
		if gather == nil {
			gather = (*Server).ductingLinksInputFor
		}
		in, err := gather(s, k.days, now)
		if err != nil {
			return nil, err
		}
		in.Memo, in.LOS, in.HistoryDays = st.memo, st.los, historyDays
		if resp, err = buildDuctingLinks(in, src, now); err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	st.cache[k.days] = &ductingLinksCached{key: k, at: now, body: b}
	st.mu.Unlock()
	return b, nil
}

func (st *ductingLinksState) key(s *Server, src ductingStepSource, days int) (ductingLinksKey, error) {
	k := ductingLinksKey{days: days, vis: s.nodeVisibilityGen(), losMethod: s.ductingLinksLOSMethod()}
	rows, err := src.Rows()
	if err != nil {
		return k, fmt.Errorf("grid rows: %w", err)
	}
	k.rowCount = len(rows)
	for _, r := range rows {
		if r.RunTime > k.runTime {
			k.runTime = r.RunTime
		}
		if r.ValidTime > k.lastValid {
			k.lastValid = r.ValidTime
		}
	}
	return k, nil
}
