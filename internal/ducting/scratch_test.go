package ducting

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
)

// randomInputs draws a messy profile: out-of-range, NaN, unsorted and
// duplicate-height levels, mismatched lengths.
func randomInputs(r *rand.Rand) (elev, ps, t2, rh2 float64, lv, ts, rhs, zs []float64) {
	nan := math.NaN()
	pick := func(v float64) float64 {
		if r.Intn(25) == 0 {
			return nan
		}
		return v
	}
	elev = r.Float64() * 3000
	ps = pick(900 + r.Float64()*200)
	if r.Intn(15) == 0 {
		ps = nan
	}
	t2 = pick(-40 + r.Float64()*80)
	rh2 = pick(r.Float64()*130 - 10)
	n := 1 + r.Intn(40)
	mk := func(n int, f func() float64) []float64 {
		s := make([]float64, n)
		for i := range s {
			s[i] = pick(f())
		}
		return s
	}
	lv = mk(n, func() float64 { return 300 + r.Float64()*800 })
	ts = mk(n, func() float64 { return -60 + r.Float64()*100 })
	rhs = mk(n, func() float64 { return r.Float64()*130 - 10 })
	zs = mk(n, func() float64 {
		if r.Intn(8) == 0 {
			return elev + 500 // forced duplicate heights
		}
		return elev - 100 + r.Float64()*3500
	})
	if r.Intn(10) == 0 {
		ts = ts[:len(ts)/2]
	}
	return
}

func TestScratchEqualsPackageFunctions(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	var sc Scratch // reused across all cases: stale data must not leak
	for i := 0; i < 20000; i++ {
		elev, ps, t2, rh2, lv, ts, rhs, zs := randomInputs(r)
		want := Profile(elev, ps, t2, rh2, lv, ts, rhs, zs)
		got := sc.Profile(elev, ps, t2, rh2, lv, ts, rhs, zs)
		if len(want) == 0 && len(got) == 0 {
			// both empty
		} else if !reflect.DeepEqual(want, got) {
			t.Fatalf("case %d: Profile %+v != %+v", i, got, want)
		}
		if w, g := Analyze(want), sc.Analyze(got); w != g {
			t.Fatalf("case %d: Analyze %+v != %+v", i, g, w)
		}
	}
}

func TestScratchDoesNotAllocate(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	elev, ps, t2, rh2, lv, ts, rhs, zs := randomInputs(r)
	var sc Scratch
	sc.Analyze(sc.Profile(elev, ps, t2, rh2, lv, ts, rhs, zs)) // grow buffers
	if a := testing.AllocsPerRun(100, func() {
		sc.Analyze(sc.Profile(elev, ps, t2, rh2, lv, ts, rhs, zs))
	}); a != 0 {
		t.Fatalf("%v allocs per point, want 0", a)
	}
}
