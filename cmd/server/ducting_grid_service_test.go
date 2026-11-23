package main

import (
	"bytes"
	"container/list"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
	ducting "github.com/meshcore-analyzer/ducting"
)

// ductingGridFixture is a read-only server DB with a writable handle on the
// same file for seeding, and a grid service over it with a settable clock
// and a counted decode.
type ductingGridFixture struct {
	db      *DB
	rw      *sql.DB
	svc     *ductingGridService
	clock   atomic.Int64 // unix seconds
	decodes atomic.Int64
}

func newDuctingGridFixture(t testing.TB) *ductingGridFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grid.db")
	rw, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rw.Close() })
	rw.SetMaxOpenConns(1)
	if _, err := rw.Exec(dbschema.SQLiteEndState); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.conn.Close() })
	f := &ductingGridFixture{db: db, rw: rw}
	f.clock.Store(1_800_000_000)
	f.svc = newDuctingGridService(db, nil)
	f.svc.now = func() time.Time { return time.Unix(f.clock.Load(), 0) }
	f.svc.decode = func(b []byte) (*ducting.Grid, error) {
		f.decodes.Add(1)
		return ducting.Decode(b)
	}
	return f
}

// advance moves the clock past the row list's TTL.
func (f *ductingGridFixture) advance() { f.clock.Add(int64(ductingGridRowsTTL/time.Second) + 1) }

// testDuctingGrid is a 41 x 41 lattice over 40..60 N, 10 W..10 E at 0.5°:
// every point normal (g -40, n -50) except one surface duct at 50 N 0 E,
// whose g is the run time / -1 so runs are told apart.
func testDuctingGrid(t testing.TB, valid, run int64) *ducting.Grid {
	t.Helper()
	g, err := ducting.NewGrid("gfs", run, valid, 60, -10, -0.5, 0.5, 41, 41)
	if err != nil {
		t.Fatal(err)
	}
	for i := range g.C {
		g.C[i], g.G[i], g.N[i] = ducting.ClassNormal, -40, -50
	}
	i := 20*41 + 20 // 50 N, 0 E
	g.C[i], g.F[i], g.B[i], g.T[i], g.DM[i], g.G[i], g.N[i] = ducting.ClassSurface, 150, 10, 120, 55, int16(-run), -80
	return g
}

func encodeDuctingGrid(t testing.TB, g *ducting.Grid) []byte {
	t.Helper()
	b, err := ducting.Encode(g)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// put writes (or replaces) the row at valid.
func (f *ductingGridFixture) put(t testing.TB, valid, run, created int64, data []byte) {
	t.Helper()
	if _, err := f.rw.Exec(`INSERT OR REPLACE INTO ducting_grid (model, valid_time, run_time, lead_hours, data, created_at)
		VALUES ('gfs', ?, ?, ?, ?, ?)`, valid, run, (valid-run)/3600, data, created); err != nil {
		t.Fatal(err)
	}
}

func (f *ductingGridFixture) putGrid(t testing.TB, valid, run, created int64) {
	t.Helper()
	f.put(t, valid, run, created, encodeDuctingGrid(t, testDuctingGrid(t, valid, run)))
}

func (f *ductingGridFixture) lruLen() int {
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if f.svc.lru.Len() != len(f.svc.entries) {
		panic("lru and index disagree")
	}
	return f.svc.lru.Len()
}

func ductingGridGet(svc *ductingGridService, query string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/ducting/grid?"+query, nil)
	r.RemoteAddr = "192.0.2.7:4000"
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	svc.serveGrid(rec, r)
	return rec
}

func ductingMetaGet(t *testing.T, svc *ductingGridService) (DuctingMetaResponse, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	svc.serveMeta(rec, httptest.NewRequest(http.MethodGet, "/api/ducting/meta", nil))
	var m DuctingMetaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("meta body %q: %v", rec.Body.String(), err)
	}
	return m, rec
}

func TestDuctingGridMeta(t *testing.T) {
	f := newDuctingGridFixture(t)
	m, rec := ductingMetaGet(t, f.svc)
	if m.Status != "pending" || len(m.Times) != 0 || m.Times == nil || m.Source != "gfs-grid" || m.Model != "gfs" ||
		m.ModelLabel != "NOAA GFS" || m.NativeStepDeg != 0.5 || m.HistoryDays != 7 || m.Attribution == "" {
		t.Fatalf("pending meta %+v", m)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Errorf("Cache-Control %q", cc)
	}
	// Inserted out of order; the newest run (200) has two rows written at
	// different times.
	f.putGrid(t, 10800, 200, 260)
	f.putGrid(t, 3600, 100, 150)
	f.putGrid(t, 7200, 200, 250)
	f.putGrid(t, 0, 100, 900) // older run written later: not the newest run's generatedAt
	if m, _ := ductingMetaGet(t, f.svc); m.Status != "pending" {
		t.Fatalf("row list reread before its TTL: %+v", m)
	}
	f.advance()
	m, _ = ductingMetaGet(t, f.svc)
	if m.Status != "ok" || m.RunTime != 200 || m.GeneratedAt != 260 {
		t.Fatalf("meta %+v", m)
	}
	if want := []int64{0, 3600, 7200, 10800}; len(m.Times) != 4 || m.Times[0] != want[0] || m.Times[1] != want[1] || m.Times[2] != want[2] || m.Times[3] != want[3] {
		t.Fatalf("times %v, want %v", m.Times, want)
	}
}

func TestDuctingGridRowsCopyAndTTL(t *testing.T) {
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	rows, err := f.svc.Rows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	rows[0].ValidTime = 42
	if again, _ := f.svc.Rows(); again[0].ValidTime != 3600 {
		t.Fatal("Rows shares its cache with the caller")
	}
	// A failed reread keeps the previous list.
	f.svc.store = failingDuctingStore{}
	f.advance()
	if rows, err := f.svc.Rows(); err != nil || len(rows) != 1 {
		t.Fatalf("after a failed reread: %v %v", rows, err)
	}
	// With no previous list the error is returned.
	g := newDuctingGridService(failingDuctingStore{}, nil)
	if _, err := g.Rows(); err == nil {
		t.Fatal("no error without a previous list")
	}
	m, _ := ductingMetaGet(t, g)
	if m.Status != "error" || m.Error == "" {
		t.Fatalf("meta on error %+v", m)
	}
	if rec := ductingGridGet(g, "t=1&bbox=40,-10,60,10"); !strings.Contains(rec.Body.String(), `"status":"error"`) ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("grid on error %d %q", rec.Code, rec.Body.String())
	}
}

type failingDuctingStore struct{}

func (failingDuctingStore) ductingGridRows(string) ([]ductingGridRow, error) {
	return nil, errors.New("db down")
}
func (failingDuctingStore) ductingGridBlob(string, int64) ([]byte, int64, error) {
	return nil, 0, errors.New("db down")
}

func TestDuctingGridParamValidation(t *testing.T) {
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	for _, q := range []string{
		"bbox=40,-10,60,10",                 // no t
		"t=&bbox=40,-10,60,10",              // empty t
		"t=1.5&bbox=40,-10,60,10",           // not an integer
		"t=abc&bbox=40,-10,60,10",           // not a number
		"t=3600",                            // no bbox
		"t=3600&bbox=40,-10,60",             // three numbers
		"t=3600&bbox=40,-10,60,10,1",        // five numbers
		"t=3600&bbox=40,x,60,10",            // not a number
		"t=3600&bbox=NaN,-10,60,10",         // NaN
		"t=3600&bbox=40,-Inf,60,10",         // infinite
		"t=3600&bbox=50,-10,50,10",          // s == n
		"t=3600&bbox=60,-10,40,10",          // s > n
		"t=3600&bbox=91,-10,95,10",          // both clamp to 90
		"t=3600&bbox=40,10,60,10",           // w == e
		"t=3600&bbox=40,10,60,-10",          // w > e
		"t=3600&bbox=40.001,0,40.004,10",    // collapses when rounded
		"t=3600&bbox=40,179.999,60,180.004", // collapses when rounded
	} {
		if rec := ductingGridGet(f.svc, q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 (%s)", q, rec.Code, rec.Body.String())
		}
	}
	if f.decodes.Load() != 0 {
		t.Errorf("a refused request decoded %d steps", f.decodes.Load())
	}
}

func TestDuctingGridBBox(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want [4]float64
	}{
		{"40,-10,60,10", [4]float64{40, -10, 60, 10}},
		{" 40 , -10 , 60 , 10 ", [4]float64{40, -10, 60, 10}},
		{"-100,-10,100,10", [4]float64{-90, -10, 90, 10}},               // latitudes clamped
		{"40,190,50,200", [4]float64{40, -170, 50, -160}},               // shifted by 360
		{"40,-200,50,-190", [4]float64{40, 160, 50, 170}},               // shifted by -360
		{"40,170,50,200", [4]float64{40, 170, 50, 180}},                 // crosses 180: cut there
		{"40,-180,50,180", [4]float64{40, -180, 50, 180}},               // whole world
		{"40,-500,50,-100", [4]float64{40, -180, 50, 180}},              // 400° wide
		{"40,900,50,910", [4]float64{40, 180 - 360, 50, 190 - 360}},     // several turns
		{"40,180,50,190", [4]float64{40, -180, 50, -170}},               // w = 180 is -180
		{"40.004,-10.006,59.996,-0.001", [4]float64{40, -10.01, 60, 0}}, // rounded to 0.01, no -0
	} {
		got, err := ductingGridBBox(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%q: %v %v, want %v", tc.in, got, err, tc.want)
		}
	}
	if b, _ := ductingGridBBox("40,-0.001,50,1"); ductingGridETag(1, 2, 0.5, b) != `"1-2-0.5-40.00,0.00,50.00,1.00"` {
		t.Errorf("etag %s", ductingGridETag(1, 2, 0.5, b))
	}
}

func TestDuctingGridSnapAndCells(t *testing.T) {
	f := newDuctingGridFixture(t)
	if rec := ductingGridGet(f.svc, "t=3600&bbox=40,-10,60,10"); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"status":"pending"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("pending: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	f.putGrid(t, 3600, 100, 111)
	f.putGrid(t, 7200, 200, 222)
	f.advance()
	for _, tc := range []struct{ t, want int64 }{
		{-5, 3600}, {3600, 3600}, {5399, 3600}, {5400, 3600} /* tie: earlier */, {5401, 7200}, {7200, 7200}, {1 << 40, 7200},
	} {
		rec := ductingGridGet(f.svc, "t="+strconv.FormatInt(tc.t, 10)+"&bbox=49,-1,51,1")
		var resp DuctingResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("t=%d: %d %v", tc.t, rec.Code, err)
		}
		if len(resp.Times) != 1 || resp.Times[0] != tc.want {
			t.Errorf("t=%d: times %v, want [%d]", tc.t, resp.Times, tc.want)
		}
	}

	rec := ductingGridGet(f.svc, "t=7200&bbox=49.2,-0.9,50.8,0.9")
	var resp DuctingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || resp.Model != "gfs" || resp.ModelLabel != "NOAA GFS" || resp.StepDeg != 0.5 ||
		resp.GeneratedAt != 222 || resp.Attribution != ductingGridAttribution || resp.Error != "" {
		t.Fatalf("header %+v", resp)
	}
	// Box snapped outward to 0.5° cells: rows 49..51, columns -1..1.
	if len(resp.Cells) != 16 {
		t.Fatalf("%d cells, want 16", len(resp.Cells))
	}
	var raw struct {
		Cells []map[string]json.RawMessage `json:"cells"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, c := range raw.Cells {
		for _, k := range []string{"c", "f", "b", "t", "dm", "g", "n"} {
			var arr []json.Number
			d := json.NewDecoder(bytes.NewReader(c[k]))
			d.UseNumber()
			if err := d.Decode(&arr); err != nil || len(arr) != 1 || strings.ContainsAny(arr[0].String(), ".eE") {
				t.Fatalf("cell %s %s: %s (%v)", c["lat"], k, c[k], err)
			}
		}
	}
	// The duct at 50 N 0 E lies on the corner of four cells; each carries it.
	ducted := 0
	for _, c := range resp.Cells {
		if c.C[0] == int(ducting.ClassSurface) {
			ducted++
			if c.F[0] != 150 || c.B[0] != 10 || c.T[0] != 120 || c.DM[0] != 55 || c.G[0] != -200 || c.N[0] != -58 /* mean of -80 and three -50, rounded */ {
				t.Errorf("ducted cell %+v", c)
			}
		} else if c.C[0] != int(ducting.ClassNormal) || c.F[0] != 0 || c.G[0] != -40 || c.N[0] != -50 {
			t.Errorf("normal cell %+v", c)
		}
	}
	if ducted != 4 {
		t.Errorf("%d ducted cells, want 4", ducted)
	}

	// The whole world: a coarser step, still within the cell cap; only the
	// lattice's area has cells.
	rec = ductingGridGet(f.svc, "t=7200&bbox=-90,-180,90,180")
	resp = DuctingResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status != "ok" {
		t.Fatalf("world: %v %q", err, rec.Body.String())
	}
	if resp.StepDeg != 8 || len(resp.Cells) == 0 || len(resp.Cells) > ductingGridMaxCells {
		t.Fatalf("world: step %v, %d cells", resp.StepDeg, len(resp.Cells))
	}
	for _, c := range resp.Cells {
		if c.Lat < 40-8 || c.Lat > 60+8 || c.Lon < -10-8 || c.Lon > 10+8 {
			t.Fatalf("world cell outside the lattice: %+v", c)
		}
	}
	// A box shifted by a turn of longitude gives the same answer.
	a := ductingGridGet(f.svc, "t=7200&bbox=49.2,-0.9,50.8,0.9")
	b := ductingGridGet(f.svc, "t=7200&bbox=49.2,359.1,50.8,360.9")
	if a.Body.String() != b.Body.String() || a.Header().Get("ETag") != b.Header().Get("ETag") {
		t.Error("a box one turn east differs")
	}
}

func TestDuctingGridETagAndCache(t *testing.T) {
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	rec := ductingGridGet(f.svc, "t=3600&bbox=40.004,-10,60,10")
	etag := rec.Header().Get("ETag")
	if rec.Code != 200 || etag != `"100-3600-0.5-40.00,-10.00,60.00,10.00"` {
		t.Fatalf("%d ETag %q", rec.Code, etag)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=600" {
		t.Errorf("Cache-Control %q", cc)
	}
	for _, inm := range []string{etag, "W/" + etag, `"x", ` + etag, "*"} {
		r := ductingGridGet(f.svc, "t=3600&bbox=40,-10,60,10", "If-None-Match", inm)
		if r.Code != http.StatusNotModified || r.Body.Len() != 0 || r.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %s: %d, %d bytes", inm, r.Code, r.Body.Len())
		}
	}
	if r := ductingGridGet(f.svc, "t=3600&bbox=40,-10,60,10", "If-None-Match", `"100-3600-0.5-other"`); r.Code != 200 {
		t.Errorf("stale ETag: %d", r.Code)
	}
	// A different box, step or run is a different ETag.
	if e := ductingGridGet(f.svc, "t=3600&bbox=40,-10,60,11").Header().Get("ETag"); e == etag {
		t.Error("other box, same ETag")
	}
	if e := ductingGridGet(f.svc, "t=3600&bbox=-90,-180,90,180").Header().Get("ETag"); !strings.HasPrefix(e, `"100-3600-8-`) {
		t.Errorf("world ETag %s", e)
	}
	f.putGrid(t, 3600, 300, 301)
	f.advance()
	if e := ductingGridGet(f.svc, "t=3600&bbox=40,-10,60,10").Header().Get("ETag"); !strings.HasPrefix(e, `"300-3600-`) {
		t.Errorf("new run ETag %s", e)
	}
}

func TestDuctingETagListMatches(t *testing.T) {
	const tag = `"1-2-0.5-40.00,-10.00,60.00,10.00"`
	for _, h := range []string{tag, "W/" + tag, " " + tag + " ", `"a", ` + tag, `"a,b",W/` + tag + `, "c"`, "*", ` * `, `junk, ` + tag} {
		if !ductingETagListMatches(h, tag) {
			t.Errorf("%q: no match", h)
		}
	}
	for _, h := range []string{"", `"1-2-0.5-40.00"`, `"-10.00,60.00,10.00"`, `"x"`, `"unterminated`, `"1-2-0.5-40.00,-10.00,60.00,10.00`, "junk", `W/`, `, ,`} {
		if ductingETagListMatches(h, tag) {
			t.Errorf("%q: matched", h)
		}
	}
	// The route's own tag may be weak (GET /api/ducting): W/ is ignored on
	// that side too.
	for _, h := range []string{tag, "W/" + tag, `"a", ` + tag} {
		if !ductingETagListMatches(h, "W/"+tag) {
			t.Errorf("%q against a weak tag: no match", h)
		}
	}
	if ductingETagListMatches(`"x"`, "W/"+tag) {
		t.Error(`"x" matched a weak tag`)
	}
}

func TestDuctingGridLRUBound(t *testing.T) {
	f := newDuctingGridFixture(t)
	const n = ductingGridLRUSteps + 4
	for i := int64(0); i < n; i++ {
		f.putGrid(t, i*3600, 0, 1)
	}
	for i := int64(0); i < n; i++ {
		g, err := f.svc.Step(i * 3600)
		if err != nil || g.ValidTime != i*3600 {
			t.Fatalf("step %d: %v", i, err)
		}
		if l := f.lruLen(); l > ductingGridLRUSteps {
			t.Fatalf("LRU holds %d steps", l)
		}
	}
	if f.decodes.Load() != n || f.lruLen() != ductingGridLRUSteps {
		t.Fatalf("%d decodes, LRU %d", f.decodes.Load(), f.lruLen())
	}
	// The newest ductingGridLRUSteps are cached.
	for i := int64(n - ductingGridLRUSteps); i < n; i++ {
		f.svc.Step(i * 3600)
	}
	if f.decodes.Load() != n {
		t.Fatalf("cached steps decoded again: %d", f.decodes.Load())
	}
	// Using step 4 keeps it; the next miss evicts step 5, the least recent.
	f.svc.Step(4 * 3600)
	f.svc.Step(0)
	f.svc.Step(4 * 3600)
	if f.decodes.Load() != n+1 {
		t.Fatalf("decodes %d, want %d", f.decodes.Load(), n+1)
	}
	f.svc.Step(5 * 3600)
	if f.decodes.Load() != n+2 {
		t.Fatalf("evicted step not decoded again: %d", f.decodes.Load())
	}
	// Load never touches the LRU.
	before := f.lruLen()
	f.svc.mu.Lock()
	front := f.svc.lru.Front().Value.(*ductingGridEntry).validTime
	f.svc.mu.Unlock()
	if g, err := f.svc.Load(1 * 3600); err != nil || g.ValidTime != 3600 {
		t.Fatalf("Load: %v", err)
	}
	f.svc.mu.Lock()
	_, has := f.svc.entries[3600]
	newFront := f.svc.lru.Front().Value.(*ductingGridEntry).validTime
	f.svc.mu.Unlock()
	if has || f.lruLen() != before || newFront != front {
		t.Error("Load changed the LRU")
	}
	if _, err := f.svc.Step(999); !errors.Is(err, errDuctingStepAbsent) {
		t.Errorf("absent step: %v", err)
	}
	if _, err := f.svc.Load(999); !errors.Is(err, errDuctingStepAbsent) {
		t.Errorf("absent Load: %v", err)
	}
	// A step removed by retention leaves the LRU on the next reread.
	if _, err := f.rw.Exec(`DELETE FROM ducting_grid WHERE valid_time = ?`, 5*3600); err != nil {
		t.Fatal(err)
	}
	f.advance()
	f.svc.Rows()
	f.svc.mu.Lock()
	_, has = f.svc.entries[5*3600]
	f.svc.mu.Unlock()
	if has {
		t.Error("deleted step still cached")
	}
}

func TestDuctingGridSingleflight(t *testing.T) {
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.svc.decode = func(b []byte) (*ducting.Grid, error) {
		f.decodes.Add(1)
		once.Do(func() { close(started) })
		<-release
		return ducting.Decode(b)
	}
	const callers = 16
	var wg sync.WaitGroup
	grids := make([]*ducting.Grid, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			grids[i], errs[i] = f.svc.Step(3600)
		}(i)
	}
	<-started
	time.Sleep(20 * time.Millisecond) // let the other callers join the flight
	close(release)
	wg.Wait()
	if f.decodes.Load() != 1 {
		t.Fatalf("%d decodes for one step, want 1", f.decodes.Load())
	}
	for i := range grids {
		if errs[i] != nil || grids[i] != grids[0] {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
	}
}

func TestDuctingGridReplacedRow(t *testing.T) {
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	if g, _ := f.svc.Step(3600); g.RunTime != 100 {
		t.Fatalf("run %d", g.RunTime)
	}
	f.putGrid(t, 3600, 200, 201)
	// Within the row list's TTL the list still names run 100: the cached
	// grid is current as far as the service knows.
	if g, _ := f.svc.Step(3600); g.RunTime != 100 || f.decodes.Load() != 1 {
		t.Fatalf("before reread: run %d, %d decodes", g.RunTime, f.decodes.Load())
	}
	f.advance()
	g, err := f.svc.Step(3600)
	if err != nil || g.RunTime != 200 || g.G[20*41+20] != -200 || f.decodes.Load() != 2 {
		t.Fatalf("after reread: %v run %d, %d decodes", err, g.RunTime, f.decodes.Load())
	}
	if f.lruLen() != 1 {
		t.Fatalf("LRU %d, want the one step", f.lruLen())
	}
	// The handler serves the new run.
	rec := ductingGridGet(f.svc, "t=3600&bbox=49,-1,51,1")
	if !strings.HasPrefix(rec.Header().Get("ETag"), `"200-3600-`) || !strings.Contains(rec.Body.String(), `"generatedAt":201`) {
		t.Fatalf("%q %s", rec.Header().Get("ETag"), rec.Body.String())
	}
	// A row list older than the blob: the newer blob is served and kept,
	// and the stale list does not evict it.
	f.putGrid(t, 3600, 300, 301)
	f.svc.mu.Lock()
	f.svc.lru.Init()
	f.svc.entries = map[int64]*list.Element{}
	f.svc.mu.Unlock()
	if g, _ := f.svc.Step(3600); g.RunTime != 300 {
		t.Fatalf("blob newer than the list: run %d", g.RunTime)
	}
	n := f.decodes.Load()
	if g, _ := f.svc.Step(3600); g.RunTime != 300 || f.decodes.Load() != n {
		t.Fatalf("newer cached run not reused: run %d", g.RunTime)
	}
}

func TestDuctingGridDecodeErrorIsolated(t *testing.T) {
	f := newDuctingGridFixture(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	f.putGrid(t, 3600, 100, 101)
	f.put(t, 7200, 100, 101, []byte("CSDG garbage"))
	// A valid blob filed under the wrong valid time.
	f.put(t, 10800, 100, 101, encodeDuctingGrid(t, testDuctingGrid(t, 3600, 100)))
	for i := 0; i < 3; i++ {
		for _, vt := range []int64{7200, 10800} {
			if _, err := f.svc.Step(vt); !errors.Is(err, errDuctingStepUnavailable) {
				t.Fatalf("step %d: %v", vt, err)
			}
			if _, err := f.svc.Load(vt); !errors.Is(err, errDuctingStepUnavailable) {
				t.Fatalf("Load %d: %v", vt, err)
			}
		}
	}
	if f.decodes.Load() != 2 {
		t.Errorf("bad steps decoded %d times, want once each", f.decodes.Load())
	}
	if c := strings.Count(buf.String(), "is unreadable"); c != 2 {
		t.Errorf("logged %d times, want once per step:\n%s", c, buf.String())
	}
	if g, err := f.svc.Step(3600); err != nil || g == nil {
		t.Fatalf("good step: %v", err)
	}
	rec := ductingGridGet(f.svc, "t=7200&bbox=40,-10,60,10")
	var resp DuctingResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp.Status != "error" || resp.Error == "" || len(resp.Cells) != 0 ||
		rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("ETag") != "" {
		t.Fatalf("bad step answer %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "garbage") || strings.Contains(rec.Body.String(), "header") {
		t.Errorf("internal detail leaked: %s", rec.Body.String())
	}
	// A replacement run of the bad step is decoded afresh.
	f.putGrid(t, 7200, 200, 201)
	f.advance()
	if g, err := f.svc.Step(7200); err != nil || g.RunTime != 200 {
		t.Fatalf("replaced bad step: %v", err)
	}
	// Bad-step memory is bounded.
	for i := 0; i < ductingGridMaxBad+5; i++ {
		f.svc.markBad(ductingGridStepKey{int64(i), 1}, errors.New("x"))
	}
	if len(f.svc.bad) > ductingGridMaxBad {
		t.Errorf("bad set %d", len(f.svc.bad))
	}
}

func TestDuctingGridLimiterChargesMissesOnly(t *testing.T) {
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	f.putGrid(t, 7200, 100, 101)
	f.put(t, 10800, 100, 101, []byte("bad"))
	f.svc.limit = &clientLimiter{ipr: &wsLimiter{}, rl: newRateLimiter(1, time.Hour), tag: "t", what: "w"}
	if rec := ductingGridGet(f.svc, "t=3600&bbox=40,-10,60,10"); rec.Code != 200 {
		t.Fatalf("first miss: %d", rec.Code)
	}
	for i := 0; i < 5; i++ {
		if rec := ductingGridGet(f.svc, "t=3600&bbox=41,-10,60,10"); rec.Code != 200 {
			t.Fatalf("cached step charged: %d", rec.Code)
		}
	}
	rec := ductingGridGet(f.svc, "t=7200&bbox=40,-10,60,10")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second miss: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if f.decodes.Load() != 1 {
		t.Fatalf("a refused request decoded: %d", f.decodes.Load())
	}
	// A step already known to be unreadable is not charged either.
	f.svc.Load(10800)
	if rec := ductingGridGet(f.svc, "t=10800&bbox=40,-10,60,10"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("bad step: %d %s", rec.Code, rec.Body.String())
	}
	// Another client has its own budget.
	r := httptest.NewRequest(http.MethodGet, "/api/ducting/grid?t=7200&bbox=40,-10,60,10", nil)
	r.RemoteAddr = "198.51.100.9:1"
	other := httptest.NewRecorder()
	f.svc.serveGrid(other, r)
	if other.Code != 200 {
		t.Fatalf("other client: %d", other.Code)
	}
}

func TestDuctingGridRoutes(t *testing.T) {
	s, router := setupTestServer(t)
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	paths := []string{"/api/ducting/meta", "/api/ducting/grid?t=3600&bbox=40,-10,60,10"}
	for _, p := range paths {
		if rec := get(p); rec.Code != http.StatusNotFound {
			t.Errorf("disabled %s: %d", p, rec.Code)
		}
	}
	s.cfg.Ducting = &DuctingConfig{Enabled: true}
	for _, p := range paths {
		if rec := get(p); rec.Code != http.StatusNotFound {
			t.Errorf("no service %s: %d", p, rec.Code)
		}
	}
	s.ductingGrid = f.svc
	s.ducting = newDuctingService(&DuctingConfig{Enabled: true, Source: "open-meteo"}, nil)
	for _, p := range paths {
		rec := get(p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
			t.Errorf("gfs-grid %s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	// The handler's Cache-Control and ETag win over the /api/ no-store
	// middleware, and 304 works through the router.
	rec := get(paths[1])
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=600" {
		t.Errorf("grid Cache-Control through the router %q", cc)
	}
	req := httptest.NewRequest(http.MethodGet, paths[1], nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec304 := httptest.NewRecorder()
	router.ServeHTTP(rec304, req)
	if rec304.Code != http.StatusNotModified {
		t.Errorf("304 through the router: %d", rec304.Code)
	}
	if cc := get(paths[0]).Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Errorf("meta Cache-Control through the router %q", cc)
	}
	// /api/ducting is the Open-Meteo snapshot: 404 under gfs-grid even with
	// a service present.
	if rec := get("/api/ducting"); rec.Code != http.StatusNotFound {
		t.Errorf("/api/ducting under gfs-grid: %d", rec.Code)
	}
	s.cfg.Ducting.Source = "open-meteo"
	for _, p := range paths {
		if rec := get(p); rec.Code != http.StatusNotFound {
			t.Errorf("open-meteo %s: %d", p, rec.Code)
		}
	}
	if rec := get("/api/ducting"); rec.Code != http.StatusOK {
		t.Errorf("/api/ducting under open-meteo: %d", rec.Code)
	}
	s.cfg.Ducting = &DuctingConfig{Enabled: false}
	for _, p := range append(paths, "/api/ducting") {
		if rec := get(p); rec.Code != http.StatusNotFound {
			t.Errorf("disabled again %s: %d", p, rec.Code)
		}
	}
}

func TestDuctingGridServiceFromConfig(t *testing.T) {
	cfg := &Config{Ducting: &DuctingConfig{Enabled: true, HistoryDays: 14}}
	svc := newDuctingGridService(failingDuctingStore{}, cfg)
	if svc.historyDays != 14 || svc.limit == nil || svc.limit.off() || svc.model != "gfs" {
		t.Fatalf("%+v", svc)
	}
	if m, _ := ductingMetaGet(t, newDuctingGridService(failingDuctingStore{}, &Config{})); m.HistoryDays != 7 {
		t.Fatalf("default history %d", m.HistoryDays)
	}
}

// BenchmarkDuctingGridHandler serves a cached step of a worldwide 0.5°
// lattice (the GFS shape) for a CONUS window and the whole world.
func BenchmarkDuctingGridHandler(b *testing.B) {
	f := newDuctingGridFixture(b)
	g, err := ducting.NewGrid("gfs", 100, 3600, 90, 0, -0.5, 0.5, 720, 361)
	if err != nil {
		b.Fatal(err)
	}
	for i := range g.C {
		g.C[i] = ducting.ClassNormal + uint8(i%4)
		g.G[i], g.N[i] = int16(-40-i%200), int16(-30-i%100)
		if g.C[i] >= ducting.ClassElev {
			g.F[i], g.B[i], g.T[i], g.DM[i] = uint16(100+i%900), uint16(i%300), uint16(300+i%500), uint16(i%400)
		}
	}
	f.put(b, 3600, 100, 101, encodeDuctingGrid(b, g))
	if _, err := f.svc.Step(3600); err != nil {
		b.Fatal(err)
	}
	for _, bc := range []struct{ name, bbox string }{
		{"conus", "24,-125,50,-66"},
		{"world", "-90,-180,90,180"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			rec := ductingGridGet(f.svc, "t=3600&bbox="+bc.bbox)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
				b.Fatalf("%d", rec.Code)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ductingGridGet(f.svc, "t=3600&bbox="+bc.bbox)
			}
			b.ReportMetric(float64(rec.Body.Len()), "body-bytes")
		})
		// A revalidation (If-None-Match) skips the body encode entirely.
		b.Run(bc.name+"-304", func(b *testing.B) {
			etag := ductingGridGet(f.svc, "t=3600&bbox="+bc.bbox).Header().Get("ETag")
			if rec := ductingGridGet(f.svc, "t=3600&bbox="+bc.bbox, "If-None-Match", etag); rec.Code != 304 {
				b.Fatalf("%d", rec.Code)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ductingGridGet(f.svc, "t=3600&bbox="+bc.bbox, "If-None-Match", etag)
			}
		})
	}
	if f.decodes.Load() != 1 {
		b.Fatalf("benchmark decoded %d times", f.decodes.Load())
	}
}

// /api/ducting/links is registered on the router: 404 while ducting is off,
// without the grid service or without the links state; with both, the
// analysis answers (no packets here, so no links, but status ok).
func TestDuctingLinksRoute(t *testing.T) {
	s, router := setupTestServer(t)
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 100, 101)
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ducting/links?days=1", nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: %d", rec.Code)
	}
	s.cfg.Ducting = &DuctingConfig{Enabled: true}
	s.ductingGrid = f.svc
	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("no links state: %d", rec.Code)
	}
	s.ductingLinks = newDuctingLinksState()
	rec := get()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"links":[]`) {
		t.Fatalf("enabled: %d %s", rec.Code, rec.Body.String())
	}
	s.cfg.Ducting = &DuctingConfig{Enabled: true, Source: "open-meteo"}
	if rec := get(); rec.Code != http.StatusNotFound {
		t.Fatalf("open-meteo source: %d", rec.Code)
	}
}
