package main

// Global ducting grid job (ducting.source "gfs-grid"): the ingestor finds
// the newest NOAA GFS 0.5° cycle, downloads only the 43 messages a step
// needs (HTTP Range by the .idx inventory), decodes them with
// internal/grib2, runs the duct physics (internal/ducting) for every lattice
// point and stores one encoded grid per forecast step in ducting_grid,
// keeping historyDays of past steps.
//
// The job must never put MQTT ingest at risk:
//   - everything downloaded is untrusted: bounded reads, no redirects,
//     206 + matching Content-Range for every byte range, a per-cycle
//     download budget, and every decoded field checked (lattice, times,
//     parameter, level, physical range) before use;
//   - a panic anywhere in a cycle is recovered and ends that cycle only;
//   - one step at a time is held in memory (~45 MB of fields at 0.5°);
//   - the write path is taken once per step, after the step is computed and
//     encoded, never while downloading.
// Log lines never print URLs (a mirror's URL is operator config).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/ducting"
	"github.com/meshcore-analyzer/grib2"
)

// Fixed limits of the grid job (customizer/config later if ever needed).
const (
	ductingGridModel          = "gfs"
	ductingGridStartDelay     = 30 * time.Second
	ductingGridInterval       = 30 * time.Minute
	ductingGridCycleTimeout   = 20 * time.Minute
	ductingGridRequestTimeout = 30 * time.Second
	ductingGridStopWait       = 10 * time.Second
	ductingGridMaxIdxBytes    = 1 << 20
	ductingGridMaxMsgBytes    = 2 << 20
	ductingGridMaxErrBytes    = 4 << 10
	ductingGridCycleBudget    = 300 << 20 // bytes downloaded per cycle, probes included
	ductingGridFetchParallel  = 4
	ductingGridCycleSeconds   = 6 * 3600
	ductingGridProbeCycles    = 4 // the current cycle and up to 3 earlier ones
	ductingGridRowChunk       = 4 // lattice rows per compute work item
)

// ductingGridLevelsHPa are the pressure levels of the profile (lowest
// ~3 km), highest pressure first.
var ductingGridLevelsHPa = [...]float64{1000, 975, 950, 925, 900, 875, 850, 825, 800, 775, 750, 725, 700}

// pgrb2b carries the odd 25 hPa levels; everything else is in pgrb2.
var ductingGridPgrb2bLevels = map[float64]bool{875: true, 825: true, 775: true, 725: true}

const (
	gfsFilePgrb2 = iota
	gfsFilePgrb2b
)

var gfsFileKinds = [...]string{gfsFilePgrb2: "pgrb2", gfsFilePgrb2b: "pgrb2b"}

// GRIB2 surface types (code table 4.5).
const (
	gribSurfaceGround   = 1
	gribSurfaceIsobaric = 100
	gribSurfaceAboveGnd = 103
)

// ductingGridVar is one message a step needs: its .idx names, the file
// it is in, the GRIB2 parameter/level the decoded field must report, and the
// physical range its values must fall in.
type ductingGridVar struct {
	Name, Level string
	File        int
	Category    uint8
	Number      uint8
	SurfType    uint8
	SurfValue   float64 // SI units; not checked for gribSurfaceGround
	Lo, Hi      float64
}

func (v ductingGridVar) label() string { return v.Name + " " + v.Level }

// Slots of ductingGridVars: four surface fields, then TMP, RH, HGT for each
// level of ductingGridLevelsHPa.
const (
	slotPresSfc = iota
	slotHgtSfc
	slotTmp2m
	slotRh2m
	slotLevel0
	ductingGridSlots = slotLevel0 + 3*len(ductingGridLevelsHPa)
)

func slotTmp(i int) int { return slotLevel0 + 3*i }
func slotRh(i int) int  { return slotLevel0 + 3*i + 1 }
func slotHgt(i int) int { return slotLevel0 + 3*i + 2 }

var ductingGridVars = func() [ductingGridSlots]ductingGridVar {
	tmp := func(level string, file int, st uint8, sv float64) ductingGridVar {
		return ductingGridVar{"TMP", level, file, 0, 0, st, sv, 150, 350}
	}
	rh := func(level string, file int, st uint8, sv float64) ductingGridVar {
		return ductingGridVar{"RH", level, file, 1, 1, st, sv, 0, 110}
	}
	hgt := func(level string, file int, st uint8, sv float64) ductingGridVar {
		return ductingGridVar{"HGT", level, file, 3, 5, st, sv, -1000, 40000}
	}
	var v [ductingGridSlots]ductingGridVar
	v[slotPresSfc] = ductingGridVar{"PRES", "surface", gfsFilePgrb2, 3, 0, gribSurfaceGround, 0, 30000, 110000}
	v[slotHgtSfc] = hgt("surface", gfsFilePgrb2, gribSurfaceGround, 0)
	v[slotTmp2m] = tmp("2 m above ground", gfsFilePgrb2, gribSurfaceAboveGnd, 2)
	v[slotRh2m] = rh("2 m above ground", gfsFilePgrb2, gribSurfaceAboveGnd, 2)
	for i, p := range ductingGridLevelsHPa {
		file := gfsFilePgrb2
		if ductingGridPgrb2bLevels[p] {
			file = gfsFilePgrb2b
		}
		level := strconv.Itoa(int(p)) + " mb"
		v[slotTmp(i)] = tmp(level, file, gribSurfaceIsobaric, p*100)
		v[slotRh(i)] = rh(level, file, gribSurfaceIsobaric, p*100)
		v[slotHgt(i)] = hgt(level, file, gribSurfaceIsobaric, p*100)
	}
	return v
}()

// ductingGridLattice is the lattice every decoded field must be on. The
// job pins the GFS 0.5° lattice; tests use a small one.
type ductingGridLattice struct {
	Ni, Nj     int
	La1, Lo1   float64
	DLat, DLon float64
}

var gfsHalfDegreeLattice = ductingGridLattice{Ni: 720, Nj: 361, La1: 90, Lo1: 0, DLat: -0.5, DLon: 0.5}

// ductingGridStore is the slice of *Store the job uses.
type ductingGridStore interface {
	UpsertDuctingGrid(model string, validTime, runTime int64, leadHours int, data []byte, createdAt int64) (bool, error)
	LatestDuctingRun(model string) (int64, error)
	DuctingRunLeads(model string, runTime int64) ([]int, error)
	DuctingValidLeads(model string, from, to int64) (map[int64]int, error)
	PruneDuctingGrid(model string, before int64) (int64, error)
}

// ductingGridJob is one configured grid job. The function fields are the
// injection points for tests (clock, sleep, physics, logging).
type ductingGridJob struct {
	store         ductingGridStore
	client        *http.Client
	baseURL       string
	forecastHours int
	historyDays   int
	lattice       ductingGridLattice

	budget  int64 // bytes per cycle
	maxIdx  int64
	maxMsg  int64
	workers int // compute workers

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool // false when ctx ended first
	// analyze overrides the physics (tests inject failures). nil, the
	// default, runs the allocation-free ducting.Scratch path, which gives
	// the same results as ducting.Analyze(ducting.Profile(...)).
	analyze func([]ducting.Level) ducting.Result
	logf    func(format string, args ...any)

	// missingCycles holds past cycles the bucket answered 404 for, with
	// when (unix s): backfill skips them for ductingGridMissingRetry. Used
	// only from the scheduler goroutine; bounded by forgetMissing.
	missingCycles map[int64]int64
}

// newDuctingGridHTTPClient returns the job's client: 30 s per request and
// redirects never followed (a 3xx comes back as the response and is refused).
func newDuctingGridHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = ductingGridFetchParallel
	return &http.Client{
		Transport: tr,
		Timeout:   ductingGridRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newDuctingGridJob(store ductingGridStore, s ductingGridSettings) *ductingGridJob {
	return &ductingGridJob{
		store:         store,
		client:        newDuctingGridHTTPClient(),
		baseURL:       s.BaseURL,
		forecastHours: s.ForecastHours,
		historyDays:   s.HistoryDays,
		lattice:       gfsHalfDegreeLattice,
		budget:        ductingGridCycleBudget,
		maxIdx:        ductingGridMaxIdxBytes,
		maxMsg:        ductingGridMaxMsgBytes,
		workers:       ductingGridWorkers(runtime.GOMAXPROCS(0)),
		now:           time.Now,
		sleep:         sleepCtx,
		logf:          log.Printf,
	}
}

// ductingGridWorkers is the compute worker count for procs available CPUs:
// one fewer, so the MQTT ingest sharing the process always has a core to run
// on while a step is computed (a step is ~0.2 s of CPU, 17 steps a run, so the
// run takes slightly longer on a multi-core host; a 1-CPU host keeps 1 worker).
func ductingGridWorkers(procs int) int { return max(1, procs-1) }

// sleepCtx waits d or until ctx ends; false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// startDuctingGridJob resolves the config, logs its warnings and starts the
// job. It returns a stop func (idempotent) that cancels any cycle in flight
// and waits up to ductingGridStopWait for it to end.
func startDuctingGridJob(cfg *DuctingConfig, store ductingGridStore) func() {
	s, warns := cfg.resolved()
	for _, w := range warns {
		log.Printf("[ducting-grid] %s", w)
	}
	log.Printf("[ducting-grid] enabled (model=%s, forecast=%d h, history=%d d, check every %s)",
		ductingGridModel, s.ForecastHours, s.HistoryDays, ductingGridInterval)
	return newDuctingGridJob(store, s).start()
}

// start runs the scheduler: first cycle after ductingGridStartDelay, then
// every ductingGridInterval.
func (j *ductingGridJob) start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				j.logf("[ducting-grid] scheduler panic, job stopped: %v\n%s", r, debug.Stack())
			}
		}()
		if !j.sleep(ctx, ductingGridStartDelay) {
			return
		}
		for {
			j.runCycle(ctx)
			if !j.sleep(ctx, ductingGridInterval) {
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(cancel)
		select {
		case <-done:
		case <-time.After(ductingGridStopWait):
			j.logf("[ducting-grid] still stopping after %s", ductingGridStopWait)
		}
	}
}

// errDuctingGridPanic marks a recovered panic inside a step: it ends the cycle.
var errDuctingGridPanic = errors.New("panic")

// recoverAsError turns a panic into an error wrapping errDuctingGridPanic.
func recoverAsError(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%w: %v\n%s", errDuctingGridPanic, r, debug.Stack())
	}
}

// runCycle is one tick: a panic anywhere logs and ends this cycle only.
func (j *ductingGridJob) runCycle(parent context.Context) {
	defer func() {
		if r := recover(); r != nil {
			j.logf("[ducting-grid] panic, cycle abandoned: %v\n%s", r, debug.Stack())
		}
	}()
	ctx, cancel := context.WithTimeout(parent, ductingGridCycleTimeout)
	defer cancel()
	j.runOnce(ctx)
}

// steps returns the forecast hours of one run: 0, 3, ..., forecastHours.
func (j *ductingGridJob) steps() []int {
	out := make([]int, 0, j.forecastHours/ductingGridStepHours+1)
	for h := 0; h <= j.forecastHours; h += ductingGridStepHours {
		out = append(out, h)
	}
	return out
}

func cycleStamp(run int64) string { return time.Unix(run, 0).UTC().Format("2006010215") }

// runOnce is one tick: the steps of the newest usable cycle not yet stored,
// then history gaps (backfill) with what is left of the download budget,
// then pruning. Backfill and prune share one clock reading, so prune never
// removes a step backfill just wrote.
func (j *ductingGridJob) runOnce(ctx context.Context) {
	now := j.now().Unix()
	latest, err := j.store.LatestDuctingRun(ductingGridModel)
	if err != nil {
		j.logf("[ducting-grid] reading the latest stored run: %v", err)
		return
	}
	budget := &ductingGridBudget{limit: j.budget}
	if j.runNewest(ctx, latest, budget) {
		if latest, err = j.store.LatestDuctingRun(ductingGridModel); err != nil {
			j.logf("[ducting-grid] reading the latest stored run: %v", err)
		} else {
			j.backfill(ctx, now, latest, budget)
		}
	}

	cutoff := j.historyCutoff(now)
	if n, err := j.store.PruneDuctingGrid(ductingGridModel, cutoff); err != nil {
		j.logf("[ducting-grid] prune: %v", err)
	} else if n > 0 {
		j.logf("[ducting-grid] pruned %d steps older than %d days", n, j.historyDays)
	}
}

// historyCutoff is the oldest valid time kept: now - historyDays.
func (j *ductingGridJob) historyCutoff(now int64) int64 {
	return now - int64(j.historyDays)*86400
}

// runNewest computes the steps of the newest usable cycle not yet stored.
// false means the tick must end here (a panic, or the cycle context ended).
func (j *ductingGridJob) runNewest(ctx context.Context, latest int64, budget *ductingGridBudget) bool {
	start := j.now()
	run, missing, ok := j.pickCycle(ctx, latest, budget)
	if !ok {
		return ctx.Err() == nil
	}
	stamp := cycleStamp(run)
	total := len(j.steps())
	written := 0
	cont := true
	for _, step := range missing {
		if ctx.Err() != nil {
			j.logf("[ducting-grid] run %s: stopped before step f%03d: %v", stamp, step, ctx.Err())
			cont = false
			break
		}
		err := j.writeStep(ctx, run, step, budget)
		if err != nil {
			j.logf("[ducting-grid] run %s step f%03d failed: %v", stamp, step, err)
			if errors.Is(err, errDuctingGridPanic) || ctx.Err() != nil {
				cont = false
				break
			}
			if errors.Is(err, errDuctingGridBudget) {
				break
			}
			continue
		}
		written++
	}
	j.logf("[ducting-grid] run %s: %d/%d steps, %d MB, %s",
		stamp, total-len(missing)+written, total, budget.used()/1e6, j.now().Sub(start).Round(time.Second))
	return cont
}

// writeStep builds one step of run and stores it.
func (j *ductingGridJob) writeStep(ctx context.Context, run int64, step int, budget *ductingGridBudget) error {
	data, err := j.buildStep(ctx, run, step, budget)
	if err != nil {
		return err
	}
	_, err = j.store.UpsertDuctingGrid(ductingGridModel, run+int64(step)*3600, run, step, data, j.now().Unix())
	return err
}

// ductingGridBackfillLeads are the analysis steps backfill stores per past
// cycle: f000 and f003 cover the cycle's 6 hours (valid C and C+3h).
var ductingGridBackfillLeads = [...]int{0, 3}

// ductingGridMissingRetry is how long a cycle the bucket does not have
// (404 on its .idx) is left alone before backfill asks again.
const ductingGridMissingRetry = 86400

type ductingGridBackfillStep struct {
	run  int64
	lead int
}

// backfill fills history gaps: for every cycle C with
// historyCutoff(now) <= C < latest, newest first, the steps f000 and f003
// unless a row for that valid time with lead_hours <= 3 already exists. It
// uses what the newest-run work left of the budget and the cycle context;
// whatever is left over continues on the next tick.
func (j *ductingGridJob) backfill(ctx context.Context, now, latest int64, budget *ductingGridBudget) {
	if latest <= 0 {
		return
	}
	cutoff := j.historyCutoff(now)
	first := cutoff + (ductingGridCycleSeconds-cutoff%ductingGridCycleSeconds)%ductingGridCycleSeconds // first cycle >= cutoff
	j.forgetMissing(now, first)
	stored, err := j.store.DuctingValidLeads(ductingGridModel, cutoff, latest)
	if err != nil {
		j.logf("[ducting-grid] backfill: reading stored steps: %v", err)
		return
	}
	var todo []ductingGridBackfillStep
	for c := latest - ductingGridCycleSeconds; c >= first; c -= ductingGridCycleSeconds {
		if _, skip := j.missingCycles[c]; skip {
			continue
		}
		for _, lead := range ductingGridBackfillLeads {
			if l, ok := stored[c+int64(lead)*3600]; ok && l <= ductingGridBackfillLeads[len(ductingGridBackfillLeads)-1] {
				continue
			}
			todo = append(todo, ductingGridBackfillStep{c, lead})
		}
	}
	if len(todo) == 0 {
		return
	}
	used0 := budget.used()
	written, gone := 0, 0
	for i := 0; i < len(todo); i++ {
		st := todo[i]
		if ctx.Err() != nil {
			break
		}
		if _, skip := j.missingCycles[st.run]; skip {
			gone++ // the cycle's other step just came back 404
			continue
		}
		err := j.writeStep(ctx, st.run, st.lead, budget)
		if err == nil {
			written++
			continue
		}
		if errors.Is(err, errDuctingGridNotFound) {
			j.rememberMissing(st.run, now)
			gone++
			continue
		}
		j.logf("[ducting-grid] backfill run %s step f%03d failed: %v", cycleStamp(st.run), st.lead, err)
		if errors.Is(err, errDuctingGridPanic) || errors.Is(err, errDuctingGridBudget) || ctx.Err() != nil {
			break
		}
	}
	j.logf("[ducting-grid] backfill: %d steps written, %d remaining, %d MB",
		written, len(todo)-written-gone, (budget.used()-used0)/1e6)
}

// rememberMissing records a cycle the bucket does not have.
func (j *ductingGridJob) rememberMissing(run, now int64) {
	if j.missingCycles == nil {
		j.missingCycles = make(map[int64]int64)
	}
	j.missingCycles[run] = now
}

// forgetMissing drops entries older than ductingGridMissingRetry or
// outside the history window, which bounds the set to the window's cycles
// (historyDays*4 at most).
func (j *ductingGridJob) forgetMissing(now, first int64) {
	for c, at := range j.missingCycles {
		if now-at >= ductingGridMissingRetry || c < first {
			delete(j.missingCycles, c)
		}
	}
}

// pickCycle returns the newest cycle (of the current one and up to 3 earlier
// ones) whose last step is published in both files, with the steps of it
// not stored yet. ok is false when there is nothing to do: no cycle newer
// than the stored one, the stored one complete, or none published.
func (j *ductingGridJob) pickCycle(ctx context.Context, latest int64, budget *ductingGridBudget) (run int64, missing []int, ok bool) {
	now := j.now().Unix()
	newest := now - now%ductingGridCycleSeconds
	steps := j.steps()
	last := steps[len(steps)-1]
	for k := 0; k < ductingGridProbeCycles; k++ {
		run = newest - int64(k)*ductingGridCycleSeconds
		if run < latest {
			return 0, nil, false // older than what is stored
		}
		missing = steps
		if run == latest {
			leads, err := j.store.DuctingRunLeads(ductingGridModel, run)
			if err != nil {
				j.logf("[ducting-grid] reading stored steps of run %s: %v", cycleStamp(run), err)
				return 0, nil, false
			}
			missing = missingSteps(steps, leads)
			if len(missing) == 0 {
				return 0, nil, false // up to date
			}
		}
		published := true
		for file := range gfsFileKinds {
			if _, err := j.fetchIndex(ctx, run, last, file, budget); err != nil {
				if !errors.Is(err, errDuctingGridNotFound) {
					j.logf("[ducting-grid] probing run %s %s f%03d: %v", cycleStamp(run), gfsFileKinds[file], last, err)
				}
				published = false
				break
			}
		}
		if published {
			return run, missing, true
		}
		if ctx.Err() != nil {
			return 0, nil, false
		}
	}
	j.logf("[ducting-grid] no published GFS cycle newer than the stored one among the last %d", ductingGridProbeCycles)
	return 0, nil, false
}

// missingSteps returns the steps not in stored.
func missingSteps(steps, stored []int) []int {
	have := make(map[int]bool, len(stored))
	for _, s := range stored {
		have[s] = true
	}
	var out []int
	for _, s := range steps {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}

// ---- download ----

var (
	errDuctingGridNotFound = errors.New("not published")
	errDuctingGridBudget   = errors.New("download budget exceeded")
)

// ductingGridBudget counts the bytes a cycle downloads, across goroutines.
type ductingGridBudget struct {
	limit int64
	n     atomic.Int64
}

// take accounts n bytes; an error (and nothing taken) when it would pass the limit.
func (b *ductingGridBudget) take(n int64) error {
	for {
		cur := b.n.Load()
		if cur+n > b.limit {
			return fmt.Errorf("%w (%d MB)", errDuctingGridBudget, b.limit/1e6)
		}
		if b.n.CompareAndSwap(cur, cur+n) {
			return nil
		}
	}
}

func (b *ductingGridBudget) used() int64 { return b.n.Load() }

// fileURL is the object path of one GFS file (or its .idx) for a run and step.
func (j *ductingGridJob) fileURL(run int64, step, file int, idx bool) string {
	t := time.Unix(run, 0).UTC()
	hh := t.Format("15")
	u := fmt.Sprintf("%s/gfs.%s/%s/atmos/gfs.t%sz.%s.0p50.f%03d", j.baseURL, t.Format("20060102"), hh, hh, gfsFileKinds[file], step)
	if idx {
		u += ".idx"
	}
	return u
}

// get downloads u, or the inclusive byte range start..end of it (start >= 0;
// end < 0 means to the end of the object), reading at most limit bytes and
// accounting them in budget. Errors never contain the URL.
func (j *ductingGridJob) get(ctx context.Context, u string, start, end, limit int64, budget *ductingGridBudget) ([]byte, error) {
	ranged := start >= 0
	want := int64(-1)
	if ranged && end >= 0 {
		want = end - start + 1
		if want <= 0 || want > limit {
			return nil, fmt.Errorf("message of %d bytes exceeds the %d byte limit", want, limit)
		}
		// Reserve before downloading, so the budget is never overrun.
		if err := budget.take(want); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("building the request failed")
	}
	if ranged {
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		}
	}
	resp, err := j.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // *url.Error carries the URL
		}
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("redirect refused (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, ductingGridMaxErrBytes))
		return nil, fmt.Errorf("%w (HTTP %d)", errDuctingGridNotFound, resp.StatusCode)
	case resp.StatusCode >= 400:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, ductingGridMaxErrBytes))
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if ranged {
		// Anything but 206 with the requested range is refused before the
		// body is read: a server ignoring Range would send the whole file.
		if resp.StatusCode != http.StatusPartialContent {
			return nil, fmt.Errorf("byte range not honoured (HTTP %d, want 206)", resp.StatusCode)
		}
		cs, ce, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || cs != start || (end >= 0 && ce != end) {
			return nil, errors.New("Content-Range does not match the requested range")
		}
		if want < 0 {
			want = ce - cs + 1
			if want > limit {
				return nil, fmt.Errorf("message of %d bytes exceeds the %d byte limit", want, limit)
			}
			if err := budget.take(want); err != nil {
				return nil, err
			}
		}
	} else if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("body of %d bytes exceeds the %d byte limit", resp.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading the body: %v", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("body exceeds the %d byte limit", limit)
	}
	if want >= 0 {
		if int64(len(body)) != want {
			return nil, fmt.Errorf("body is %d bytes, range is %d", len(body), want)
		}
	} else if err := budget.take(int64(len(body))); err != nil {
		return nil, err
	}
	return body, nil
}

// parseContentRange parses "bytes start-end/total" (total may be "*").
func parseContentRange(h string) (start, end int64, ok bool) {
	rest, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, false
	}
	span, total, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, false
	}
	if total != "*" {
		t, err := strconv.ParseInt(total, 10, 64)
		if err != nil || t <= end {
			return 0, 0, false
		}
	}
	return start, end, true
}

// forecastLabel is the .idx forecast string of a step.
func forecastLabel(step int) string {
	if step == 0 {
		return "anl"
	}
	return strconv.Itoa(step) + " hour fcst"
}

// fetchIndex downloads and parses the .idx of one file; every line must be
// for the run's cycle.
func (j *ductingGridJob) fetchIndex(ctx context.Context, run int64, step, file int, budget *ductingGridBudget) ([]grib2.IndexEntry, error) {
	body, err := j.get(ctx, j.fileURL(run, step, file, true), -1, -1, j.maxIdx, budget)
	if err != nil {
		return nil, fmt.Errorf("%s index: %w", gfsFileKinds[file], err)
	}
	entries, err := grib2.ParseIndex(body)
	if err != nil {
		return nil, fmt.Errorf("%s index: %w", gfsFileKinds[file], err)
	}
	stamp := cycleStamp(run)
	for _, e := range entries {
		if e.RefDate != stamp {
			return nil, fmt.Errorf("%s index: message %d is for cycle %s, want %s", gfsFileKinds[file], e.Num, e.RefDate, stamp)
		}
	}
	return entries, nil
}

// ductingGridMsg is one planned download: slot, file and inclusive byte
// range (end -1: to the end of the file).
type ductingGridMsg struct {
	slot, file int
	start, end int64
}

// planStep maps every needed variable to exactly one .idx entry of its file
// with the step's forecast string.
func planStep(entries [len(gfsFileKinds)][]grib2.IndexEntry, step int) ([]ductingGridMsg, error) {
	fcst := forecastLabel(step)
	plan := make([]ductingGridMsg, 0, ductingGridSlots)
	for slot, v := range ductingGridVars {
		es := entries[v.File]
		found := -1
		other := ""
		for i, e := range es {
			if e.Variable != v.Name || e.Level != v.Level {
				continue
			}
			if e.Forecast != fcst {
				other = e.Forecast
				continue
			}
			if found >= 0 {
				return nil, fmt.Errorf("%s is listed twice in the %s index", v.label(), gfsFileKinds[v.File])
			}
			found = i
		}
		if found < 0 {
			if other != "" {
				return nil, fmt.Errorf("%s in the %s index has forecast %q, want %q", v.label(), gfsFileKinds[v.File], other, fcst)
			}
			return nil, fmt.Errorf("%s is missing from the %s index", v.label(), gfsFileKinds[v.File])
		}
		m := ductingGridMsg{slot: slot, file: v.File, end: -1}
		var next *grib2.IndexEntry
		if found+1 < len(es) {
			next = &es[found+1]
		}
		if next == nil {
			m.start = es[found].Offset
		} else {
			start, end, ok := es[found].Range(next, 0)
			if !ok {
				return nil, fmt.Errorf("%s has an invalid byte range in the %s index", v.label(), gfsFileKinds[v.File])
			}
			m.start, m.end = start, end
		}
		plan = append(plan, m)
	}
	return plan, nil
}

// ductingGridFields holds the decoded values of one step, by slot.
type ductingGridFields [ductingGridSlots][]float32

// fetchFields downloads, decodes and checks the planned messages,
// ductingGridFetchParallel at a time. The first failure cancels the rest.
func (j *ductingGridJob) fetchFields(ctx context.Context, run int64, step int, plan []ductingGridMsg, budget *ductingGridBudget) (*ductingGridFields, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		fields   ductingGridFields
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	work := make(chan ductingGridMsg)
	for w := 0; w < ductingGridFetchParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range work {
				if ctx.Err() != nil {
					continue
				}
				vals, err := j.fetchField(ctx, run, step, m, budget)
				if err != nil {
					fail(fmt.Errorf("%s: %w", ductingGridVars[m.slot].label(), err))
					continue
				}
				fields[m.slot] = vals
			}
		}()
	}
feed:
	for _, m := range plan {
		select {
		case work <- m:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &fields, nil
}

// fetchField downloads and decodes one message and checks it; a panic is
// returned as an error.
func (j *ductingGridJob) fetchField(ctx context.Context, run int64, step int, m ductingGridMsg, budget *ductingGridBudget) (vals []float32, err error) {
	defer recoverAsError(&err)
	msg, err := j.get(ctx, j.fileURL(run, step, m.file, false), m.start, m.end, j.maxMsg, budget)
	if err != nil {
		return nil, err
	}
	f, err := grib2.Decode(msg)
	if err != nil {
		return nil, err
	}
	if err := j.checkField(f, ductingGridVars[m.slot], run, step); err != nil {
		return nil, err
	}
	return f.Values, nil
}

func nearlyEqual(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

// checkField verifies that a decoded field is the one asked for, on the
// pinned lattice, with values in the variable's physical range. NaN is a
// point the decoder marked missing (bitmap or missing-value management);
// it is kept and the physics treats it as missing.
func (j *ductingGridJob) checkField(f *grib2.Field, v ductingGridVar, run int64, step int) error {
	l := j.lattice
	if f.Ni != l.Ni || f.Nj != l.Nj || !nearlyEqual(f.La1, l.La1) || !nearlyEqual(f.Lo1, l.Lo1) ||
		!nearlyEqual(f.DLat, l.DLat) || !nearlyEqual(f.DLon, l.DLon) {
		return fmt.Errorf("lattice %dx%d from %g,%g step %g,%g, want %dx%d from %g,%g step %g,%g",
			f.Ni, f.Nj, f.La1, f.Lo1, f.DLat, f.DLon, l.Ni, l.Nj, l.La1, l.Lo1, l.DLat, l.DLon)
	}
	if len(f.Values) != l.Ni*l.Nj {
		return fmt.Errorf("%d values for %d points", len(f.Values), l.Ni*l.Nj)
	}
	if f.RefTime != run {
		return fmt.Errorf("reference time %s, want %s", cycleStamp(f.RefTime), cycleStamp(run))
	}
	if f.ForecastHour != step {
		return fmt.Errorf("forecast hour %d, want %d", f.ForecastHour, step)
	}
	if f.Discipline != 0 || f.ParameterCategory != v.Category || f.ParameterNumber != v.Number {
		return fmt.Errorf("parameter %d.%d.%d, want 0.%d.%d", f.Discipline, f.ParameterCategory, f.ParameterNumber, v.Category, v.Number)
	}
	if f.SurfaceType != v.SurfType || (v.SurfType != gribSurfaceGround && !nearlyEqual(f.SurfaceValue, v.SurfValue)) {
		return fmt.Errorf("level type %d value %g, want type %d value %g", f.SurfaceType, f.SurfaceValue, v.SurfType, v.SurfValue)
	}
	for i, x := range f.Values {
		if x != x { // NaN: missing point
			continue
		}
		if fx := float64(x); fx < v.Lo || fx > v.Hi {
			return fmt.Errorf("value %g at point %d outside %g..%g", fx, i, v.Lo, v.Hi)
		}
	}
	return nil
}

// ---- compute ----

// buildStep downloads one step, computes every lattice point and returns
// the encoded grid.
func (j *ductingGridJob) buildStep(ctx context.Context, run int64, step int, budget *ductingGridBudget) ([]byte, error) {
	var entries [len(gfsFileKinds)][]grib2.IndexEntry
	for file := range gfsFileKinds {
		es, err := j.fetchIndex(ctx, run, step, file, budget)
		if err != nil {
			return nil, err
		}
		entries[file] = es
	}
	plan, err := planStep(entries, step)
	if err != nil {
		return nil, err
	}
	fields, err := j.fetchFields(ctx, run, step, plan, budget)
	if err != nil {
		return nil, err
	}
	l := j.lattice
	g, err := ducting.NewGrid(ductingGridModel, run, run+int64(step)*3600, l.La1, l.Lo1, l.DLat, l.DLon, l.Ni, l.Nj)
	if err != nil {
		return nil, err
	}
	if err := j.computeGrid(ctx, fields, g); err != nil {
		return nil, err
	}
	*fields = ductingGridFields{} // release ~45 MB before encoding
	return ducting.Encode(g)
}

// computeGrid runs the physics for every lattice point, rows split into
// chunks over j.workers goroutines. Each worker reuses its level arrays;
// points are disjoint, so the grid needs no lock.
func (j *ductingGridJob) computeGrid(ctx context.Context, fields *ductingGridFields, g *ducting.Grid) error {
	workers := max(1, j.workers)
	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := j.computeRows(ctx, fields, g, &next)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// computeRows takes row chunks from next until none are left; a panic
// is returned as an error.
func (j *ductingGridJob) computeRows(ctx context.Context, fields *ductingGridFields, g *ducting.Grid, next *atomic.Int64) (err error) {
	defer recoverAsError(&err)
	const nl = len(ductingGridLevelsHPa)
	levels := ductingGridLevelsHPa[:]
	temps := make([]float64, nl)
	rhs := make([]float64, nl)
	hgts := make([]float64, nl)
	var sc ducting.Scratch
	f := fields
	for {
		r0 := int(next.Add(ductingGridRowChunk)) - ductingGridRowChunk
		if r0 >= g.Nj {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		r1 := min(r0+ductingGridRowChunk, g.Nj)
		for idx := r0 * g.Ni; idx < r1*g.Ni; idx++ {
			for i := 0; i < nl; i++ {
				temps[i] = float64(f[slotTmp(i)][idx]) - 273.15
				rhs[i] = float64(f[slotRh(i)][idx])
				hgts[i] = float64(f[slotHgt(i)][idx])
			}
			p := sc.Profile(float64(f[slotHgtSfc][idx]), float64(f[slotPresSfc][idx])/100,
				float64(f[slotTmp2m][idx])-273.15, float64(f[slotRh2m][idx]), levels, temps, rhs, hgts)
			if j.analyze != nil {
				g.Set(idx, j.analyze(p))
			} else {
				g.Set(idx, sc.Analyze(p))
			}
		}
	}
}
