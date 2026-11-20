// Package ducting holds the tropospheric ducting physics and the stored grid
// format, shared by the ingestor (which computes and writes the global grid)
// and the server (which reads it, and still runs the physics for the
// Open-Meteo source).
//
// Physics (calc.go):
//
//	e  = RH/100 * 6.1121 * exp(17.502*t / (240.97 + t))     hPa, t in °C
//	N  = 77.6/T * (P + 4810*e/T)                            ITU-R P.453, P,e hPa, T K
//	M  = N + 0.157*h                                        h metres above ground
//	trapping layer: consecutive levels where M decreases with height
//	duct thickness d (m) and strength ΔM (M units) -> lowest trapped frequency
//	fmin = 120000 / (d * sqrt(ΔM))  MHz   (λmax ≈ 0.0025 * d * sqrt(ΔM) m)
//
// Grid (grid.go): one analysed time step on a regular lat/lon lattice, the
// unit stored per row of the ducting_grid table (Encode/Decode).
package ducting

// Duct classes (Result.Class, Grid.C values). Ordered by severity, so the
// larger class wins when cells are merged.
const (
	ClassNoData  uint8 = 0 // fewer than 2 usable levels, or no forecast
	ClassNormal  uint8 = 1 // min dN/dh > -79 N/km
	ClassSuper   uint8 = 2 // super-refractive: min dN/dh <= -79 N/km, no trapping layer
	ClassElev    uint8 = 3 // elevated duct (duct bottom above the surface point)
	ClassSurface uint8 = 4 // surface-based duct (duct reaches the surface point)
)

// Analysis limits.
const (
	MaxHeightAGL    = 3000.0 // m: only the lowest 3 km are analysed
	SuperGradientNk = -79.0  // N/km: super-refraction threshold
	SurfaceHeightM  = 2.0    // m AGL of the 2 m surface point
	// FminConstMHz: fmin = FminConstMHz / (d * sqrt(ΔM)) MHz, the same
	// constant for surface-based and elevated ducts, on purpose. There is no
	// single ratio between the two: ITU-R P.834-4 (2003) section 7.2 and its
	// Fig. 4 show that for an elevated duct fmin depends on the shape of the
	// profile below the layer, and is higher than for the equivalent surface
	// duct when the lapse rate is only just past ducting but lower for very
	// intense gradients. NTIA Report 81-69 (Dougherty and Dutton, 1981,
	// eq. 7) likewise applies one thickness law to elevated ducts. With model
	// levels 200-250 m apart, d is itself only a coarse bound, so a per-class
	// factor would add precision the data does not have. d for an elevated
	// duct is the full duct thickness (from where M below the layer equals M
	// at its top), not just the layer.
	FminConstMHz = 120000.0
	MeanLayerM   = 1500.0 // m: depth of the layer Result.MeanGradNkm averages over
)

// Level is one point of a vertical profile, ascending by HeightAGL.
type Level struct {
	HeightAGL   float64 // metres above ground
	PressureHPa float64
	TempC       float64
	RHPct       float64 // 0..100
}

// Result is the analysis of one profile (one cell, one time).
// Heights are metres above ground; all zero when there is no duct.
type Result struct {
	Class      uint8
	FminMHz    float64 // lowest frequency the strongest duct can trap; 0 = no duct
	BaseM      float64 // duct bottom (0 for surface-based)
	TopM       float64 // top of the trapping layer (M minimum)
	DeltaM     float64 // duct strength, M units
	MinGradNkm float64 // most negative dN/dh between consecutive levels, N/km
	// MeanGradNkm is the mean refractivity gradient over the lowest
	// MeanLayerM above ground: (N at MeanLayerM - N at the 2 m surface point)
	// / MeanLayerM, in N/km (ITU-R P.453 style). The map field is coloured by
	// it: averaged over the layer that matters for propagation, it is far
	// less noisy than MinGradNkm, which a single thin layer can dominate. On
	// high terrain, where the profile stops below MeanLayerM but its top is
	// at least 1000 m above ground, it is the mean over that shallower depth:
	// (N at the top - N at 2 m) / the top's height. 0 when the profile is
	// shallower than 1000 m (or there is no data).
	MeanGradNkm float64
}
