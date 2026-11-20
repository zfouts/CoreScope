// Package grib2 decodes the subset of WMO GRIB edition 2 that NOAA's GFS
// pressure-level files use, and parses their wgrib2-style .idx inventories.
//
// Supported: section 3 grid definition template 3.0 (regular lat/lon),
// section 5 data representation templates 5.0 (simple packing), 5.2 (complex
// packing) and 5.3 (complex packing with spatial differencing, order 1 or 2),
// section 6 bitmap indicator 255 (none) or 0 (bitmap in this message).
// Anything else is an error (ErrUnsupported), never a panic.
//
// The input is untrusted (downloaded from the internet). Every length,
// count and bit width read from the message is checked against the limits
// below and against the bytes actually present before it is used to
// allocate or index.
//
// Functions implemented in this package:
//
//	func Decode(msg []byte) (*Field, error)           // exactly one GRIB2 message
//	func ParseIndex(idx []byte) ([]IndexEntry, error) // .idx text inventory
//	func (e IndexEntry) Range(next *IndexEntry, fileSize int64) (start, end int64, ok bool)
package grib2

import "errors"

// Limits for untrusted input.
const (
	MaxMessageBytes = 64 << 20  // a single message larger than this is refused
	MaxPoints       = 1440 * 721 // ni*nj above this is refused (0.25° worldwide)
	MaxBitsPerValue = 32         // packing widths above this are refused
	MaxGroups       = MaxPoints  // complex packing group count above this is refused
	MaxIndexLines   = 10000      // .idx inventories longer than this are refused
)

var (
	ErrUnsupported = errors.New("grib2: unsupported template or feature")
	ErrCorrupt     = errors.New("grib2: corrupt or truncated message")
	ErrTooLarge    = errors.New("grib2: message exceeds limits")
)

// Field is one decoded message on a regular lat/lon grid.
// Values[r*Ni + c] is the point at lat = La1 + r*DLat, lon = Lo1 + c*DLon
// (DLat is negative when the scan runs north to south, as in GFS). Missing
// points (bitmap off) are NaN.
type Field struct {
	Discipline        uint8
	ParameterCategory uint8
	ParameterNumber   uint8
	// Level: first fixed surface type and its value in SI units (type 100 =
	// isobaric, value in Pa; 1 = ground; 103 = height above ground in m).
	SurfaceType  uint8
	SurfaceValue float64

	RefTime      int64 // unix seconds (section 1 reference time)
	ForecastHour int   // lead time in hours (template 4.0 forecast time; units converted)

	Ni, Nj     int
	La1, Lo1   float64 // degrees
	DLat, DLon float64 // degrees, signed by scanning mode
	Values     []float32
}

// IndexEntry is one line of a wgrib2 .idx inventory, e.g.
// "312:204528563:d=2026111918:TMP:850 mb:6 hour fcst:".
type IndexEntry struct {
	Num      int    // message number
	Offset   int64  // byte offset of the message in the file
	RefDate  string // "2026111918"
	Variable string // "TMP"
	Level    string // "850 mb", "surface", "2 m above ground"
	Forecast string // "6 hour fcst", "anl"
}
