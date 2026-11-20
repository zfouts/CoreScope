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
	ductingGridMaxCells     = 4000   // cells per /api/ducting/grid answer
	ductingLinksMaxKm       = 1000   // receptions over this distance are treated as bad positions
	ductingLinksMaxPairs    = 500    // node pairs per /api/ducting/links answer, longest first
	ductingLinksMaxRecv     = 50     // receptions listed per pair, newest first
	ductingLinksPathKm      = 25.0   // path sample spacing
	ductingLinksStepWindowS = 5400   // a reception uses the grid step within ±90 min of it
	ductingLinksDuctFrac    = 0.5    // "during a duct": at least this share of path samples carry LoRa
	ductingLinksMaxSamples  = 64     // path samples per reception at most
	ductingLinksCacheTTL    = 5 * 60 // seconds a computed links answer is reused (same days, data generation, run, visibility)
	ductingLinksMaxMemo     = 5000   // per-reception results remembered across rebuilds, keyed by (observation, run_time)
	ductingGridLRUSteps     = 8      // decoded steps kept in memory (~3.4 MB each at 0.5°)
	ductingGridMaxAgeS      = 600    // Cache-Control max-age of /api/ducting/grid
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

// DuctingLinksSummary compares long-link receptions with how often the same
// paths are ducted at all.
type DuctingLinksSummary struct {
	Links      int `json:"links"`      // distinct node pairs (before the ductingLinksMaxPairs cap)
	Receptions int `json:"receptions"` // receptions over all pairs
	WithGrid   int `json:"withGrid"`   // receptions with a stored step within ±90 min
	DuringDuct int `json:"duringDuct"` // of WithGrid: path duct share >= ductingLinksDuctFrac
	// BaselineDuctFrac: over every stored step in the period and every pair,
	// the share of (pair, step) combinations whose path duct share >=
	// ductingLinksDuctFrac. -1 when there is no data.
	BaselineDuctFrac float64 `json:"baselineDuctFrac"`
}

// DuctingLink is one node pair: A transmitted the advert, B (an observer
// that is also a node) heard it with no hops in between.
type DuctingLink struct {
	A          DuctingLinkEnd     `json:"a"`
	B          DuctingLinkEnd     `json:"b"`
	DistKm     float64            `json:"distKm"`
	HorizonKm  float64            `json:"horizonKm"` // 4.12 * (sqrt(hA) + sqrt(hB))
	Count      int                `json:"count"`     // receptions in the period
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
	T         int64    `json:"t"`
	Via       string   `json:"via"`           // "observer": zero-hop advert heard by an observer node; "relay": consecutive certain hops of a flood path
	SNR       *float64 `json:"snr,omitempty"` // observer receptions only (a path's SNR is the last hop's link to the observer)
	RSSI      *float64 `json:"rssi,omitempty"`
	DuctFrac  float64  `json:"ductFrac"`  // share of path samples whose duct can carry FreqMHz; -1 = no stored step
	SuperFrac float64  `json:"superFrac"` // share of path samples that are super-refractive; -1 = no stored step
}
