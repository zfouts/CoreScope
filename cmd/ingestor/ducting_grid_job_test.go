package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcore-analyzer/ducting"
	"github.com/meshcore-analyzer/grib2"
	"github.com/meshcore-analyzer/grib2/grib2test"
)

// Global ducting grid job against a fake NOAA bucket built from grib2test
// messages on a 12 x 7 lattice (30° spacing).

var testGridLattice = ductingGridLattice{Ni: 12, Nj: 7, La1: 90, Lo1: 0, DLat: -30, DLon: 30}

// testGridRun is the cycle the tests publish: 2026-11-20 06Z.
var testGridRun = time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC).Unix()

// testGridDuctPoint is the one lattice point with a strong surface inversion.
const testGridDuctPoint = 2*12 + 3

// stdHeight is the standard-atmosphere height (m) of a pressure level.
func stdHeight(pHPa float64) float64 { return 44330.8 * (1 - math.Pow(pHPa/1013.25, 0.190263)) }

// testGridValue is the default field: a standard atmosphere at RH 50 %
// (normal refraction) everywhere but testGridDuctPoint, which has a humid
// surface under warm, dry air (a surface duct).
func testGridValue(slot, idx int) float64 {
	duct := idx == testGridDuctPoint
	switch slot {
	case slotPresSfc:
		return 101325
	case slotHgtSfc:
		return 0
	case slotTmp2m:
		if duct {
			return 290
		}
		return 288.15
	case slotRh2m:
		if duct {
			return 95
		}
		return 50
	}
	i := (slot - slotLevel0) / 3
	z := stdHeight(ductingGridLevelsHPa[i])
	switch (slot - slotLevel0) % 3 {
	case 0: // TMP
		if duct {
			return 300 - 0.0065*z
		}
		return 288.15 - 0.0065*z
	case 1: // RH
		if duct {
			return 5
		}
		return 50
	}
	return z // HGT
}

// fakeGridStep describes one published step; the hooks corrupt it.
type fakeGridStep struct {
	run   int64
	step  int
	value func(slot, idx int) float64
	// field adjusts the message of a slot before encoding.
	field func(slot int, f *grib2test.Field)
	// idxLine adjusts or drops (return "") one .idx line of a needed slot.
	idxLine func(slot int, line string) string
}

type fakeNOAA struct {
	mu       sync.Mutex
	files    map[string][]byte
	requests map[string]int
	// hook may answer a request itself (return true).
	hook func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeNOAA(t *testing.T) (*fakeNOAA, *httptest.Server) {
	f := &fakeNOAA{files: map[string][]byte{}, requests: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeNOAA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests[r.URL.Path]++
	data, ok := f.files[r.URL.Path]
	hook := f.hook
	f.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func (f *fakeNOAA) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

// totalCount sums requests whose path contains substr.
func (f *fakeNOAA) totalCount(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, c := range f.requests {
		if strings.Contains(p, substr) {
			n += c
		}
	}
	return n
}

// gridPath is the bucket path of a file, as ductingGridJob.fileURL builds it.
func gridPath(run int64, step, file int, idx bool) string {
	j := &ductingGridJob{}
	return j.fileURL(run, step, file, idx)
}

func testGridMessage(run int64, step int, cat, num, surfType uint8, surfValue float64, val func(idx int) float64) grib2test.Field {
	l := testGridLattice
	vals := make([]float64, l.Ni*l.Nj)
	for i := range vals {
		vals[i] = val(i)
	}
	return grib2test.Field{
		Ni: l.Ni, Nj: l.Nj, La1: l.La1, Lo1: l.Lo1, DLat: l.DLat, DLon: l.DLon,
		Ref: time.Unix(run, 0), ForecastHour: step,
		Category: cat, Number: num, SurfaceType: surfType, SurfaceValue: surfValue, Values: vals,
	}
}

// publish adds both files of a step and their .idx. Each file starts with
// an unneeded UGRD message; pgrb2 also ends with one, pgrb2b ends with a
// needed message (open-ended range).
func (f *fakeNOAA) publish(s fakeGridStep) {
	if s.value == nil {
		s.value = testGridValue
	}
	stamp := cycleStamp(s.run)
	fcst := forecastLabel(s.step)
	for file := range gfsFileKinds {
		var body []byte
		var lines []string
		add := func(name, level string, msg []byte) string {
			line := fmt.Sprintf("%d:%d:d=%s:%s:%s:%s:", len(lines)+1, len(body), stamp, name, level, fcst)
			body = append(body, msg...)
			return line
		}
		ugrd := grib2test.Message(testGridMessage(s.run, s.step, 2, 2, gribSurfaceAboveGnd, 10, func(int) float64 { return 1 }))
		lines = append(lines, add("UGRD", "10 m above ground", ugrd))
		for slot, v := range ductingGridVars {
			if v.File != file {
				continue
			}
			sv := v.SurfValue
			if v.SurfType == gribSurfaceGround {
				sv = math.NaN()
			}
			fld := testGridMessage(s.run, s.step, v.Category, v.Number, v.SurfType, sv, func(idx int) float64 { return s.value(slot, idx) })
			if s.field != nil {
				s.field(slot, &fld)
			}
			line := add(v.Name, v.Level, grib2test.Message(fld))
			if s.idxLine != nil {
				line = s.idxLine(slot, line)
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
		if file == gfsFilePgrb2 {
			lines = append(lines, add("UGRD", "10 m above ground", ugrd))
		}
		f.mu.Lock()
		f.files[gridPath(s.run, s.step, file, false)] = body
		f.files[gridPath(s.run, s.step, file, true)] = []byte(strings.Join(lines, "\n") + "\n")
		f.mu.Unlock()
	}
}

// publishRun publishes steps 0 and 3 of run.
func (f *fakeNOAA) publishRun(run int64) {
	for _, step := range []int{0, 3} {
		f.publish(fakeGridStep{run: run, step: step})
	}
}

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func (l *logBuf) contains(s string) bool { return strings.Contains(l.String(), s) }

func openGridTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "grid.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newTestGridJob is a job with forecastHours 3 (steps f000, f003) on the
// test lattice, clock at now.
func newTestGridJob(store ductingGridStore, baseURL string, now time.Time) (*ductingGridJob, *logBuf) {
	j := newDuctingGridJob(store, ductingGridSettings{HistoryDays: 7, ForecastHours: 3, BaseURL: baseURL})
	j.lattice = testGridLattice
	j.now = func() time.Time { return now }
	j.sleep = func(context.Context, time.Duration) bool { return false }
	lb := &logBuf{}
	j.logf = lb.logf
	return j, lb
}

func testGridNow() time.Time { return time.Unix(testGridRun, 0).Add(4 * time.Hour) }

func storedLeads(t *testing.T, s *Store, run int64) []int {
	t.Helper()
	leads, err := s.DuctingRunLeads(ductingGridModel, run)
	if err != nil {
		t.Fatal(err)
	}
	return leads
}

func leadsEqual(a []int, b ...int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDuctingGridHappyPath(t *testing.T) {
	noaa, srv := newFakeNOAA(t)
	noaa.publishRun(testGridRun)
	store := openGridTestStore(t)
	j, logs := newTestGridJob(store, srv.URL, testGridNow())
	j.runCycle(context.Background())

	if !logs.contains("[ducting-grid] run 2026112006: 2/2 steps") {
		t.Fatalf("summary missing:\n%s", logs)
	}
	if strings.Contains(logs.String(), srv.URL) || strings.Contains(logs.String(), "127.0.0.1") {
		t.Fatalf("log prints the URL:\n%s", logs)
	}
	for _, step := range []int{0, 3} {
		run, lead, data, _, ok := ductingRow(t, store, ductingGridModel, testGridRun+int64(step)*3600)
		if !ok || run != testGridRun || lead != step {
			t.Fatalf("step %d: run=%d lead=%d found=%v", step, run, lead, ok)
		}
		g, err := ducting.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		if g.Ni != 12 || g.Nj != 7 || g.RunTime != testGridRun || g.ValidTime != testGridRun+int64(step)*3600 || g.Model != "gfs" {
			t.Fatalf("grid header %+v", g)
		}
		for i, c := range g.C {
			want := ducting.ClassNormal
			if i == testGridDuctPoint {
				want = ducting.ClassSurface
			}
			if c != want {
				t.Errorf("step %d point %d class %d, want %d", step, i, c, want)
			}
		}
		if g.F[testGridDuctPoint] == 0 {
			t.Errorf("ducted point has no fmin")
		}
	}
}

func TestDuctingGridNewestCycleProbing(t *testing.T) {
	noaa, srv := newFakeNOAA(t)
	noaa.publishRun(testGridRun)
	store := openGridTestStore(t)
	// The clock is in the 12Z cycle, which is not published: fall back to 06Z.
	now := time.Unix(testGridRun, 0).Add(7 * time.Hour)
	j, logs := newTestGridJob(store, srv.URL, now)
	j.runCycle(context.Background())
	if !leadsEqual(storedLeads(t, store, testGridRun), 0, 3) {
		t.Fatalf("06Z not stored:\n%s", logs)
	}
	newer := testGridRun + ductingGridCycleSeconds
	if noaa.count(gridPath(newer, 3, gfsFilePgrb2, true)) != 1 {
		t.Fatalf("12Z last step not probed")
	}

	// Already stored and complete: no download of 06Z at all.
	before := noaa.totalCount("gfs.20261120/06/")
	j.runCycle(context.Background())
	if got := noaa.totalCount("gfs.20261120/06/"); got != before {
		t.Fatalf("stored run fetched again: %d requests, was %d", got, before)
	}

	// 12Z published: it is newer and replaces nothing older.
	noaa.publishRun(newer)
	j.runCycle(context.Background())
	if !leadsEqual(storedLeads(t, store, newer), 0, 3) {
		t.Fatalf("12Z not stored:\n%s", logs)
	}
	latest, _ := store.LatestDuctingRun(ductingGridModel)
	if latest != newer {
		t.Fatalf("latest run %d, want %d", latest, newer)
	}
}

func TestDuctingGridNothingPublished(t *testing.T) {
	_, srv := newFakeNOAA(t)
	store := openGridTestStore(t)
	j, logs := newTestGridJob(store, srv.URL, testGridNow())
	j.runCycle(context.Background())
	if !logs.contains("no published GFS cycle") {
		t.Fatalf("logs:\n%s", logs)
	}
	if l, _ := store.LatestDuctingRun(ductingGridModel); l != 0 {
		t.Fatalf("stored run %d", l)
	}
}

func TestDuctingGridStepFailureIsolation(t *testing.T) {
	noaa, srv := newFakeNOAA(t)
	noaa.publish(fakeGridStep{run: testGridRun, step: 0})
	noaa.publish(fakeGridStep{run: testGridRun, step: 3, idxLine: func(slot int, line string) string {
		if slot == slotRh(6) { // RH 850 mb
			return ""
		}
		return line
	}})
	store := openGridTestStore(t)
	j, logs := newTestGridJob(store, srv.URL, testGridNow())
	j.runCycle(context.Background())
	if !leadsEqual(storedLeads(t, store, testGridRun), 0) {
		t.Fatalf("want only f000 stored:\n%s", logs)
	}
	if !logs.contains("step f003 failed: RH 850 mb is missing from the pgrb2 index") || !logs.contains("1/2 steps") {
		t.Fatalf("logs:\n%s", logs)
	}

	// Next tick, f003 fixed: only f003 is downloaded.
	f000 := noaa.count(gridPath(testGridRun, 0, gfsFilePgrb2, false)) + noaa.count(gridPath(testGridRun, 0, gfsFilePgrb2, true))
	noaa.publish(fakeGridStep{run: testGridRun, step: 3})
	j.runCycle(context.Background())
	if !leadsEqual(storedLeads(t, store, testGridRun), 0, 3) {
		t.Fatalf("f003 not retried:\n%s", logs)
	}
	if got := noaa.count(gridPath(testGridRun, 0, gfsFilePgrb2, false)) + noaa.count(gridPath(testGridRun, 0, gfsFilePgrb2, true)); got != f000 {
		t.Fatalf("f000 downloaded again (%d requests, was %d)", got, f000)
	}
	if !logs.contains("2/2 steps") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// runRefusal publishes the run, applies corrupt to step f000 (by
// republishing it or installing a server hook), runs one cycle and checks
// that f000 was refused with wantLog while f003 was stored.
func runRefusal(t *testing.T, step0 *fakeGridStep, hook func(w http.ResponseWriter, r *http.Request) bool, tune func(j *ductingGridJob), wantLog string) (*fakeNOAA, *logBuf) {
	t.Helper()
	noaa, srv := newFakeNOAA(t)
	noaa.publishRun(testGridRun)
	if step0 != nil {
		step0.run, step0.step = testGridRun, 0
		noaa.publish(*step0)
	}
	noaa.hook = hook
	store := openGridTestStore(t)
	j, logs := newTestGridJob(store, srv.URL, testGridNow())
	if tune != nil {
		tune(j)
	}
	j.runCycle(context.Background())
	if leads := storedLeads(t, store, testGridRun); !leadsEqual(leads, 3) {
		t.Fatalf("stored leads %v, want [3]:\n%s", leads, logs)
	}
	if !logs.contains("step f000 failed") || !logs.contains(wantLog) {
		t.Fatalf("want %q in logs:\n%s", wantLog, logs)
	}
	if strings.Contains(logs.String(), srv.URL) {
		t.Fatalf("log prints the URL:\n%s", logs)
	}
	return noaa, logs
}

// onPath returns a hook that answers requests for path with h.
func onPath(path string, h func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != path {
			return false
		}
		h(w, r)
		return true
	}
}

func TestDuctingGridRefuses200ForRange(t *testing.T) {
	p := gridPath(testGridRun, 0, gfsFilePgrb2, false)
	runRefusal(t, nil, onPath(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, 1<<16))
	}), nil, "byte range not honoured (HTTP 200, want 206)")
}

func TestDuctingGridRefusesWrongContentRange(t *testing.T) {
	p := gridPath(testGridRun, 0, gfsFilePgrb2b, false)
	runRefusal(t, nil, onPath(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 10))
	}), nil, "Content-Range does not match")
}

func TestDuctingGridRefusesRedirect(t *testing.T) {
	p := gridPath(testGridRun, 0, gfsFilePgrb2, true)
	noaa, _ := runRefusal(t, nil, onPath(p, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere/file.idx", http.StatusFound)
	}), nil, "redirect refused (HTTP 302)")
	if noaa.count("/elsewhere/file.idx") != 0 {
		t.Fatal("redirect followed")
	}
}

func TestDuctingGridRefusesOversizeMessage(t *testing.T) {
	// The .idx range of one f000 message is over the limit: refused before
	// any byte of it is requested.
	big := &fakeGridStep{field: func(slot int, f *grib2test.Field) {
		if slot == slotTmp(0) {
			f.Ni, f.Nj = 120, 70
			f.Values = make([]float64, 120*70)
			for i := range f.Values {
				f.Values[i] = 200 + float64(i%1000)/10
			}
		}
	}}
	runRefusal(t, big, nil, func(j *ductingGridJob) { j.maxMsg = 4096 }, "TMP 1000 mb: message of")
}

func TestDuctingGridRefusesOversizeOpenRange(t *testing.T) {
	// The last pgrb2b message has an open-ended range; the server claims
	// more than the limit in Content-Range.
	p := gridPath(testGridRun, 0, gfsFilePgrb2b, false)
	runRefusal(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		rng := r.Header.Get("Range")
		if r.URL.Path != p || !strings.HasSuffix(rng, "-") {
			return false
		}
		start := strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-")
		w.Header().Set("Content-Range", "bytes "+start+"-99999999/100000000")
		w.WriteHeader(http.StatusPartialContent)
		return true
	}, nil, "exceeds the 2097152 byte limit")
}

func TestDuctingGridRefusesOversizeIndex(t *testing.T) {
	// A chunked .idx (no Content-Length) longer than the limit.
	p := gridPath(testGridRun, 0, gfsFilePgrb2, true)
	runRefusal(t, nil, onPath(p, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 64; i++ {
			_, _ = w.Write(bytes.Repeat([]byte("1:0:d=2026112006:TMP:850 mb:anl:\n"), 64))
			w.(http.Flusher).Flush()
		}
	}), func(j *ductingGridJob) { j.maxIdx = 16 << 10 }, "body exceeds the 16384 byte limit")
}

func TestDuctingGridBudget(t *testing.T) {
	noaa, srv := newFakeNOAA(t)
	noaa.publishRun(testGridRun)
	store := openGridTestStore(t)
	j, logs := newTestGridJob(store, srv.URL, testGridNow())
	j.budget = 8 << 10
	j.runCycle(context.Background())
	if !logs.contains("download budget exceeded") || !logs.contains("0/2 steps") {
		t.Fatalf("logs:\n%s", logs)
	}
	// The budget stops the cycle: f003 is not even tried.
	if logs.contains("step f003") {
		t.Fatalf("cycle went on after the budget ran out:\n%s", logs)
	}
}

func TestDuctingGridRefusesIndexWrongCycle(t *testing.T) {
	runRefusal(t, &fakeGridStep{idxLine: func(slot int, line string) string {
		return strings.Replace(line, "d=2026112006", "d=2026112000", 1)
	}}, nil, nil, "is for cycle 2026112000, want 2026112006")
}

func TestDuctingGridRefusesIndexWrongForecast(t *testing.T) {
	runRefusal(t, &fakeGridStep{idxLine: func(slot int, line string) string {
		if slot == slotTmp2m {
			return strings.Replace(line, ":anl:", ":3 hour fcst:", 1)
		}
		return line
	}}, nil, nil, `TMP 2 m above ground in the pgrb2 index has forecast "3 hour fcst", want "anl"`)
}

func TestDuctingGridRefusesBadFields(t *testing.T) {
	cases := []struct {
		name  string
		field func(slot int, f *grib2test.Field)
		want  string
	}{
		{"dims", func(slot int, f *grib2test.Field) {
			if slot == slotTmp(6) {
				f.Ni, f.Values = 6, f.Values[:6*7]
			}
		}, "TMP 850 mb: lattice 6x7"},
		{"spacing", func(slot int, f *grib2test.Field) {
			if slot == slotHgtSfc {
				f.DLat = -29
			}
		}, "HGT surface: lattice"},
		{"temperature range", func(slot int, f *grib2test.Field) {
			if slot == slotTmp(6) {
				f.Values[5] = 400
			}
		}, "TMP 850 mb: value 400 at point 5 outside 150..350"},
		{"pressure range", func(slot int, f *grib2test.Field) {
			if slot == slotPresSfc {
				f.Values[0] = 200000
			}
		}, "PRES surface: value 200000 at point 0 outside 30000..110000"},
		{"humidity range", func(slot int, f *grib2test.Field) {
			if slot == slotRh(0) {
				f.Values[3] = -5
			}
		}, "RH 1000 mb: value -5 at point 3 outside 0..110"},
		{"reference time", func(slot int, f *grib2test.Field) {
			if slot == slotRh2m {
				f.Ref = f.Ref.Add(-6 * time.Hour)
			}
		}, "RH 2 m above ground: reference time 2026112000, want 2026112006"},
		{"forecast hour", func(slot int, f *grib2test.Field) {
			if slot == slotRh2m {
				f.ForecastHour = 3
			}
		}, "forecast hour 3, want 0"},
		{"parameter", func(slot int, f *grib2test.Field) {
			if slot == slotTmp(5) { // TMP 875 mb, pgrb2b
				f.Category, f.Number = 1, 1
			}
		}, "TMP 875 mb: parameter 0.1.1, want 0.0.0"},
		{"level", func(slot int, f *grib2test.Field) {
			if slot == slotHgt(2) {
				f.SurfaceValue = 90000
			}
		}, "HGT 950 mb: level type 100 value 90000, want type 100 value 95000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runRefusal(t, &fakeGridStep{field: tc.field}, nil, nil, tc.want)
		})
	}
}

func TestDuctingGridAcceptsBitmapMissing(t *testing.T) {
	noaa, srv := newFakeNOAA(t)
	noaa.publishRun(testGridRun)
	noaa.publish(fakeGridStep{run: testGridRun, step: 0, field: func(slot int, f *grib2test.Field) {
		if slot == slotRh2m || slot == slotTmp(0) {
			f.Values[7] = math.NaN()
		}
	}})
	store := openGridTestStore(t)
	j, logs := newTestGridJob(store, srv.URL, testGridNow())
	j.runCycle(context.Background())
	if !leadsEqual(storedLeads(t, store, testGridRun), 0, 3) {
		t.Fatalf("NaN (bitmap) refused:\n%s", logs)
	}
}

func TestDuctingGridComputePanicRecovered(t *testing.T) {
	noaa, srv := newFakeNOAA(t)
	noaa.publishRun(testGridRun)
	store := openGridTestStore(t)
	j, logs := newTestGridJob(store, srv.URL, testGridNow())
	j.analyze = func([]ducting.Level) ducting.Result { panic("boom") }
	j.runCycle(context.Background())
	if !logs.contains("panic: boom") || !logs.contains("0/2 steps") || logs.contains("step f003") {
		t.Fatalf("want the panic to end the cycle:\n%s", logs)
	}
	if l, _ := store.LatestDuctingRun(ductingGridModel); l != 0 {
		t.Fatalf("stored run %d after a panic", l)
	}
	// The job keeps working on the next cycle.
	j.analyze = ducting.Analyze
	j.runCycle(context.Background())
	if !leadsEqual(storedLeads(t, store, testGridRun), 0, 3) {
		t.Fatalf("not recovered:\n%s", logs)
	}
}

// gridStoreSpy wraps a Store, records prune cutoffs and can panic.
type gridStoreSpy struct {
	*Store
	mu          sync.Mutex
	prunes      []int64
	latestCalls int
	panicLatest bool
}

func (s *gridStoreSpy) PruneDuctingGrid(model string, before int64) (int64, error) {
	s.mu.Lock()
	s.prunes = append(s.prunes, before)
	s.mu.Unlock()
	return s.Store.PruneDuctingGrid(model, before)
}

func (s *gridStoreSpy) LatestDuctingRun(model string) (int64, error) {
	s.mu.Lock()
	s.latestCalls++
	p := s.panicLatest
	s.mu.Unlock()
	if p {
		panic("store exploded")
	}
	return s.Store.LatestDuctingRun(model)
}

func TestDuctingGridPanicOutsideStepRecovered(t *testing.T) {
	_, srv := newFakeNOAA(t)
	spy := &gridStoreSpy{Store: openGridTestStore(t), panicLatest: true}
	j, logs := newTestGridJob(spy, srv.URL, testGridNow())
	j.runCycle(context.Background())
	if !logs.contains("panic, cycle abandoned: store exploded") {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestDuctingGridPruneCutoff(t *testing.T) {
	noaa, srv := newFakeNOAA(t)
	noaa.publishRun(testGridRun)
	spy := &gridStoreSpy{Store: openGridTestStore(t)}
	old := testGridRun - 8*86400
	if _, err := spy.UpsertDuctingGrid(ductingGridModel, old, old, 0, []byte("x"), old); err != nil {
		t.Fatal(err)
	}
	now := testGridNow()
	j, logs := newTestGridJob(spy, srv.URL, now)
	j.runCycle(context.Background())
	want := now.Unix() - 7*86400
	if len(spy.prunes) != 1 || spy.prunes[0] != want {
		t.Fatalf("prunes %v, want [%d]", spy.prunes, want)
	}
	if _, _, _, _, ok := ductingRow(t, spy.Store, ductingGridModel, old); ok {
		t.Fatal("old step not pruned")
	}
	if !logs.contains("pruned 1 steps older than 7 days") {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestDuctingGridScheduler(t *testing.T) {
	_, srv := newFakeNOAA(t)
	spy := &gridStoreSpy{Store: openGridTestStore(t)}
	j, _ := newTestGridJob(spy, srv.URL, testGridNow())
	var mu sync.Mutex
	var sleeps []time.Duration
	j.sleep = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)
		return len(sleeps) < 3
	}
	stop := j.start()
	stop()
	stop() // idempotent
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{ductingGridStartDelay, ductingGridInterval, ductingGridInterval}
	if fmt.Sprint(sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps %v, want %v", sleeps, want)
	}
	if spy.latestCalls != 2 {
		t.Fatalf("%d cycles, want 2", spy.latestCalls)
	}
}

func TestDuctingGridStopCancelsWait(t *testing.T) {
	store := openGridTestStore(t)
	j, _ := newTestGridJob(store, "https://example.invalid", testGridNow())
	j.sleep = sleepCtx // the real 30 s start delay
	stop := j.start()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not cancel the start delay")
	}
}

func TestDuctingGridSteps(t *testing.T) {
	j := &ductingGridJob{forecastHours: 9}
	if got := fmt.Sprint(j.steps()); got != "[0 3 6 9]" {
		t.Fatalf("steps %s", got)
	}
	if forecastLabel(0) != "anl" || forecastLabel(6) != "6 hour fcst" {
		t.Fatal("forecast labels")
	}
	if got := fmt.Sprint(missingSteps([]int{0, 3, 6, 9}, []int{3, 9, 12})); got != "[0 6]" {
		t.Fatalf("missing %s", got)
	}
	if p := gridPath(testGridRun, 6, gfsFilePgrb2b, true); p != "/gfs.20261120/06/atmos/gfs.t06z.pgrb2b.0p50.f006.idx" {
		t.Fatalf("path %s", p)
	}
	if len(ductingGridVars) != 43 {
		t.Fatalf("%d messages per step, want 43", len(ductingGridVars))
	}
}

func TestDuctingGridParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		h          string
		start, end int64
		ok         bool
	}{
		{"bytes 0-99/1000", 0, 99, true},
		{"bytes 10-19/*", 10, 19, true},
		{"bytes 10-19/19", 0, 0, false},
		{"bytes 20-19/100", 0, 0, false},
		{"bytes -1-19/100", 0, 0, false},
		{"bytes 0-99", 0, 0, false},
		{"items 0-99/100", 0, 0, false},
		{"", 0, 0, false},
	} {
		s, e, ok := parseContentRange(tc.h)
		if ok != tc.ok || ok && (s != tc.start || e != tc.end) {
			t.Errorf("%q = %d %d %v", tc.h, s, e, ok)
		}
	}
}

// BenchmarkDuctingGridStep is one full-size step (720 x 361, 43 messages)
// through decode, field checks, compute and encode. Downloads excluded.
func BenchmarkDuctingGridStep(b *testing.B) {
	l := gfsHalfDegreeLattice
	n := l.Ni * l.Nj
	var msgs [ductingGridSlots][]byte
	for slot, v := range ductingGridVars {
		vals := make([]float64, n)
		for i := range vals {
			// Every 50th point ducted; small per-point jitter so packing is not trivial.
			x := testGridValue(slot, testGridDuctPoint+1)
			if i%50 == 0 {
				x = testGridValue(slot, testGridDuctPoint)
			}
			if v.Name != "PRES" && v.Level != "surface" {
				x += float64(i%97) * 0.01
			}
			vals[i] = x
		}
		sv := v.SurfValue
		if v.SurfType == gribSurfaceGround {
			sv = math.NaN()
		}
		msgs[slot] = grib2test.Message(grib2test.Field{
			Ni: l.Ni, Nj: l.Nj, La1: l.La1, Lo1: l.Lo1, DLat: l.DLat, DLon: l.DLon,
			Ref: time.Unix(testGridRun, 0), ForecastHour: 0,
			Category: v.Category, Number: v.Number, SurfaceType: v.SurfType, SurfaceValue: sv, Values: vals,
		})
	}
	j := newDuctingGridJob(nil, ductingGridSettings{HistoryDays: 7, ForecastHours: 3, BaseURL: ductingGridDefaultBaseURL})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	var size int
	for it := 0; it < b.N; it++ {
		var fields ductingGridFields
		for slot := range msgs {
			f, err := grib2.Decode(msgs[slot])
			if err != nil {
				b.Fatal(err)
			}
			if err := j.checkField(f, ductingGridVars[slot], testGridRun, 0); err != nil {
				b.Fatal(err)
			}
			fields[slot] = f.Values
		}
		g, err := ducting.NewGrid(ductingGridModel, testGridRun, testGridRun, l.La1, l.Lo1, l.DLat, l.DLon, l.Ni, l.Nj)
		if err != nil {
			b.Fatal(err)
		}
		if err := j.computeGrid(ctx, &fields, g); err != nil {
			b.Fatal(err)
		}
		data, err := ducting.Encode(g)
		if err != nil {
			b.Fatal(err)
		}
		size = len(data)
	}
	b.ReportMetric(float64(size), "encoded-bytes")
}
