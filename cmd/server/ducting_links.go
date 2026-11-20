package main

// GET /api/ducting/links: observed long links against the stored ducting
// grid.
//
// The question it answers: when the mesh heard a node directly over more
// than the smooth-earth radio horizon, did the model show a duct along that
// path at that time, and how often are the same paths ducted at all?
//
// Evidence is certain only. A reception is an observation of an ADVERT whose
// path is empty (directHeardNode's zero-hop case: a flood with no hops, or a
// direct-route zero-hop advert), so the observer heard the advertiser's own
// transmission and the advert names it. Excluded: the observer's own
// transmissions (direction "tx"), adverts whose signature failed, blacklisted
// observers, observers that are not a known node, and either end hidden by
// the /api/nodes visibility rule or without a position fix. Positions are the
// nodes' current ones, never old advert payloads.
//
// Cost: the candidate observations are copied under the store's read lock
// (only the fields needed), then all grid work runs without it. Each stored
// step is decoded at most once per rebuild through ductingStepSource.Load
// (never the LRU behind Step), one at a time. Answers are cached per days
// for ductingLinksCacheTTL, concurrent requests share one rebuild, rebuilds
// run one at a time, and per-reception results are memoised across rebuilds
// in a bounded FIFO.

import (
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

// ductingLinkObs is one candidate reception, copied out of the store.
type ductingLinkObs struct {
	obsID int    // StoreObs.ID: the observation's identity
	t     int64  // reception time, unix seconds
	a, b  string // lowercase pubkeys: advertiser, observer node
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
}

// ductingHorizonKm is the smooth-earth radio horizon between two antennas at
// k = 4/3, heights in metres.
func ductingHorizonKm(hA, hB float64) float64 {
	return 4.12 * (math.Sqrt(math.Max(0, hA)) + math.Sqrt(math.Max(0, hB)))
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

// --- memo ---

// ductingLinksMemoKey identifies one reception's path result. Beyond the
// observation and the step's run, it holds everything else the result
// depends on, so a moved node, a re-run step or another frequency is a miss
// rather than a stale hit.
type ductingLinksMemoKey struct {
	obsID              int
	validTime, runTime int64
	latA, lonA         float64
	latB, lonB         float64
	freq               float64
}

type ductingLinksMemoVal struct{ duct, super float64 }

// ductingLinksMemo is a bounded map with first-in-first-out eviction.
type ductingLinksMemo struct {
	mu   sync.Mutex
	m    map[ductingLinksMemoKey]ductingLinksMemoVal
	ring []ductingLinksMemoKey // insertion order; slot next is the oldest once full
	next int
	max  int
}

func newDuctingLinksMemo(max int) *ductingLinksMemo {
	if max < 1 {
		max = 1
	}
	return &ductingLinksMemo{m: make(map[ductingLinksMemoKey]ductingLinksMemoVal, max), max: max}
}

func (m *ductingLinksMemo) get(k ductingLinksMemoKey) (ductingLinksMemoVal, bool) {
	if m == nil {
		return ductingLinksMemoVal{}, false
	}
	m.mu.Lock()
	v, ok := m.m[k]
	m.mu.Unlock()
	return v, ok
}

func (m *ductingLinksMemo) put(k ductingLinksMemoKey, v ductingLinksMemoVal) {
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

func (m *ductingLinksMemo) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

// --- analysis ---

type ductingLinkPair struct {
	a, b    string
	distKm  float64
	horizon float64
	recv    []int // indices into in.Recv
}

// ductingPathFracs turns path stats into the duct and super-refractive
// shares; -1 for both when no sample had data.
func ductingPathFracs(ps ducting.PathStats) (duct, super float64) {
	n := ps.Samples - ps.NoData
	if n <= 0 {
		return -1, -1
	}
	return float64(ps.Duct) / float64(n), float64(ps.Super) / float64(n)
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

// buildDuctingLinks is the analysis over copied inputs. It never touches the
// store; the grid is read through src.Load only, one step at a time.
func buildDuctingLinks(in ductingLinksInput, src ductingStepSource, now time.Time) (*DuctingLinksResponse, error) {
	resp := &DuctingLinksResponse{
		Status:      "ok",
		FreqMHz:     in.FreqMHz,
		Days:        in.Days,
		GeneratedAt: now.Unix(),
		Summary:     DuctingLinksSummary{BaselineDuctFrac: -1},
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

	// Pairs: one per (advertiser, observer node), only beyond the horizon and
	// within ductingLinksMaxKm.
	type pairKey struct{ a, b string }
	byPair := make(map[pairKey]*ductingLinkPair)
	var pairs []*ductingLinkPair
	for i := range in.Recv {
		r := &in.Recv[i]
		k := pairKey{r.a, r.b}
		p, seen := byPair[k]
		if !seen {
			ea, okA := in.Ends[r.a]
			eb, okB := in.Ends[r.b]
			if okA && okB && r.a != r.b {
				d := haversineKm(ea.Lat, ea.Lon, eb.Lat, eb.Lon)
				h := ductingHorizonKm(ea.HeightM, eb.HeightM)
				if d > h && d <= ductingLinksMaxKm {
					p = &ductingLinkPair{a: r.a, b: r.b, distKm: d, horizon: h}
					pairs = append(pairs, p)
				}
			}
			byPair[k] = p // nil remembers a rejected pair
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
	resp.Summary.Links = len(pairs)
	listed := pairs
	if len(listed) > ductingLinksMaxPairs {
		listed = listed[:ductingLinksMaxPairs]
	}

	// Per-reception step, from the memo where possible; the rest are grouped
	// by the row they need.
	ductF := make([]float64, len(in.Recv))
	superF := make([]float64, len(in.Recv))
	pairOf := make([]*ductingLinkPair, len(in.Recv))
	need := make(map[int][]int) // row index -> receptions still to compute
	for _, p := range pairs {
		for _, i := range p.recv {
			resp.Summary.Receptions++
			ductF[i], superF[i] = -1, -1
			pairOf[i] = p
			row := ductingNearestRow(rows, in.Recv[i].t)
			if row < 0 {
				continue
			}
			if v, ok := in.Memo.get(ductingLinksMemoKeyFor(in, p, &in.Recv[i], rows[row])); ok {
				ductF[i], superF[i] = v.duct, v.super
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

	combos, ducted := 0, 0
	for _, row := range steps {
		g, err := src.Load(rows[row].ValidTime)
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
			p, r := pairOf[i], &in.Recv[i]
			ea, eb := in.Ends[p.a], in.Ends[p.b]
			ductF[i], superF[i] = ductingPathFracs(g.PathStats(ea.Lat, ea.Lon, eb.Lat, eb.Lon, in.FreqMHz, ductingLinksPathKm))
			in.Memo.put(ductingLinksMemoKeyFor(in, p, r, rows[row]), ductingLinksMemoVal{ductF[i], superF[i]})
		}
		if len(listed) > 0 && isBase(row) {
			for _, p := range listed {
				ea, eb := in.Ends[p.a], in.Ends[p.b]
				d, _ := ductingPathFracs(g.PathStats(ea.Lat, ea.Lon, eb.Lat, eb.Lon, in.FreqMHz, ductingLinksPathKm))
				if d < 0 {
					continue // no data along the path at this step: not a combination
				}
				combos++
				if d >= ductingLinksDuctFrac {
					ducted++
				}
			}
		}
		// g goes out of scope here: at most one decoded step is held.
	}
	if combos > 0 {
		resp.Summary.BaselineDuctFrac = float64(ducted) / float64(combos)
	}

	for _, p := range pairs {
		for _, i := range p.recv {
			if ductF[i] < 0 {
				continue
			}
			resp.Summary.WithGrid++
			if ductF[i] >= ductingLinksDuctFrac {
				resp.Summary.DuringDuct++
			}
		}
	}

	resp.Links = make([]DuctingLink, 0, len(listed))
	for _, p := range listed {
		idx := append([]int(nil), p.recv...)
		sort.Slice(idx, func(x, y int) bool {
			rx, ry := &in.Recv[idx[x]], &in.Recv[idx[y]]
			if rx.t != ry.t {
				return rx.t > ry.t
			}
			return rx.obsID > ry.obsID
		})
		if len(idx) > ductingLinksMaxRecv {
			idx = idx[:ductingLinksMaxRecv]
		}
		recs := make([]DuctingReception, len(idx))
		for k, i := range idx {
			r := &in.Recv[i]
			recs[k] = DuctingReception{T: r.t, SNR: r.snr, RSSI: r.rssi, DuctFrac: ductF[i], SuperFrac: superF[i]}
		}
		resp.Links = append(resp.Links, DuctingLink{
			A: in.Ends[p.a], B: in.Ends[p.b],
			DistKm: p.distKm, HorizonKm: p.horizon,
			Count: len(p.recv), Receptions: recs,
		})
	}
	return resp, nil
}

func ductingLinksMemoKeyFor(in ductingLinksInput, p *ductingLinkPair, r *ductingLinkObs, row ductingGridRow) ductingLinksMemoKey {
	ea, eb := in.Ends[p.a], in.Ends[p.b]
	return ductingLinksMemoKey{
		obsID: r.obsID, validTime: row.ValidTime, runTime: row.RunTime,
		latA: ea.Lat, lonA: ea.Lon, latB: eb.Lat, lonB: eb.Lon, freq: in.FreqMHz,
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

// ductingLinkReceptions copies the certain zero-hop advert receptions in
// [from, to] whose two ends are both in ends. Holds the read lock only for
// the copy.
func (s *PacketStore) ductingLinkReceptions(ends map[string]DuctingLinkEnd, cfg *Config, from, to int64) []ductingLinkObs {
	var out []ductingLinkObs
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, tx := range s.byPayloadType[PayloadADVERT] {
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
				obsID: obs.ID, t: ts.Unix(), a: a, b: b,
				snr: copyFloatPtr(obs.SNR), rssi: copyFloatPtr(obs.RSSI),
			})
		}
	}
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
	nodes, _ := s.store.getCachedNodesAndPM()
	ends, err := s.ductingLinkEnds(nodes)
	if err != nil {
		return in, err
	}
	in.Ends = ends
	in.Recv = s.store.ductingLinkReceptions(ends, s.cfg, now.Unix()-int64(days)*86400, now.Unix())
	return in, nil
}

// --- HTTP, cache, singleflight ---

// ductingLinksKey is what a cached answer is valid for. The store
// generation is (maxObsID, totalObs): a new observation raises maxObsID and
// eviction lowers totalObs. The grid generation is the newest run_time plus
// the row count and the last valid time (a new step or a pruned one). The
// visibility generation is nodeVisibilityGen. Node positions, repeater
// heights and rewritten signature flags are not in the key; the TTL bounds
// how long they can be stale.
type ductingLinksKey struct {
	days      int
	maxObsID  int
	totalObs  int
	runTime   int64
	rowCount  int
	lastValid int64
	vis       string
}

func (k ductingLinksKey) String() string {
	return fmt.Sprintf("%d|%d|%d|%d|%d|%d|%s", k.days, k.maxObsID, k.totalObs, k.runTime, k.rowCount, k.lastValid, k.vis)
}

type ductingLinksCached struct {
	key  ductingLinksKey
	at   time.Time
	body []byte
}

// ductingLinksState caches /api/ducting/links answers (one per days value,
// so at most ductingHistoryDaysMax entries), collapses concurrent rebuilds
// of the same key, runs at most one rebuild at a time, and keeps the
// per-reception memo.
type ductingLinksState struct {
	sf      singleflight.Group
	buildMu sync.Mutex // one rebuild at a time, whatever the key

	mu    sync.Mutex
	cache map[int]*ductingLinksCached

	memo *ductingLinksMemo
	now  func() time.Time // tests replace it

	// gather collects the input; tests replace it. nil: Server.ductingLinksInputFor.
	gather func(s *Server, days int, now time.Time) (ductingLinksInput, error)
}

func newDuctingLinksState() *ductingLinksState {
	return &ductingLinksState{
		cache: make(map[int]*ductingLinksCached),
		memo:  newDuctingLinksMemo(ductingLinksMaxMemo),
		now:   time.Now,
	}
}

func (st *ductingLinksState) cached(k ductingLinksKey, now time.Time) []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	if c := st.cache[k.days]; c != nil && c.key == k && now.Sub(c.at) < ductingLinksCacheTTL*time.Second && !now.Before(c.at) {
		return c.body
	}
	return nil
}

func (st *ductingLinksState) key(s *Server, src ductingStepSource, days int) (ductingLinksKey, error) {
	k := ductingLinksKey{days: days, vis: s.nodeVisibilityGen()}
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
	if s.store != nil {
		s.store.mu.RLock()
		k.maxObsID, k.totalObs = s.store.maxObsID, s.store.totalObs
		s.store.mu.RUnlock()
	}
	return k, nil
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
	body, err := st.get(s, src, days)
	if err != nil {
		log.Printf("[ducting] links: %v", err)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, DuctingLinksResponse{
			Status: "error", Error: "long-link analysis is unavailable", FreqMHz: ductingLinksFreq(s.cfg),
			Days: days, GeneratedAt: st.now().Unix(), Summary: DuctingLinksSummary{BaselineDuctFrac: -1}, Links: []DuctingLink{},
		})
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSONBytes(w, body)
}

// get returns the encoded answer for days, from the cache or one shared
// rebuild.
func (st *ductingLinksState) get(s *Server, src ductingStepSource, days int) ([]byte, error) {
	k, err := st.key(s, src, days)
	if err != nil {
		return nil, err
	}
	if b := st.cached(k, st.now()); b != nil {
		return b, nil
	}
	v, err, _ := st.sf.Do(k.String(), func() (interface{}, error) {
		st.buildMu.Lock()
		defer st.buildMu.Unlock()
		now := st.now()
		if b := st.cached(k, now); b != nil {
			return b, nil // built while this caller waited for buildMu
		}
		var resp *DuctingLinksResponse
		if k.rowCount == 0 {
			// Nothing stored yet: skip the store scan entirely.
			resp = &DuctingLinksResponse{
				Status: "pending", FreqMHz: ductingLinksFreq(s.cfg), Days: days, GeneratedAt: now.Unix(),
				Summary: DuctingLinksSummary{BaselineDuctFrac: -1}, Links: []DuctingLink{},
			}
		} else {
			gather := st.gather
			if gather == nil {
				gather = (*Server).ductingLinksInputFor
			}
			in, err := gather(s, days, now)
			if err != nil {
				return nil, err
			}
			in.Memo = st.memo
			if resp, err = buildDuctingLinks(in, src, now); err != nil {
				return nil, err
			}
		}
		b, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		st.mu.Lock()
		st.cache[days] = &ductingLinksCached{key: k, at: now, body: b}
		st.mu.Unlock()
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}
