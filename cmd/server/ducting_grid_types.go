package main

// Global ducting grid (ducting.source "gfs-grid"): server side contract.
//
// Data flow:
//   ingestor: NOAA GFS 0.5° (internal/grib2) -> physics per point
//     (internal/ducting) -> ducting.Encode -> table ducting_grid, one row
//     per (model, valid_time), kept for historyDays
//   server (read-only): ducting_grid -> ducting.Decode (LRU) ->
//     GET /api/ducting/meta   DuctingMetaResponse
//     GET /api/ducting/grid   DuctingResponse with one time (the existing
//                             cell format, so the browser renderer is reused)
//     GET /api/ducting/links  DuctingLinksResponse
//
// Table (both backends; SQLite types shown, PostgreSQL BIGINT/BYTEA):
//   ducting_grid(model TEXT NOT NULL, valid_time INTEGER NOT NULL,
//                run_time INTEGER NOT NULL, lead_hours INTEGER NOT NULL,
//                data BLOB NOT NULL, created_at INTEGER NOT NULL,
//                PRIMARY KEY (model, valid_time))
//   All times are unix seconds. A row is replaced only by a newer run_time.

import ducting "github.com/meshcore-analyzer/ducting"

// Fixed limits of the grid endpoints (customizer/config later: see the PR's
// follow-up list).
const (
	ductingGridModel        = "gfs"
	ductingGridMaxCells     = 4000     // cells per /api/ducting/grid answer
	ductingLinksMaxKm       = 1000     // receptions over this distance are treated as bad positions
	ductingLinksMaxPairs    = 500      // node pairs per /api/ducting/links answer, longest first
	ductingLinksRecvBucketS = 3 * 3600 // receptions listed per pair: the newest per stored step (or per 3 h UTC bucket without one)
	ductingLinksRecvPerDay  = 8        // ... and at most this many per day of history
	ductingLinksPathKm      = 25.0     // path sample spacing
	ductingLinksStepWindowS = 5400     // a reception uses the grid step within ±90 min of it
	ductingLinksDuctFrac    = 0.5      // "during a duct": at least this share of path samples carry LoRa
	ductingLinksMaxSamples  = 64       // path samples per reception at most
	ductingLinksCacheTTL    = 5 * 60   // seconds a computed links answer is reused (same days, data generation, run, visibility)
	ductingLinksMaxMemo     = 5000     // per-reception results remembered across rebuilds, keyed by (observation, run_time)
	ductingGridLRUSteps     = 8        // decoded steps kept in memory (~3.4 MB each at 0.5°)
	ductingGridMaxAgeS      = 600      // Cache-Control max-age of /api/ducting/grid
	ductingHistoryDaysMax   = 30
)

// ductingGridRow is one ducting_grid row without its blob.
type ductingGridRow struct {
	ValidTime int64
	RunTime   int64
	LeadHours int
	CreatedAt int64
}

// ductingStepSource gives decoded grid steps. Implemented by
// ductingGridService (LRU over the DB); tests substitute a fake.
type ductingStepSource interface {
	// Rows returns the stored rows of the model, ascending by ValidTime
	// (cached; refreshed at most once a minute).
	Rows() ([]ductingGridRow, error)
	// Step returns the decoded grid for an exact stored valid time, through
	// the LRU (ductingGridLRUSteps entries, singleflight per valid time).
	Step(validTime int64) (*ducting.Grid, error)
	// Load decodes a stored valid time without touching the LRU, for bulk
	// scans (the long-link analysis) that would otherwise evict the steps
	// the map is showing.
	Load(validTime int64) (*ducting.Grid, error)
}

// ductingPlaneLoader is an optional ductingStepSource capability: Load that
// decodes only the C and F planes into a caller-owned, reusable Decoder.
type ductingPlaneLoader interface {
	LoadPlanes(validTime int64, d *ducting.Decoder) (*ducting.Grid, error)
}

// DuctingMetaResponse is GET /api/ducting/meta.
type DuctingMetaResponse struct {
	Status        string  `json:"status"` // "ok" | "pending" (no rows yet) | "error"
	Error         string  `json:"error,omitempty"`
	Source        string  `json:"source"` // "gfs-grid"
	Model         string  `json:"model"`
	ModelLabel    string  `json:"modelLabel"`
	Attribution   string  `json:"attribution"`
	NativeStepDeg float64 `json:"nativeStepDeg"`
	RunTime       int64   `json:"runTime"`     // newest model cycle stored
	GeneratedAt   int64   `json:"generatedAt"` // created_at of the newest run
	HistoryDays   int     `json:"historyDays"`
	Times         []int64 `json:"times"` // every stored valid time, ascending (past and forecast)
}

// DuctingLinksResponse is GET /api/ducting/links: zero-hop adverts heard
// directly over more than the radio horizon, with the modelled ducting
// along the path at the time of each reception.
type DuctingLinksResponse struct {
	Status      string              `json:"status"` // "ok" | "pending" | "error"
	Error       string              `json:"error,omitempty"`
	FreqMHz     float64             `json:"freqMHz"` // frequency the duct test used
	Days        int                 `json:"days"`
	GeneratedAt int64               `json:"generatedAt"`
	Summary     DuctingLinksSummary `json:"summary"`
	Links       []DuctingLink       `json:"links"`
}

// DuctingLinksSummary compares long-link receptions with how the same paths
// are refracted at all. A pair is listed only when it is beyond line of
// sight (LosMethod says how that was decided).
type DuctingLinksSummary struct {
	Links int `json:"links"` // distinct beyond-line-of-sight node pairs (before the ductingLinksMaxPairs cap)
	// ListedPairs is how many of them are listed (the ductingLinksMaxPairs
	// longest). Every statistic below is over the listed pairs only, so the
	// receptions and the baseline describe the same paths.
	ListedPairs int `json:"listedPairs"`
	Receptions  int `json:"receptions"` // receptions over the listed pairs
	WithGrid    int `json:"withGrid"`   // of Receptions: with a stored step within ±90 min and data along the path
	DuringDuct  int `json:"duringDuct"` // of WithGrid: path duct share >= ductingLinksDuctFrac
	// BaselineDuctFrac: over every stored step in the period and every listed
	// pair, the share of (pair, step) combinations whose path duct share >=
	// ductingLinksDuctFrac. -1 when there is no data.
	BaselineDuctFrac float64 `json:"baselineDuctFrac"`
	// RecvMeanGradNkm: mean, over the receptions (of the listed pairs) with a path mean dN/dh, of that
	// mean (N/km; more negative bends more). BaselineMeanGradNkm: the same
	// over the (pair, step) combinations of BaselineDuctFrac. nil: no data.
	RecvMeanGradNkm     *float64 `json:"recvMeanGradNkm"`
	BaselineMeanGradNkm *float64 `json:"baselineMeanGradNkm"`
	// RecvEnhancedFrac: of WithGrid, the share of receptions whose path
	// highFrac >= ductingLinksDuctFrac (at least half the path in the map's
	// High band or stronger); BaselineEnhancedFrac: the same over the
	// baseline combinations. -1 when there is no data.
	RecvEnhancedFrac     float64 `json:"recvEnhancedFrac"`
	BaselineEnhancedFrac float64 `json:"baselineEnhancedFrac"`
	// LosMethod: "terrain" (SRTM profile at los.kFactor; endpoint elevations
	// past the profile leg limit) or "smooth-earth" (antenna heights only,
	// k = 4/3; LoS terrain not enabled).
	LosMethod string `json:"losMethod"`
	// UnverifiedPairs: pairs past the antenna-height horizon whose terrain
	// check has not run yet (per-rebuild budget, tiles loading) or had no
	// terrain data; they are left out until verified.
	UnverifiedPairs int `json:"unverifiedPairs"`
}

// DuctingLink is one node pair: A transmitted, B heard it directly (a
// zero-hop advert at an observer node, or consecutive certain hops of a
// flood path).
type DuctingLink struct {
	A         DuctingLinkEnd `json:"a"`
	B         DuctingLinkEnd `json:"b"`
	DistKm    float64        `json:"distKm"`
	HorizonKm float64        `json:"horizonKm"` // 4.12 * (sqrt(hA) + sqrt(hB))
	Count     int            `json:"count"`     // all receptions in the period
	// Receptions, newest first, thinned by time: the newest per stored grid
	// step (per 3 h UTC bucket for one without a step), at most
	// ductingLinksRecvPerDay per day of history.
	Receptions []DuctingReception `json:"receptions"`
}

// DuctingLinkEnd is one end of a link.
type DuctingLinkEnd struct {
	Pubkey      string  `json:"pubkey"`
	Name        string  `json:"name"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	HeightM     float64 `json:"heightM"`
	HeightKnown bool    `json:"heightKnown"` // false: the configured default antenna height was used
}

// DuctingReception is one reception of a link.
type DuctingReception struct {
	T    int64    `json:"t"`
	Via  string   `json:"via"`           // "observer": zero-hop advert heard by an observer node; "relay": consecutive certain hops of a flood path
	SNR  *float64 `json:"snr,omitempty"` // observer receptions only (a path's SNR is the last hop's link to the observer)
	RSSI *float64 `json:"rssi,omitempty"`
	// The shares are of the path samples with data (Samples - NoData); -1
	// for all three when there is no stored step or no sample has data.
	DuctFrac  float64 `json:"ductFrac"`  // in a duct whose lowest trapped frequency is <= FreqMHz
	SuperFrac float64 `json:"superFrac"` // class super-refractive or ducting (C >= ClassSuper: some layer at or below -79 N/km)
	// HighFrac: mean dN/dh over the lowest 1.5 km at or below
	// ducting.HighGradientNk (-62 N/km, where the map legend's High band
	// starts); a sample without a value counts as not high.
	HighFrac float64 `json:"highFrac"`
	// MeanGradNkm is the mean dN/dh (N/km) along the path; nil without a
	// stored step or without values along the path.
	MeanGradNkm *float64 `json:"meanGradNkm"`
}
