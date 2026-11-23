package grib2test_test

import (
	"math"
	"testing"
	"time"

	"github.com/meshcore-analyzer/grib2"
	"github.com/meshcore-analyzer/grib2/grib2test"
)

func TestMessageRoundTrip(t *testing.T) {
	ref := time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)
	vals := []float64{288.15, 290.5, math.NaN(), 250.01, 300, 101325.25}
	for _, f := range []grib2test.Field{
		{Ni: 3, Nj: 2, La1: 90, Lo1: 0, DLat: -0.5, DLon: 0.5, Ref: ref, ForecastHour: 6,
			Category: 0, Number: 0, SurfaceType: 100, SurfaceValue: 85000, Values: vals},
		{Ni: 3, Nj: 2, La1: -60, Lo1: 10, DLat: 30, DLon: 30, Ref: ref, ForecastHour: 0,
			Category: 3, Number: 0, SurfaceType: 1, SurfaceValue: math.NaN(), Values: []float64{1, 2, 3, 4, 5, 6}},
	} {
		got, err := grib2.Decode(grib2test.Message(f))
		if err != nil {
			t.Fatal(err)
		}
		if got.Ni != f.Ni || got.Nj != f.Nj || got.La1 != f.La1 || got.Lo1 != f.Lo1 || got.DLat != f.DLat || got.DLon != f.DLon {
			t.Errorf("lattice %+v", got)
		}
		if got.RefTime != ref.Unix() || got.ForecastHour != f.ForecastHour {
			t.Errorf("times %d %d", got.RefTime, got.ForecastHour)
		}
		if got.ParameterCategory != f.Category || got.ParameterNumber != f.Number || got.SurfaceType != f.SurfaceType {
			t.Errorf("product %+v", got)
		}
		if !math.IsNaN(f.SurfaceValue) && got.SurfaceValue != f.SurfaceValue {
			t.Errorf("surface value %g", got.SurfaceValue)
		}
		for i, v := range f.Values {
			g := float64(got.Values[i])
			if math.IsNaN(v) != math.IsNaN(g) || !math.IsNaN(v) && math.Abs(g-v) > 0.01 {
				t.Errorf("value %d = %g, want %g", i, g, v)
			}
		}
	}
}
