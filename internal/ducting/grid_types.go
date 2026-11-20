package ducting

// Stored grid contract. One Grid is one analysed time step on a regular
// lat/lon lattice; the ingestor writes Encode(grid) as one ducting_grid row
// per (model, valid_time) and the server reads it back with Decode.
//
// Functions implemented in codec.go / grid.go / window.go / path.go:
//
//	func NewGrid(model string, runTime, validTime int64, lat0, lon0, dLat, dLon float64, ni, nj int) (*Grid, error)
//	func (g *Grid) Set(idx int, r Result)                 // rounds/clamps r into the planar arrays
//	func (g *Grid) LatLon(idx int) (lat, lon float64)     // lon normalised to [-180, 180)
//	func (g *Grid) Nearest(lat, lon float64) (int, bool)  // nearest lattice point; lon wrapped; false outside the lattice
//	func Encode(g *Grid) ([]byte, error)
//	func Decode(b []byte) (*Grid, error)                  // enforces the limits below; never panics
//	func (g *Grid) Window(south, west, north, east float64, maxCells int) Window
//	func (g *Grid) PathStats(lat1, lon1, lat2, lon2, freqMHz, spacingKm float64) PathStats
//	func CarriesLoRa(class uint8, fminMHz uint16, freqMHz float64) bool
//
// Encoded format (version 1), all little-endian:
//
//	"CSDG"            4 bytes magic
//	version           uint8 (=1)
//	modelLen          uint8, then model bytes (1..32, [a-z0-9-])
//	ni, nj            uint32 each
//	lat0, lon0        float64 each
//	dLat, dLon        float64 each (dLon > 0; dLat != 0, GFS rows run north to south so dLat < 0)
//	runTime, validTime int64 each (unix seconds)
//	payload           raw DEFLATE (compress/flate) of the planar arrays in the
//	                  order C, F, B, T, DM, G, N; exactly ni*nj*13 bytes inflated
const (
	GridMagic   = "CSDG"
	GridVersion = 1
	// GridMaxPoints bounds ni*nj on Decode and NewGrid: 0.25° worldwide
	// (1440 x 721) is the largest lattice we accept.
	GridMaxPoints = 1440 * 721
	// GridBytesPerPoint is the inflated payload size per lattice point:
	// C 1 + F 2 + B 2 + T 2 + DM 2 + G 2 + N 2.
	GridBytesPerPoint = 13
)

// Grid is one analysed time step. Index of point (row r, col c) is r*Ni + c;
// its position is lat = Lat0 + r*DLat, lon = Lon0 + c*DLon. Every slice has
// length Ni*Nj.
type Grid struct {
	Model     string
	RunTime   int64 // unix seconds of the model cycle
	ValidTime int64 // unix seconds
	Lat0      float64
	Lon0      float64
	DLat      float64
	DLon      float64
	Ni        int // columns
	Nj        int // rows

	C  []uint8  // class (Class*)
	F  []uint16 // fmin MHz, rounded, clamped to 65535; 0 = no duct
	B  []uint16 // duct base m AGL, rounded, clamped
	T  []uint16 // duct top m AGL, rounded, clamped
	DM []uint16 // ΔM x10, rounded, clamped
	G  []int16  // min dN/dh N/km, rounded, clamped to int16
	N  []int16  // mean dN/dh over the lowest 1.5 km, N/km, rounded; 0 = no data
}

// WindowCell is one aggregated cell of a Window: the square
// [i*Step, (i+1)*Step] x [j*Step, (j+1)*Step] (edges inclusive), Lat/Lon its
// centre ((i+0.5)*Step, (j+0.5)*Step). Aggregation over the lattice points
// inside the square:
//
//	C  = largest class (severity order)
//	F, B, T, DM = from the point with the lowest non-zero F among points with
//	             C >= ClassElev; all 0 when there is none
//	G  = minimum G over points with C >= ClassNormal
//	N  = mean of the non-zero N, rounded; 0 when all are 0
type WindowCell struct {
	Lat, Lon float64
	C        uint8
	F, B, T  uint16
	DM       uint16
	G, N     int16
}

// Window is the grid cut to a bounding box at a step coarse enough that it
// holds at most maxCells cells. Step is the smallest of 1, 2, 4, 8, 16
// times |DLon| that fits (never finer than the lattice). Only cells with
// C >= ClassNormal are returned. Longitudes are in [-180, 180).
type Window struct {
	Step  float64
	Cells []WindowCell
}

// PathStats samples the lattice along the great circle between two points,
// every spacingKm (at least the two end points), using the nearest lattice
// point for each sample.
type PathStats struct {
	Samples int // samples taken
	NoData  int // samples whose point has C == ClassNoData (or is outside the lattice)
	Duct    int // samples where CarriesLoRa(C, F, freqMHz)
	Super   int // samples with C == ClassSuper
	// SuperOrDuct counts samples with C >= ClassSuper: super-refractive or
	// ducting by the class, which a single thin layer at or below
	// SuperGradientNk decides (common over humid land).
	SuperOrDuct int
	// Enhanced counts samples whose N (mean dN/dh over the lowest MeanLayerM)
	// is non-zero and at or below HighGradientNk: the map's "High" band or
	// stronger. 0 without an N plane.
	Enhanced int
	// MeanN is the mean of the non-zero N (mean dN/dh, N/km) over the
	// samples with data; NCount is how many samples it averages (N = 0 is
	// "no value" in the grid). 0 and 0 when there are none, or when the
	// grid carries no N plane (Decoder without N).
	MeanN  float64
	NCount int
}
