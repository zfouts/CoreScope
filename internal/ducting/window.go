package ducting

import "math"

// windowHardMaxCells caps the accumulator array of one Window call. Only a
// lattice far finer than any real model, zoomed out to the whole world,
// can reach it; Window then returns no cells rather than allocating
// without bound.
const windowHardMaxCells = 2 * GridMaxPoints

// idxEps is the slack, in lattice index units, for a point lying exactly
// on a cell edge.
const idxEps = 1e-9

var windowLadder = [...]float64{1, 2, 4, 8, 16}

// Window aggregates the grid into square cells over the bounding box, at the
// finest ladder step that yields at most maxCells cells (else the
// coarsest). See Window and WindowCell for the rules. The caller guarantees
// -90 <= south < north <= 90 and -180 <= west < east <= 180; anything else
// returns no cells.
//
// Cost is O(lattice points in the snapped box + cells): each cell row maps
// to a contiguous run of lattice rows and each cell column to a short list
// of lattice columns, both computed directly from the lattice spacing.
func (g *Grid) Window(south, west, north, east float64, maxCells int) Window {
	empty := Window{Step: g.DLon}
	if !(south < north) || !(west < east) || !isFinite(south) || !isFinite(north) || !isFinite(west) || !isFinite(east) {
		return empty
	}
	var (
		step               float64
		i0, rows, j0, cols int
		found              bool
	)
	for _, k := range windowLadder {
		s := k * g.DLon
		si0, sr, sj0, sc, ok := windowShape(south, west, north, east, s)
		if !ok {
			continue
		}
		step, i0, rows, j0, cols, found = s, si0, sr, sj0, sc, true
		if rows*cols <= maxCells {
			break
		}
	}
	if !found {
		return empty
	}

	rowLo := make([]int, rows)
	rowHi := make([]int, rows)
	for ii := range rowLo {
		i := float64(i0 + ii)
		rowLo[ii], rowHi[ii] = g.latRows(i*step, (i+1)*step)
	}
	colOff := make([]int32, cols+1)
	colIdx := make([]int32, 0, cols*(windowLadderMax+2))
	for jj := 0; jj < cols; jj++ {
		j := float64(j0 + jj)
		colIdx = g.appendLonCols(colIdx, j*step, (j+1)*step)
		colOff[jj+1] = int32(len(colIdx))
	}

	acc := make([]winAcc, rows*cols)
	for ii := 0; ii < rows; ii++ {
		cellRow := acc[ii*cols : (ii+1)*cols]
		for r := rowLo[ii]; r <= rowHi[ii]; r++ {
			rb := r * g.Ni
			for jj := range cellRow {
				a := &cellRow[jj]
				for _, c := range colIdx[colOff[jj]:colOff[jj+1]] {
					a.add(g, rb+int(c))
				}
			}
		}
	}

	n := 0
	for i := range acc {
		if acc[i].c >= ClassNormal {
			n++
		}
	}
	out := Window{Step: step, Cells: make([]WindowCell, 0, n)}
	for ii := 0; ii < rows; ii++ {
		i := float64(i0 + ii)
		lat := (i + 0.5) * step
		if lo, hi := i*step, (i+1)*step; lo < -90 || hi > 90 {
			lat = (math.Max(lo, -90) + math.Min(hi, 90)) / 2
		}
		for jj := 0; jj < cols; jj++ {
			a := &acc[ii*cols+jj]
			if a.c < ClassNormal {
				continue
			}
			cell := WindowCell{
				Lat: lat, Lon: normLon((float64(j0+jj) + 0.5) * step),
				C: a.c, F: a.f, B: a.b, T: a.t, DM: a.dm, G: a.g,
			}
			if a.nCnt > 0 {
				cell.N = int16(math.Round(float64(a.nSum) / float64(a.nCnt)))
			}
			out.Cells = append(out.Cells, cell)
		}
	}
	return out
}

// windowLadderMax is the largest ladder multiplier: a cell column holds at
// most windowLadderMax+1 lattice columns (more only for a cell of 360°).
const windowLadderMax = 16

// windowShape snaps the box outward to multiples of step: cell rows i0 ..
// i0+rows-1 and columns j0 .. j0+cols-1. Rows are clipped to the cells that
// touch [-90, 90]; columns to one turn, so a box spanning the antimeridian
// does not list the same cell at both ends. ok is false when the shape is
// empty or over windowHardMaxCells.
func windowShape(south, west, north, east, step float64) (i0, rows, j0, cols int, ok bool) {
	fi0 := math.Max(math.Floor(south/step), math.Floor(-90/step))
	fi1 := math.Min(math.Ceil(north/step)-1, math.Ceil(90/step)-1)
	fj0 := math.Floor(west / step)
	fj1 := math.Ceil(east/step) - 1
	nr, nc := fi1-fi0+1, fj1-fj0+1
	if turn := math.Max(1, math.Floor(360/step+idxEps)); nc > turn {
		nc = turn
	}
	if !(nr >= 1 && nc >= 1 && nr*nc <= windowHardMaxCells) {
		return 0, 0, 0, 0, false
	}
	return int(fi0), int(nr), int(fj0), int(nc), true
}

// latRows returns the inclusive run of lattice rows whose latitude lies in
// [a, b]; lo > hi when there is none.
func (g *Grid) latRows(a, b float64) (lo, hi int) {
	x, y := (a-g.Lat0)/g.DLat, (b-g.Lat0)/g.DLat
	if x > y {
		x, y = y, x
	}
	lo = max(g.clampIdx(math.Ceil(x-idxEps), g.Nj), 0)
	hi = min(g.clampIdx(math.Floor(y+idxEps), g.Nj), g.Nj-1)
	return lo, hi
}

// clampIdx converts a float index to int, clamped to [-1, n] so out-of-range
// values never overflow the conversion; callers treat -1 and n as empty.
func (g *Grid) clampIdx(v float64, n int) int {
	return int(math.Max(-1, math.Min(v, float64(n))))
}

// appendLonCols appends the lattice columns whose longitude lies in [a, b]
// modulo 360. A column at lon0+c*dLon matches the interval shifted by
// 360k, for the few k at which that interval meets the lattice's span.
func (g *Grid) appendLonCols(dst []int32, a, b float64) []int32 {
	if b-a >= 360-lonSpanTol {
		for c := 0; c < g.Ni; c++ {
			dst = append(dst, int32(c))
		}
		return dst
	}
	span := float64(g.Ni-1) * g.DLon
	kLo := math.Ceil((g.Lon0-b)/360 - idxEps)
	kHi := math.Floor((g.Lon0+span-a)/360 + idxEps)
	for k := kLo; k <= kHi; k++ {
		lo := g.clampIdx(math.Ceil((a+360*k-g.Lon0)/g.DLon-idxEps), g.Ni)
		hi := g.clampIdx(math.Floor((b+360*k-g.Lon0)/g.DLon+idxEps), g.Ni)
		for c := max(lo, 0); c <= min(hi, g.Ni-1); c++ {
			dst = append(dst, int32(c))
		}
	}
	return dst
}

// winAcc accumulates one cell's lattice points.
type winAcc struct {
	c           uint8
	hasG        bool
	f, b, t, dm uint16 // from the ducted point with the lowest non-zero F
	g           int16
	nSum        int64
	nCnt        int64
}

func (a *winAcc) add(g *Grid, p int) {
	pc := g.C[p]
	if pc > a.c {
		a.c = pc
	}
	if pc >= ClassNormal {
		if gv := g.G[p]; !a.hasG || gv < a.g {
			a.g, a.hasG = gv, true
		}
	}
	if pc >= ClassElev {
		if f := g.F[p]; f > 0 && (a.f == 0 || f < a.f) {
			a.f, a.b, a.t, a.dm = f, g.B[p], g.T[p], g.DM[p]
		}
	}
	if nv := g.N[p]; nv != 0 {
		a.nSum += int64(nv)
		a.nCnt++
	}
}
