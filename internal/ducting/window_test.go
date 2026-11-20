package ducting

import (
	"math"
	"testing"
)

// smallGrid is a 5x5 non-wrapping lattice, lat 0..4 N, lon 0..4 E, 1° apart.
func smallGrid(t *testing.T) *Grid {
	t.Helper()
	g, err := NewGrid("small", 0, 0, 0, 0, 1, 1, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func cellAt(w Window, lat, lon float64) (WindowCell, bool) {
	for _, c := range w.Cells {
		if math.Abs(c.Lat-lat) < 1e-9 && math.Abs(c.Lon-lon) < 1e-9 {
			return c, true
		}
	}
	return WindowCell{}, false
}

func TestWindowAggregation(t *testing.T) {
	g := smallGrid(t)
	at := func(r, c int) int { return r*g.Ni + c }
	set := func(r, c int, cl uint8, f, b, top, dm uint16, gr, n int16) {
		i := at(r, c)
		g.C[i], g.F[i], g.B[i], g.T[i], g.DM[i], g.G[i], g.N[i] = cl, f, b, top, dm, gr, n
	}
	set(1, 1, ClassSurface, 500, 0, 100, 50, -200, -60) // on the corner of four cells
	set(0, 0, ClassElev, 300, 200, 400, 20, -150, -40)
	set(0, 1, ClassNormal, 0, 0, 0, 0, -10, 0)
	set(1, 0, ClassNoData, 0, 0, 0, 0, -999, 0) // G ignored: no data
	set(0, 2, ClassNormal, 0, 0, 0, 0, 5, -33)
	set(2, 2, ClassElev, 0, 999, 999, 999, 0, 0) // ducted but F 0: never picked

	w := g.Window(0, 0, 4, 4, 1000)
	if w.Step != 1 {
		t.Fatalf("step %v", w.Step)
	}
	want := map[[2]float64]WindowCell{
		// [0,1]x[0,1]: points (0,0) (0,1) (1,0) (1,1)
		{0.5, 0.5}: {C: ClassSurface, F: 300, B: 200, T: 400, DM: 20, G: -200, N: -50},
		// [0,1]x[1,2]: (0,1) (0,2) (1,1) (1,2); N mean(-60,-33) = -46.5 -> -47
		{0.5, 1.5}: {C: ClassSurface, F: 500, B: 0, T: 100, DM: 50, G: -200, N: -47},
		// [1,2]x[1,2]: (1,1) (1,2) (2,1) (2,2)
		{1.5, 1.5}: {C: ClassSurface, F: 500, B: 0, T: 100, DM: 50, G: -200, N: -60},
		// [1,2]x[0,1]: (1,0) (1,1) (2,0) (2,1)
		{1.5, 0.5}: {C: ClassSurface, F: 500, B: 0, T: 100, DM: 50, G: -200, N: -60},
		// [0,1]x[2,3]: (0,2) (0,3) (1,2) (1,3)
		{0.5, 2.5}: {C: ClassNormal, G: 5, N: -33},
		// [1,2]x[2,3] and [2,3]x[1,2], [2,3]x[2,3]: (2,2) elev with F 0
		{1.5, 2.5}: {C: ClassElev},
		{2.5, 1.5}: {C: ClassElev},
		{2.5, 2.5}: {C: ClassElev},
	}
	if len(w.Cells) != len(want) {
		t.Errorf("%d cells, want %d: %+v", len(w.Cells), len(want), w.Cells)
	}
	for k, wc := range want {
		wc.Lat, wc.Lon = k[0], k[1]
		got, ok := cellAt(w, k[0], k[1])
		if !ok || got != wc {
			t.Errorf("cell %v = %+v (%v), want %+v", k, got, ok, wc)
		}
	}
	for _, c := range w.Cells {
		if c.C < ClassNormal {
			t.Errorf("class-0 cell returned: %+v", c)
		}
	}
}

func TestWindowSnapsOutward(t *testing.T) {
	g := smallGrid(t)
	g.C[3*g.Ni+3] = ClassNormal // lat 3, lon 3
	// Box inside one cell row/column still yields the cells touching (3,3).
	w := g.Window(2.6, 2.6, 3.4, 3.4, 1000)
	if len(w.Cells) != 4 {
		t.Fatalf("%d cells: %+v", len(w.Cells), w.Cells)
	}
	for _, p := range [][2]float64{{2.5, 2.5}, {2.5, 3.5}, {3.5, 2.5}, {3.5, 3.5}} {
		if _, ok := cellAt(w, p[0], p[1]); !ok {
			t.Errorf("missing cell %v", p)
		}
	}
}

func TestWindowEmptyGrid(t *testing.T) {
	g := gfsGrid(t)
	if w := g.Window(-90, -180, 90, 180, 4000); len(w.Cells) != 0 {
		t.Fatalf("no-data grid returned %d cells", len(w.Cells))
	}
	for _, b := range [][4]float64{{10, 0, 10, 5}, {0, 5, 1, 5}, {math.NaN(), 0, 1, 1}} {
		if w := g.Window(b[0], b[1], b[2], b[3], 4000); len(w.Cells) != 0 {
			t.Errorf("box %v returned cells", b)
		}
	}
}

func fillClass(g *Grid, c uint8) {
	for i := range g.C {
		g.C[i] = c
	}
}

func TestWindowLadder(t *testing.T) {
	g := gfsGrid(t)
	fillClass(g, ClassNormal)
	for _, tc := range []struct {
		maxCells   int
		step       float64
		rows, cols int
	}{
		{360 * 720, 0.5, 360, 720},
		{360*720 - 1, 1, 180, 360},
		{180 * 360, 1, 180, 360},
		{90 * 180, 2, 90, 180},
		{4000, 8, 24, 45}, // step 4 needs 46 x 90 = 4140
		{46 * 90, 4, 46, 90},
		{1, 8, 24, 45}, // nothing fits: coarsest
		{0, 8, 24, 45},
	} {
		w := g.Window(-90, -180, 90, 180, tc.maxCells)
		if w.Step != tc.step || len(w.Cells) != tc.rows*tc.cols {
			t.Errorf("maxCells %d: step %v cells %d, want step %v cells %d",
				tc.maxCells, w.Step, len(w.Cells), tc.step, tc.rows*tc.cols)
			continue
		}
		seen := make(map[[2]float64]bool, len(w.Cells))
		for _, c := range w.Cells {
			if c.Lat < -90 || c.Lat > 90 || c.Lon < -180 || c.Lon >= 180 {
				t.Fatalf("step %v: cell out of range %+v", w.Step, c)
			}
			k := [2]float64{c.Lat, c.Lon}
			if seen[k] {
				t.Fatalf("step %v: duplicate cell %v", w.Step, k)
			}
			seen[k] = true
		}
	}
	// Step 8 polar rows are clipped: [-96,-88] is centred on -89.
	w := g.Window(-90, -180, 90, 180, 0)
	if _, ok := cellAt(w, -89, -180); !ok {
		t.Error("clipped south polar cell missing")
	}
	if _, ok := cellAt(w, 89, -180); !ok {
		t.Error("clipped north polar cell missing")
	}
	// A regional box fits at full resolution.
	if w := g.Window(24, -125, 50, -66, 4000); w.Step != 1 || len(w.Cells) != 26*59 {
		t.Errorf("CONUS: step %v, %d cells", w.Step, len(w.Cells))
	}
}

func TestWindowAntimeridian(t *testing.T) {
	g := gfsGrid(t)
	equator := 180 * g.Ni
	g.C[equator+360] = ClassSurface // lat 0, lon 180 == -180
	g.F[equator+360] = 400
	g.C[equator+719] = ClassElev // lat 0, lon 359.5 == -0.5
	g.F[equator+719] = 700

	east := g.Window(-1, 170, 1, 180, 4000)
	west := g.Window(-1, -180, 1, -170, 4000)
	for _, tc := range []struct {
		w   Window
		lon float64
	}{{east, 179.75}, {west, -179.75}} {
		if len(tc.w.Cells) != 2 {
			t.Errorf("lon %v: %d cells: %+v", tc.lon, len(tc.w.Cells), tc.w.Cells)
		}
		for _, lat := range []float64{-0.25, 0.25} {
			if c, ok := cellAt(tc.w, lat, tc.lon); !ok || c.C != ClassSurface || c.F != 400 {
				t.Errorf("cell %v,%v = %+v %v", lat, tc.lon, c, ok)
			}
		}
	}

	// Column 719 (lon 359.5 == -0.5) is the edge of the two cells west of
	// the prime meridian.
	w := g.Window(-1, -1, 1, 1, 4000)
	if len(w.Cells) != 4 {
		t.Errorf("prime meridian: %+v", w.Cells)
	}
	for _, lon := range []float64{-0.75, -0.25} {
		if c, ok := cellAt(w, 0.25, lon); !ok || c.F != 700 {
			t.Errorf("cell at lon %v: %+v %v", lon, c, ok)
		}
	}

	// Whole world at step 8: the cell [-184,-176] (centre -180) holds the
	// point at lon 180 and is listed once.
	w = g.Window(-90, -180, 90, 180, 1)
	n := 0
	for _, c := range w.Cells {
		if c.C == ClassSurface {
			n++
			if c.Lon != -180 {
				t.Errorf("surface cell at lon %v", c.Lon)
			}
		}
	}
	if n != 2 { // lat 0 is the edge of rows [-8,0] and [0,8]
		t.Errorf("%d surface cells at step 8", n)
	}
}

func TestWindowRegionalLongitudeConvention(t *testing.T) {
	// Columns stored as 230..240 E; the window asks in -180..180.
	g, err := NewGrid("reg", 0, 0, 0, 230, 1, 1, 11, 11)
	if err != nil {
		t.Fatal(err)
	}
	fillClass(g, ClassNormal)
	w := g.Window(0, -130, 10, -120, 1000)
	if w.Step != 1 || len(w.Cells) != 100 {
		t.Fatalf("step %v, %d cells", w.Step, len(w.Cells))
	}
	if _, ok := cellAt(w, 0.5, -129.5); !ok {
		t.Error("missing south-west cell")
	}
	if w := g.Window(0, 0, 10, 10, 1000); len(w.Cells) != 0 {
		t.Errorf("box off the lattice returned %d cells", len(w.Cells))
	}
}

func BenchmarkWindowCONUS(b *testing.B) {
	g := smoothGrid(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Window(24, -125, 50, -66, 4000)
	}
}

func BenchmarkWindowWorld(b *testing.B) {
	g := smoothGrid(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Window(-90, -180, 90, 180, 4000)
	}
}
