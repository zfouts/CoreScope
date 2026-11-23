package main

import "github.com/meshcore-analyzer/ducting"

// Tropospheric ducting overlay: shared types (the contract between the
// physics, the config, the Open-Meteo fetcher/refresher and the frontend).
//
// Data flow:
//   config `ducting` (DuctingConfig, off by default)
//     -> grid cells around the mesh's nodes (ducting_grid.go)
//     -> Open-Meteo pressure-level forecast per cell (ducting_source.go)
//     -> per cell and hour: ducting.Profile -> ducting.Analyze (internal/ducting)
//     -> in-memory snapshot, served read-only as GET /api/ducting
//        (DuctingResponse). Nothing is written to any database.
//
// Physics (internal/ducting/calc.go):
//   e  = RH/100 * 6.1121 * exp(17.502*t / (240.97 + t))     hPa, t in °C
//   N  = 77.6/T * (P + 4810*e/T)                            ITU-R P.453, P,e hPa, T K
//   M  = N + 0.157*h                                        h metres above ground
//   trapping layer: consecutive levels where M decreases with height
//   duct thickness d (m) and strength ΔM (M units) -> lowest trapped frequency
//   fmin = 120000 / (d * sqrt(ΔM))  MHz   (λmax ≈ 0.0025 * d * sqrt(ΔM) m)

// Duct classes (DuctResult.Class, DuctingCell.C values); see internal/ducting.
const (
	DuctClassNoData  = ducting.ClassNoData
	DuctClassNormal  = ducting.ClassNormal
	DuctClassSuper   = ducting.ClassSuper
	DuctClassElev    = ducting.ClassElev
	DuctClassSurface = ducting.ClassSurface
)

// DuctLevel and DuctResult are the shared physics types (internal/ducting).
type (
	DuctLevel  = ducting.Level
	DuctResult = ducting.Result
)

// DuctingModel describes one allowed weather model. Regional models must be
// explicit opt-in: they are never the default, and RegionalNote says where
// they have data.
type DuctingModel struct {
	ID           string    // config value: "gfs", "icon", "rrfs-conus"
	OpenMeteoID  string    // Open-Meteo `models=` value
	Label        string    // shown in the UI
	LevelsHPa    []float64 // pressure levels requested (lowest ~3 km)
	Global       bool
	RegionalNote string // "" for global models
	// Bounds where a regional model has data: south, west, north, east.
	Bounds *[4]float64
}

// DuctingConfig is the `ducting` config section (config.json). Zero values
// mean "default"; see (*DuctingConfig).resolved() in ducting_config.go.
type DuctingConfig struct {
	Enabled        bool        `json:"enabled"`
	Model          string      `json:"model,omitempty"`          // default "gfs"
	Area           *[4]float64 `json:"area,omitempty"`           // [south, west, north, east]; nil = bounding box of the nodes + marginDeg
	MarginDeg      *float64    `json:"marginDeg,omitempty"`      // default 5, allowed 0..10; 0 = just the nodes' bounding box; ignored with area
	GridStepDeg    float64     `json:"gridStepDeg,omitempty"`    // default 0.5, allowed 0.25..2
	MaxPoints      int         `json:"maxPoints,omitempty"`      // default 450, allowed 1..1000
	RefreshMinutes int         `json:"refreshMinutes,omitempty"` // default 360, allowed 60..1440
	ForecastHours  int         `json:"forecastHours,omitempty"`  // default 48, allowed 1..120
	BaseURL        string      `json:"baseUrl,omitempty"`        // default https://api.open-meteo.com

	// Source picks where the field comes from: "gfs-grid" (default) is the
	// global grid the ingestor computes from NOAA GFS and stores in the
	// ducting_grid table (ducting_grid_types.go); "open-meteo" is the
	// per-point Open-Meteo fetch above (model, area, marginDeg, gridStepDeg,
	// maxPoints, refreshMinutes, forecastHours and baseUrl apply to it only).
	Source string `json:"source,omitempty"`
	// HistoryDays is how many days of past grids the ingestor keeps and the
	// time slider offers (gfs-grid only). Default 7, allowed 1..30.
	HistoryDays int `json:"historyDays,omitempty"`
	// GridBaseURL is the NOAA GFS bucket (or a mirror of it) the ingestor
	// downloads from (gfs-grid only). Default
	// https://noaa-gfs-bdp-pds.s3.amazonaws.com. The server never reads it.
	GridBaseURL string `json:"gridBaseUrl,omitempty"`
}

// DuctingClientConfig is the `ducting` field of /api/config/client; nil
// (omitted) when the feature is off. Never carries baseUrl.
type DuctingClientConfig struct {
	Source      string  `json:"source"` // "gfs-grid" (/api/ducting/meta + /api/ducting/grid) | "open-meteo" (/api/ducting)
	Model       string  `json:"model"`
	ModelLabel  string  `json:"modelLabel"`
	StepDeg     float64 `json:"stepDeg"`
	Attribution string  `json:"attribution"`
	HistoryDays int     `json:"historyDays,omitempty"` // gfs-grid only
}

// DuctingCell is one grid cell in GET /api/ducting. Every array has one entry
// per DuctingResponse.Times entry. Integers keep the payload small.
type DuctingCell struct {
	Lat float64 `json:"lat"` // cell centre
	Lon float64 `json:"lon"`
	C   []int   `json:"c"`  // class (DuctClass*); []int, not []uint8, which encodes as base64
	F   []int   `json:"f"`  // fmin MHz, rounded; 0 = no duct
	B   []int   `json:"b"`  // duct base m AGL, rounded
	T   []int   `json:"t"`  // duct top m AGL, rounded
	DM  []int   `json:"dm"` // ΔM x10, rounded
	G   []int   `json:"g"`  // min dN/dh N/km, rounded
	N   []int   `json:"n"`  // mean dN/dh over the lowest 1.5 km (high terrain: down to 1 km), N/km, rounded; 0 = no data
}

// DuctingResponse is GET /api/ducting.
type DuctingResponse struct {
	Status      string        `json:"status"`          // "ok" | "pending" (no data yet) | "error"
	Error       string        `json:"error,omitempty"` // short, operator-safe; never a URL
	Model       string        `json:"model"`
	ModelLabel  string        `json:"modelLabel"`
	Attribution string        `json:"attribution"`
	GeneratedAt int64         `json:"generatedAt"` // unix seconds of the last successful refresh; 0 if none
	StepDeg     float64       `json:"stepDeg"`
	Times       []int64       `json:"times"` // unix seconds, hourly, ascending
	Cells       []DuctingCell `json:"cells"`
	// Carried is how many of Cells are not from this refresh: their batch
	// failed, and they hold the previous forecast's values (an older model
	// run) for the hours it covers. GeneratedAt is the new fetch all the
	// same. Omitted when 0.
	Carried int `json:"carried,omitempty"`
}
