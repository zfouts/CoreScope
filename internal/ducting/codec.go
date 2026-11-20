package ducting

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// decodeMaxInput bounds the encoded size Decode accepts. A 0.25° worldwide
// grid of smooth fields deflates to well under 2 MiB.
const decodeMaxInput = 8 << 20

// gridHeaderFixed is the header size after the model bytes:
// ni, nj (4 each) + lat0, lon0, dLat, dLon (8 each) + runTime, validTime (8 each).
const gridHeaderFixed = 4 + 4 + 4*8 + 2*8

// lonSpanTol is the slack, in degrees, for comparing a lattice's longitude
// span with 360 (the GFS 0.5° lattice spans exactly 720 x 0.5).
const lonSpanTol = 1e-6

func validModel(m string) bool {
	if len(m) < 1 || len(m) > 32 {
		return false
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// checkLattice validates a grid header. Besides the documented contract
// (model, dims, finite spacing, dLon > 0, dLat != 0) it rejects lattices
// that cannot be a lat/lon lattice at all: rows outside [-90, 90], a
// longitude span over 360° (columns overlapping themselves), or lon0
// outside [-360, 360]. Those bounds keep every index computation in
// Nearest, Window and PathStats small and finite, so a corrupt header can
// never make them loop or allocate without limit.
func checkLattice(model string, lat0, lon0, dLat, dLon float64, ni, nj int64) error {
	if !validModel(model) {
		return errors.New("ducting: grid model must be 1..32 chars of [a-z0-9-]")
	}
	if ni < 1 || nj < 1 {
		return fmt.Errorf("ducting: grid dims %dx%d: must be >= 1", ni, nj)
	}
	if ni > GridMaxPoints || nj > GridMaxPoints || ni*nj > GridMaxPoints {
		return fmt.Errorf("ducting: grid dims %dx%d exceed %d points", ni, nj, GridMaxPoints)
	}
	if !isFinite(lat0) || !isFinite(lon0) || !isFinite(dLat) || !isFinite(dLon) {
		return errors.New("ducting: grid origin and spacing must be finite")
	}
	if !(dLon > 0) || dLat == 0 {
		return errors.New("ducting: grid spacing needs dLon > 0 and dLat != 0")
	}
	lastLat := lat0 + float64(nj-1)*dLat
	if lat0 < -90 || lat0 > 90 || !isFinite(lastLat) || lastLat < -90-lonSpanTol || lastLat > 90+lonSpanTol {
		return errors.New("ducting: grid rows outside [-90, 90]")
	}
	if lon0 < -360 || lon0 > 360 || float64(ni)*dLon > 360+lonSpanTol {
		return errors.New("ducting: grid columns outside one turn of longitude")
	}
	return nil
}

// NewGrid returns an empty grid (every point ClassNoData) on the given
// lattice. See Grid for the lattice convention.
func NewGrid(model string, runTime, validTime int64, lat0, lon0, dLat, dLon float64, ni, nj int) (*Grid, error) {
	if err := checkLattice(model, lat0, lon0, dLat, dLon, int64(ni), int64(nj)); err != nil {
		return nil, err
	}
	n := ni * nj
	return &Grid{
		Model: model, RunTime: runTime, ValidTime: validTime,
		Lat0: lat0, Lon0: lon0, DLat: dLat, DLon: dLon, Ni: ni, Nj: nj,
		C:  make([]uint8, n),
		F:  make([]uint16, n),
		B:  make([]uint16, n),
		T:  make([]uint16, n),
		DM: make([]uint16, n),
		G:  make([]int16, n),
		N:  make([]int16, n),
	}, nil
}

// Encode serialises g in the version 1 format documented in grid_types.go.
func Encode(g *Grid) ([]byte, error) {
	if g == nil {
		return nil, errors.New("ducting: encode nil grid")
	}
	if err := checkLattice(g.Model, g.Lat0, g.Lon0, g.DLat, g.DLon, int64(g.Ni), int64(g.Nj)); err != nil {
		return nil, err
	}
	n := g.Ni * g.Nj
	if len(g.C) != n || len(g.F) != n || len(g.B) != n || len(g.T) != n ||
		len(g.DM) != n || len(g.G) != n || len(g.N) != n {
		return nil, errors.New("ducting: grid slices do not match ni*nj")
	}
	for _, c := range g.C {
		if c > ClassSurface {
			return nil, fmt.Errorf("ducting: grid class %d out of range", c)
		}
	}

	raw := make([]byte, n*GridBytesPerPoint)
	off := copy(raw, g.C)
	off = putU16s(raw, off, g.F)
	off = putU16s(raw, off, g.B)
	off = putU16s(raw, off, g.T)
	off = putU16s(raw, off, g.DM)
	off = putI16s(raw, off, g.G)
	putI16s(raw, off, g.N)

	var buf bytes.Buffer
	buf.Grow(6 + len(g.Model) + gridHeaderFixed + n)
	buf.WriteString(GridMagic)
	buf.WriteByte(GridVersion)
	buf.WriteByte(byte(len(g.Model)))
	buf.WriteString(g.Model)
	var hdr [gridHeaderFixed]byte
	le := binary.LittleEndian
	le.PutUint32(hdr[0:], uint32(g.Ni))
	le.PutUint32(hdr[4:], uint32(g.Nj))
	le.PutUint64(hdr[8:], math.Float64bits(g.Lat0))
	le.PutUint64(hdr[16:], math.Float64bits(g.Lon0))
	le.PutUint64(hdr[24:], math.Float64bits(g.DLat))
	le.PutUint64(hdr[32:], math.Float64bits(g.DLon))
	le.PutUint64(hdr[40:], uint64(g.RunTime))
	le.PutUint64(hdr[48:], uint64(g.ValidTime))
	buf.Write(hdr[:])

	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return nil, fmt.Errorf("ducting: deflate: %w", err)
	}
	if _, err := fw.Write(raw); err != nil {
		return nil, fmt.Errorf("ducting: deflate: %w", err)
	}
	if err := fw.Close(); err != nil {
		return nil, fmt.Errorf("ducting: deflate: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode parses an encoded grid. It reads database contents, so it trusts
// nothing: the input size, every header field and the inflated payload size
// are bounded before anything is allocated, and the payload must inflate to
// exactly ni*nj*13 bytes with nothing after the DEFLATE stream.
func Decode(b []byte) (*Grid, error) {
	if len(b) > decodeMaxInput {
		return nil, fmt.Errorf("ducting: encoded grid %d bytes exceeds %d", len(b), decodeMaxInput)
	}
	if len(b) < 6 {
		return nil, errors.New("ducting: encoded grid truncated")
	}
	if string(b[:4]) != GridMagic {
		return nil, errors.New("ducting: bad grid magic")
	}
	if b[4] != GridVersion {
		return nil, fmt.Errorf("ducting: unsupported grid version %d", b[4])
	}
	ml := int(b[5])
	if len(b) < 6+ml+gridHeaderFixed {
		return nil, errors.New("ducting: encoded grid header truncated")
	}
	model := string(b[6 : 6+ml])
	h := b[6+ml : 6+ml+gridHeaderFixed]
	le := binary.LittleEndian
	ni := int64(le.Uint32(h[0:]))
	nj := int64(le.Uint32(h[4:]))
	lat0 := math.Float64frombits(le.Uint64(h[8:]))
	lon0 := math.Float64frombits(le.Uint64(h[16:]))
	dLat := math.Float64frombits(le.Uint64(h[24:]))
	dLon := math.Float64frombits(le.Uint64(h[32:]))
	runTime := int64(le.Uint64(h[40:]))
	validTime := int64(le.Uint64(h[48:]))
	if err := checkLattice(model, lat0, lon0, dLat, dLon, ni, nj); err != nil {
		return nil, err
	}

	n := int(ni * nj)
	expected := int64(n) * GridBytesPerPoint
	raw := make([]byte, expected)
	src := bytes.NewReader(b[6+ml+gridHeaderFixed:])
	fr := flate.NewReader(src)
	defer fr.Close()
	lr := io.LimitReader(fr, expected+1)
	if _, err := io.ReadFull(lr, raw); err != nil {
		return nil, fmt.Errorf("ducting: grid payload: %w", err)
	}
	var extra [1]byte
	switch _, err := io.ReadFull(lr, extra[:]); {
	case err == nil:
		return nil, errors.New("ducting: grid payload inflates past ni*nj*13 bytes")
	case err != io.EOF:
		return nil, fmt.Errorf("ducting: grid payload: %w", err)
	}
	// bytes.Reader is an io.ByteReader, so flate reads it byte by byte
	// without read-ahead: whatever is left is not part of the stream.
	if src.Len() != 0 {
		return nil, fmt.Errorf("ducting: %d bytes after grid payload", src.Len())
	}

	g, err := NewGrid(model, runTime, validTime, lat0, lon0, dLat, dLon, int(ni), int(nj))
	if err != nil {
		return nil, err
	}
	off := copy(g.C, raw[:n])
	for _, c := range g.C {
		if c > ClassSurface {
			return nil, fmt.Errorf("ducting: grid class %d out of range", c)
		}
	}
	off = getU16s(raw, off, g.F)
	off = getU16s(raw, off, g.B)
	off = getU16s(raw, off, g.T)
	off = getU16s(raw, off, g.DM)
	off = getI16s(raw, off, g.G)
	getI16s(raw, off, g.N)
	return g, nil
}

func putU16s(dst []byte, off int, src []uint16) int {
	for _, v := range src {
		binary.LittleEndian.PutUint16(dst[off:], v)
		off += 2
	}
	return off
}

func putI16s(dst []byte, off int, src []int16) int {
	for _, v := range src {
		binary.LittleEndian.PutUint16(dst[off:], uint16(v))
		off += 2
	}
	return off
}

func getU16s(src []byte, off int, dst []uint16) int {
	for i := range dst {
		dst[i] = binary.LittleEndian.Uint16(src[off:])
		off += 2
	}
	return off
}

func getI16s(src []byte, off int, dst []int16) int {
	for i := range dst {
		dst[i] = int16(binary.LittleEndian.Uint16(src[off:]))
		off += 2
	}
	return off
}
