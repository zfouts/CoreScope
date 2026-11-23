package main

// Location privacy ("fuzzy location"): when locationPrivacy.enabled is set,
// the server never serves a node's exact position. Every coordinate it serves
// or computes with (distances, planner, reach, coverage) is the approximate
// position from (*locationFuzzer).Fuzz; the geo_filter classification is the
// only consumer of exact coordinates. The database stays exact: the ingestor
// is unchanged and the server stays read-only (#1283).
//
// CONTRACT (landed by the CTO; tickets implement the bodies, signatures stay):
//
//   Config JSON:   "locationPrivacy": {"enabled": true, "radiusM": 1000, "secret": "<≥32 bytes>"}
//                  The environment variable CORESCOPE_LOCATION_SECRET, when set,
//                  overrides "secret". radiusM 0 → default 1000; valid 100..50000.
//   Startup:       main calls newLocationFuzzer(cfg.LocationPrivacy) right after
//                  LoadConfig and exits (log.Fatalf) on error: enabled with a
//                  missing/short secret or an out-of-range radius never starts.
//                  On success it stores the result with cfg.setLocationFuzzer.
//   Lookup:        cfg.LocationFuzzer() — nil when disabled. Every method is
//                  nil-safe: a nil *locationFuzzer is the identity.
//   Client:        GET /api/config/client gains
//                  "locationPrivacy": {"enabled": bool, "radiusM": number}
//                  (ClientLocationPrivacy). The secret is never serialised or logged.
//   Packets:       for an ADVERT whose payload carries a location, every packet
//                  and observation the server serves (HTTP and WebSocket) has
//                  raw_hex null, decoded_json without "lat", "lon", "signature",
//                  and "location_hidden": true. hash stays.
//
// Geometry (B1): metre-based grid, cell side s = 0.4·R, origin offset derived
// from HMAC-SHA256(secret, "grid"); a node keeps its anchor cell while its
// exact position stays inside that cell grown by 0.25·s on every side
// (hysteresis, in memory, bounded); the served point is the anchor cell centre
// plus an offset uniform in a disk of radius r = R − 1.0607·s, derived from
// HMAC-SHA256(secret, pubkey | row | col). So |served − exact| ≤ R always.

// LocationPrivacyConfig is the "locationPrivacy" config block.
type LocationPrivacyConfig struct {
	Enabled bool    `json:"enabled"`
	RadiusM float64 `json:"radiusM"`
	Secret  string  `json:"secret"`
}

// ClientLocationPrivacy is what /api/config/client tells the frontend.
type ClientLocationPrivacy struct {
	Enabled bool    `json:"enabled"`
	RadiusM float64 `json:"radiusM"`
}

// locationFuzzer turns exact positions into approximate ones. Safe for
// concurrent use. A nil *locationFuzzer means the feature is off.
type locationFuzzer struct {
	radiusM float64
}

// newLocationFuzzer validates the config and builds the fuzzer. It returns
// (nil, nil) when the block is absent or disabled.
func newLocationFuzzer(c *LocationPrivacyConfig) (*locationFuzzer, error) {
	// STUB (B1 implements): validation, secret, grid, hysteresis.
	if c == nil || !c.Enabled {
		return nil, nil
	}
	return &locationFuzzer{radiusM: c.RadiusM}, nil
}

// Enabled reports whether positions are fuzzed.
func (f *locationFuzzer) Enabled() bool { return f != nil }

// RadiusM is the maximum distance between a served and an exact position
// (0 when disabled).
func (f *locationFuzzer) RadiusM() float64 {
	if f == nil {
		return 0
	}
	return f.radiusM
}

// Fuzz returns the approximate position served for the node pubkey (any case)
// at exact position (lat, lon). A no-fix position (0, 0) is returned unchanged;
// any other result is never (0, 0). Identity when f is nil.
func (f *locationFuzzer) Fuzz(pubkey string, lat, lon float64) (float64, float64) {
	// STUB (B1 implements).
	return lat, lon
}

// Client is the /api/config/client view.
func (f *locationFuzzer) Client() ClientLocationPrivacy {
	return ClientLocationPrivacy{Enabled: f.Enabled(), RadiusM: f.RadiusM()}
}

// setLocationFuzzer installs the validated fuzzer (main, tests).
func (c *Config) setLocationFuzzer(f *locationFuzzer) {
	if c != nil {
		c.locFuzz = f
	}
}

// LocationFuzzer is the installed fuzzer; nil (identity) when disabled or c is nil.
func (c *Config) LocationFuzzer() *locationFuzzer {
	if c == nil {
		return nil
	}
	return c.locFuzz
}
