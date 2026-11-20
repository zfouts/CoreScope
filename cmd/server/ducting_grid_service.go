package main

// ductingGridService serves the stored global ducting grid (ducting.source
// "gfs-grid"; contract in ducting_grid_types.go): GET /api/ducting/meta and
// GET /api/ducting/grid, and the ductingStepSource the long-link analysis
// reads. Read-only: rows come from ducting_grid, which only the ingestor
// writes.
//
// Memory: at most ductingGridLRUSteps decoded steps in the LRU (~3.4 MB each
// at 0.5°), plus at most ductingGridDecodeSlots being decoded at once
// (requests for the LRU and Load's bulk scans alike).

import (
	"container/list"
	"database/sql"
	"encoding/json"
	"errors"
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

const (
	// ductingGridRowsTTL is how long the row list is reused before the
	// table is read again.
	ductingGridRowsTTL = time.Minute
	// ductingGridDecodeSlots bounds concurrent decodes (CPU, and transient
	// memory beyond the LRU).
	ductingGridDecodeSlots = 2
	// ductingGridDecodesPerMin is one client's budget of /api/ducting/grid
	// requests that must decode a step (LRU misses): a burst of this many,
	// refilled at this rate. Scrubbing the whole time slider of a week
	// (~60 steps) fits in one burst; cached steps cost nothing.
	ductingGridDecodesPerMin = 60
	// ductingGridMaxBad bounds the remembered undecodable (valid, run)
	// steps; the set is cleared when it would grow past this.
	ductingGridMaxBad = 256
	// ductingGridMetaMaxAgeS is the Cache-Control max-age of /api/ducting/meta.
	ductingGridMetaMaxAgeS = 60
)

var (
	// errDuctingStepAbsent: no row is stored at that valid time.
	errDuctingStepAbsent = errors.New("ducting: no stored grid step at that time")
	// errDuctingStepUnavailable: the stored row does not decode.
	errDuctingStepUnavailable = errors.New("ducting: stored grid step is unreadable")
)

// ductingGridStore is the read path the service needs (*DB implements it).
type ductingGridStore interface {
	ductingGridRows(model string) ([]ductingGridRow, error)
	ductingGridBlob(model string, validTime int64) ([]byte, int64, error)
}

// ductingGridEntry is one decoded step in the LRU.
type ductingGridEntry struct {
	validTime, runTime int64
	grid               *ducting.Grid
}

// ductingGridStepKey names one stored version of a step.
type ductingGridStepKey struct{ validTime, runTime int64 }

type ductingGridService struct {
	store       ductingGridStore
	model       string
	historyDays int
	decode      func([]byte) (*ducting.Grid, error) // ducting.Decode; tests count calls
	now         func() time.Time
	limit       *clientLimiter // charged per request that must decode
	slots       chan struct{}  // ductingGridDecodeSlots
	sf          singleflight.Group

	rowsMu sync.Mutex
	rows   []ductingGridRow // shared, never mutated after it is stored
	rowsAt time.Time
	rowsOK bool

	mu      sync.Mutex
	lru     *list.List // of *ductingGridEntry, most recent first
	entries map[int64]*list.Element
	bad     map[ductingGridStepKey]struct{}
}

var _ ductingStepSource = (*ductingGridService)(nil)

// newDuctingGridService builds the service over store. cfg supplies the
// history length and the trusted proxies the per-client limit keys on; nil
// cfg (tests) means defaults and no per-client limit.
func newDuctingGridService(store ductingGridStore, cfg *Config) *ductingGridService {
	s := &ductingGridService{
		store:       store,
		model:       ductingGridModel,
		historyDays: defaultDuctingHistoryDays,
		decode:      ducting.Decode,
		now:         time.Now,
		slots:       make(chan struct{}, ductingGridDecodeSlots),
		lru:         list.New(),
		entries:     map[int64]*list.Element{},
		bad:         map[ductingGridStepKey]struct{}{},
	}
	if cfg != nil {
		s.historyDays = cfg.Ducting.historyDays()
		s.limit = &clientLimiter{
			ipr: &wsLimiter{state: map[string]*wsIPState{},
				trustedProxies: parseCIDRList(cfg.WSTrustedProxies(), "webSocket.trustedProxies")},
			rl:  newRateLimiter(ductingGridDecodesPerMin, time.Minute),
			tag: "ducting", what: "ducting grid decode",
		}
	}
	return s
}

// Rows returns a copy of the stored rows, ascending by valid time.
func (s *ductingGridService) Rows() ([]ductingGridRow, error) {
	rows, err := s.sharedRows()
	if err != nil {
		return nil, err
	}
	out := make([]ductingGridRow, len(rows))
	copy(out, rows)
	return out, nil
}

// sharedRows returns the cached row list itself; callers must not modify
// it. It is re-read at most once per ductingGridRowsTTL, by one caller
// (singleflight). A failed re-read keeps serving the previous list (logged)
// until the next TTL; with no previous list the error is returned.
func (s *ductingGridService) sharedRows() ([]ductingGridRow, error) {
	s.rowsMu.Lock()
	if s.rowsOK && s.now().Sub(s.rowsAt) < ductingGridRowsTTL {
		rows := s.rows
		s.rowsMu.Unlock()
		return rows, nil
	}
	s.rowsMu.Unlock()
	v, err, _ := s.sf.Do("rows", func() (interface{}, error) {
		s.rowsMu.Lock()
		if s.rowsOK && s.now().Sub(s.rowsAt) < ductingGridRowsTTL {
			rows := s.rows
			s.rowsMu.Unlock()
			return rows, nil
		}
		s.rowsMu.Unlock()
		rows, err := s.store.ductingGridRows(s.model)
		s.rowsMu.Lock()
		defer s.rowsMu.Unlock()
		if err != nil {
			if !s.rowsOK {
				return nil, err
			}
			log.Printf("[ducting] grid rows: %v (serving the previous list)", err)
			s.rowsAt = s.now()
			return s.rows, nil
		}
		s.rows, s.rowsAt, s.rowsOK = rows, s.now(), true
		s.pruneMissing(rows)
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]ductingGridRow), nil
}

// pruneMissing drops LRU entries whose valid time is no longer stored
// (retention), so they stop holding memory.
func (s *ductingGridService) pruneMissing(rows []ductingGridRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for vt, e := range s.entries {
		if ductingGridRowIndex(rows, vt) < 0 {
			s.lru.Remove(e)
			delete(s.entries, vt)
		}
	}
}

// ductingGridRowIndex is the index of the row at exactly vt, or -1.
func ductingGridRowIndex(rows []ductingGridRow, vt int64) int {
	i := sort.Search(len(rows), func(i int) bool { return rows[i].ValidTime >= vt })
	if i < len(rows) && rows[i].ValidTime == vt {
		return i
	}
	return -1
}

// ductingGridNearest is the index of the row nearest t (the earlier on a
// tie), or -1 when rows is empty. rows ascend by ValidTime.
func ductingGridNearest(rows []ductingGridRow, t int64) int {
	i := sort.Search(len(rows), func(i int) bool { return rows[i].ValidTime >= t })
	switch {
	case len(rows) == 0:
		return -1
	case i == 0:
		return 0
	case i == len(rows):
		return i - 1
	case t-rows[i-1].ValidTime <= rows[i].ValidTime-t:
		return i - 1
	default:
		return i
	}
}

// Step returns the decoded grid stored at exactly validTime through the
// LRU. A cached entry is served only when its run is at least the run the
// row list names, so a row replaced by a newer run is decoded afresh.
func (s *ductingGridService) Step(validTime int64) (*ducting.Grid, error) {
	rows, err := s.sharedRows()
	if err != nil {
		return nil, err
	}
	i := ductingGridRowIndex(rows, validTime)
	if i < 0 {
		return nil, errDuctingStepAbsent
	}
	return s.step(rows[i])
}

func (s *ductingGridService) step(row ductingGridRow) (*ducting.Grid, error) {
	if g := s.cached(row); g != nil {
		return g, nil
	}
	if s.isBad(ductingGridStepKey{row.ValidTime, row.RunTime}) {
		return nil, errDuctingStepUnavailable
	}
	key := strconv.FormatInt(row.ValidTime, 10) + ":" + strconv.FormatInt(row.RunTime, 10)
	v, err, _ := s.sf.Do(key, func() (interface{}, error) {
		if g := s.cached(row); g != nil {
			return g, nil // filled while this caller queued
		}
		g, err := s.Load(row.ValidTime)
		if err != nil {
			return nil, err
		}
		s.put(g)
		return g, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*ducting.Grid), nil
}

// cached returns the LRU's grid for row (marking it recently used), or nil.
func (s *ductingGridService) cached(row ductingGridRow) *ducting.Grid {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[row.ValidTime]
	if !ok {
		return nil
	}
	ent := e.Value.(*ductingGridEntry)
	if ent.runTime < row.RunTime {
		return nil // replaced by a newer run; put() swaps it
	}
	s.lru.MoveToFront(e)
	return ent.grid
}

// put stores g in the LRU unless a newer run of the step is already there,
// evicting the least recently used beyond ductingGridLRUSteps.
func (s *ductingGridService) put(g *ducting.Grid) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[g.ValidTime]; ok {
		ent := e.Value.(*ductingGridEntry)
		if ent.runTime > g.RunTime {
			return
		}
		ent.runTime, ent.grid = g.RunTime, g
		s.lru.MoveToFront(e)
		return
	}
	s.entries[g.ValidTime] = s.lru.PushFront(&ductingGridEntry{validTime: g.ValidTime, runTime: g.RunTime, grid: g})
	for s.lru.Len() > ductingGridLRUSteps {
		old := s.lru.Back()
		s.lru.Remove(old)
		delete(s.entries, old.Value.(*ductingGridEntry).validTime)
	}
}

// Load reads and decodes the step stored at validTime without touching the
// LRU. A blob that does not decode, or whose header does not match its row,
// is logged once per (valid time, run) and reported as
// errDuctingStepUnavailable from then on without decoding it again.
func (s *ductingGridService) Load(validTime int64) (*ducting.Grid, error) {
	return s.load(validTime, s.decode)
}

// LoadPlanes is Load for scans that only sample the C and F planes
// (PathStats): the grid comes from d, carries only those planes, and is valid
// until d's next use. Same errors, bad-step memory and decode slots as Load.
func (s *ductingGridService) LoadPlanes(validTime int64, d *ducting.Decoder) (*ducting.Grid, error) {
	return s.load(validTime, d.DecodePlanes)
}

func (s *ductingGridService) load(validTime int64, decode func([]byte) (*ducting.Grid, error)) (*ducting.Grid, error) {
	data, run, err := s.store.ductingGridBlob(s.model, validTime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errDuctingStepAbsent
	}
	if err != nil {
		return nil, err
	}
	key := ductingGridStepKey{validTime, run}
	if s.isBad(key) {
		return nil, errDuctingStepUnavailable
	}
	s.slots <- struct{}{}
	g, err := decode(data)
	<-s.slots
	if err == nil && (g == nil || g.ValidTime != validTime || g.RunTime != run || g.Model != s.model) {
		err = errors.New("grid header does not match its row")
	}
	if err != nil {
		s.markBad(key, err)
		return nil, errDuctingStepUnavailable
	}
	return g, nil
}

func (s *ductingGridService) isBad(k ductingGridStepKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.bad[k]
	return ok
}

func (s *ductingGridService) markBad(k ductingGridStepKey, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bad[k]; ok {
		return
	}
	if len(s.bad) >= ductingGridMaxBad {
		s.bad = map[ductingGridStepKey]struct{}{}
	}
	s.bad[k] = struct{}{}
	log.Printf("[ducting] grid step valid=%d run=%d is unreadable, skipped: %v", k.validTime, k.runTime, err)
}

// --- HTTP ---

func (s *ductingGridService) metaBase(status string) DuctingMetaResponse {
	return DuctingMetaResponse{
		Status: status, Source: ductingSourceGrid, Model: s.model,
		ModelLabel: ductingGridModelLabel, Attribution: ductingGridAttribution,
		NativeStepDeg: ductingGridNativeStepDeg, HistoryDays: s.historyDays,
		Times: []int64{},
	}
}

// serveMeta is GET /api/ducting/meta: every stored valid time, the newest
// run and when it was written. "error" is no-store; otherwise cacheable
// for ductingGridMetaMaxAgeS.
func (s *ductingGridService) serveMeta(w http.ResponseWriter, r *http.Request) {
	rows, err := s.sharedRows()
	if err != nil {
		log.Printf("[ducting] meta: %v", err)
		resp := s.metaBase("error")
		resp.Error = "ducting grid unavailable"
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, resp)
		return
	}
	resp := s.metaBase("pending")
	if len(rows) > 0 {
		resp.Status = "ok"
		resp.Times = make([]int64, len(rows))
		for i, row := range rows {
			resp.Times[i] = row.ValidTime
			resp.RunTime = max(resp.RunTime, row.RunTime)
		}
		for _, row := range rows {
			if row.RunTime == resp.RunTime {
				resp.GeneratedAt = max(resp.GeneratedAt, row.CreatedAt)
			}
		}
	}
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(ductingGridMetaMaxAgeS))
	writeJSON(w, resp)
}

// ductingGridBBox parses bbox=s,w,n,e into the box Window takes:
// latitudes clamped to [-90, 90] with s < n; a box of 360° or more of
// longitude becomes [-180, 180], otherwise w is shifted into [-180, 180)
// and e by the same multiple of 360, then e is clipped to 180. So a box
// that crosses the antimeridian shows only its western part (the browser
// asks for the rest separately if it wants it). Every edge is then rounded
// to 0.01°, so the ETag (which names the rounded box) identifies the
// answer exactly; a box that collapses under rounding is refused.
func ductingGridBBox(v string) (box [4]float64, err error) {
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		return box, errors.New("bbox must be s,w,n,e")
	}
	for i, p := range parts {
		f, perr := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if perr != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return box, errors.New("bbox must be four finite numbers s,w,n,e")
		}
		box[i] = f
	}
	s, w, n, e := box[0], box[1], box[2], box[3]
	s, n = math.Max(-90, math.Min(90, s)), math.Max(-90, math.Min(90, n))
	if !(s < n) {
		return box, errors.New("bbox needs south < north within [-90, 90]")
	}
	if !(w < e) {
		return box, errors.New("bbox needs west < east")
	}
	if e-w >= 360 {
		w, e = -180, 180
	} else {
		shift := 360 * math.Floor((w+180)/360)
		w, e = w-shift, math.Min(e-shift, 180)
	}
	r := func(x float64) float64 { return math.Round(x*100)/100 + 0 } // + 0 turns -0 into 0
	box = [4]float64{r(s), r(w), r(n), r(e)}
	if !(box[0] < box[2]) || !(box[1] < box[3]) {
		return box, errors.New("bbox is too small")
	}
	return box, nil
}

// ductingGridETag is `"<run>-<valid>-<step>-<s,w,n,e>"`.
func ductingGridETag(run, valid int64, step float64, box [4]float64) string {
	var b strings.Builder
	b.WriteByte('"')
	b.WriteString(strconv.FormatInt(run, 10))
	b.WriteByte('-')
	b.WriteString(strconv.FormatInt(valid, 10))
	b.WriteByte('-')
	b.WriteString(strconv.FormatFloat(step, 'g', -1, 64))
	b.WriteByte('-')
	for i, v := range box {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(v, 'f', 2, 64))
	}
	b.WriteByte('"')
	return b.String()
}

// ductingETagListMatches reports whether an If-None-Match header names etag:
// "*", or a list of entity-tags compared weakly (W/ ignored), RFC 9110
// 13.1.2. It parses the quoted tags rather than splitting on ",", because
// the grid's tags contain commas (the bbox). Used by every /api/ducting*
// route.
func ductingETagListMatches(header, etag string) bool {
	h := strings.TrimSpace(header)
	if h == "*" {
		return true
	}
	for {
		h = strings.TrimLeft(h, " \t,")
		if h == "" {
			return false
		}
		h = strings.TrimPrefix(h, "W/")
		if !strings.HasPrefix(h, `"`) {
			// Not an entity-tag: skip to the next list element.
			i := strings.IndexByte(h, ',')
			if i < 0 {
				return false
			}
			h = h[i+1:]
			continue
		}
		end := strings.IndexByte(h[1:], '"')
		if end < 0 {
			return false
		}
		if h[:end+2] == etag {
			return true
		}
		h = h[end+2:]
	}
}

func (s *ductingGridService) gridBase(status string) DuctingResponse {
	return DuctingResponse{
		Status: status, Model: s.model, ModelLabel: ductingGridModelLabel,
		Attribution: ductingGridAttribution, StepDeg: ductingGridNativeStepDeg,
		Times: []int64{}, Cells: []DuctingCell{},
	}
}

// ductingGridCells converts window cells to the /api/ducting cell format
// with one time: every array has length 1. One backing array holds all of
// them (7 ints per cell) instead of 7 allocations per cell.
func ductingGridCells(wc []ducting.WindowCell) []DuctingCell {
	out := make([]DuctingCell, len(wc))
	vals := make([]int, 7*len(wc))
	for i, c := range wc {
		v := vals[7*i : 7*i+7 : 7*i+7]
		v[0], v[1], v[2], v[3], v[4], v[5], v[6] = int(c.C), int(c.F), int(c.B), int(c.T), int(c.DM), int(c.G), int(c.N)
		out[i] = DuctingCell{
			Lat: c.Lat, Lon: c.Lon,
			C: v[0:1:1], F: v[1:2:2], B: v[2:3:3], T: v[3:4:4], DM: v[4:5:5], G: v[5:6:6], N: v[6:7:7],
		}
	}
	return out
}

// serveGrid is GET /api/ducting/grid?t=<unix>&bbox=s,w,n,e: the stored
// step nearest t, cut to the box at most ductingGridMaxCells cells, in the
// /api/ducting format with one time. 400 on bad parameters; "pending"
// (no-store) while nothing is stored; 429 when the client's decode budget
// is spent and the step is not in the LRU; ok answers carry an ETag and
// may be cached for ductingGridMaxAgeS (a stored step only changes with a
// new run, which changes the ETag).
func (s *ductingGridService) serveGrid(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t, err := strconv.ParseInt(strings.TrimSpace(q.Get("t")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "t must be an integer (unix seconds)")
		return
	}
	box, err := ductingGridBBox(q.Get("bbox"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.sharedRows()
	if err != nil {
		log.Printf("[ducting] grid: %v", err)
		s.writeGridError(w)
		return
	}
	i := ductingGridNearest(rows, t)
	if i < 0 {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, s.gridBase("pending"))
		return
	}
	row := rows[i]
	if s.cached(row) == nil && !s.isBad(ductingGridStepKey{row.ValidTime, row.RunTime}) {
		if ok, wait := s.limit.take(r); !ok {
			writeRateLimited(w, wait)
			return
		}
	}
	g, err := s.step(row)
	if err != nil {
		if !errors.Is(err, errDuctingStepUnavailable) {
			log.Printf("[ducting] grid step %d: %v", row.ValidTime, err)
		}
		s.writeGridError(w)
		return
	}
	win := g.Window(box[0], box[1], box[2], box[3], ductingGridMaxCells)
	etag := ductingGridETag(g.RunTime, g.ValidTime, win.Step, box)
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(ductingGridMaxAgeS))
	w.Header().Set("ETag", etag)
	if ductingETagListMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	resp := s.gridBase("ok")
	resp.GeneratedAt = row.CreatedAt
	resp.StepDeg = win.Step
	resp.Times = []int64{g.ValidTime}
	resp.Cells = ductingGridCells(win.Cells)
	body, err := json.Marshal(resp)
	if err != nil {
		log.Printf("[ducting] grid encode: %v", err)
		s.writeGridError(w)
		return
	}
	writeJSONBytes(w, body)
}

func (s *ductingGridService) writeGridError(w http.ResponseWriter) {
	resp := s.gridBase("error")
	resp.Error = "ducting grid unavailable"
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, resp)
}

// --- routes ---

// ductingGridFor is the grid service when ducting is enabled with source
// gfs-grid, else nil.
func (s *Server) ductingGridFor() *ductingGridService {
	if s.cfg == nil || !s.cfg.Ducting.enabled() || s.cfg.Ducting.source() != ductingSourceGrid {
		return nil
	}
	return s.ductingGrid
}

// handleDuctingMeta is GET /api/ducting/meta; 404 unless ducting is enabled
// with source gfs-grid.
func (s *Server) handleDuctingMeta(w http.ResponseWriter, r *http.Request) {
	g := s.ductingGridFor()
	if g == nil {
		writeError(w, http.StatusNotFound, "ducting disabled")
		return
	}
	g.serveMeta(w, r)
}

// handleDuctingLinks is GET /api/ducting/links (ducting_links.go); 404
// unless ducting is enabled with source gfs-grid.
func (s *Server) handleDuctingLinks(w http.ResponseWriter, r *http.Request) {
	g := s.ductingGridFor()
	if g == nil || s.ductingLinks == nil {
		writeError(w, http.StatusNotFound, "ducting disabled")
		return
	}
	s.ductingLinks.serve(w, r, s, g, s.cfg.Ducting.historyDays())
}

// handleDuctingGrid is GET /api/ducting/grid; 404 unless ducting is enabled
// with source gfs-grid.
func (s *Server) handleDuctingGrid(w http.ResponseWriter, r *http.Request) {
	g := s.ductingGridFor()
	if g == nil {
		writeError(w, http.StatusNotFound, "ducting disabled")
		return
	}
	g.serveGrid(w, r)
}

// handleDuctingOpenMeteo is GET /api/ducting: the Open-Meteo snapshot
// (handleDucting), 404 under source gfs-grid, where the browser reads
// /api/ducting/meta and /api/ducting/grid instead.
func (s *Server) handleDuctingOpenMeteo(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil || s.cfg.Ducting.source() != ductingSourceOpenMeteo {
		writeError(w, http.StatusNotFound, "ducting disabled")
		return
	}
	s.handleDucting(w, r)
}
