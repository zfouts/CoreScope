package main

// knownLocationLeaks is the ratchet of the location-privacy leak test
// (location_privacy_leak_test.go): every "<route> <check>" pair that reveals
// an exact position today, with the ticket that closes it.
//
//	B3 coordinates    node positions (nodes, inactive_nodes) and what is
//	                  computed from them: distances, planner, reach, route LOS.
//	B4 companion GPS  client_receptions / client_rf_samples and what is
//	                  derived from them: coverage cells, RF noise, estimates.
//	B5 packets        located adverts: raw_hex, decoded_json, the live feed.
//
// "B3 coordinates, B5 packets": the route serves node rows and adverts, and
// the entry goes only when both are done.
//
// The test fails on a leak that is not listed here, and on an entry that no
// longer leaks: a ticket deletes its entries as it closes them. Never add one
// to get a route through. The finished feature has an empty map.
//
// Routes are GET templates; "POST …" are the data POSTs, "ws:…" the live
// feed, "oracle:…" the oracle checks. Regenerate with
//
//	LOCPRIV_LEAK_REPORT=1 go test -run LocationLeak ./...
//
// which prints the current set in this format. Generated against the identity
// stub of Fuzz; the same set holds with the real one (the checks treat an
// exact value as a leak either way).
//
// Fixture gaps: none. Every route answers 200 from the seeded data except
// GET /api/notifications/unsubscribe (303, a redirect by design).
var knownLocationLeaks = map[string]string{
	"/api/analytics/distance dist":          "B3 coordinates",
	"/api/analytics/hash-collisions coords": "B3 coordinates",
	"/api/analytics/hash-collisions text":   "B3 coordinates",
	"/api/analytics/subpath-detail coords":  "B3 coordinates",
	"/api/analytics/subpath-detail text":    "B3 coordinates",
	"/api/audio-lab/buckets coords":         "B5 packets",
	"/api/audio-lab/buckets packet":         "B5 packets",
	"/api/audio-lab/buckets rawhex":         "B5 packets",
	"/api/audio-lab/buckets text":           "B5 packets",
	"/api/nodes coords":                     "B3 coordinates",
	"/api/nodes text":                       "B3 coordinates",
	"/api/nodes/bulk-health coords":         "B3 coordinates",
	"/api/nodes/bulk-health text":           "B3 coordinates",
	"/api/nodes/search coords":              "B3 coordinates",
	"/api/nodes/search text":                "B3 coordinates",
	"/api/nodes/{pubkey} coords":            "B3 coordinates, B5 packets",
	"/api/nodes/{pubkey} packet":            "B5 packets",
	"/api/nodes/{pubkey} rawhex":            "B5 packets",
	"/api/nodes/{pubkey} text":              "B3 coordinates, B5 packets",
	"/api/nodes/{pubkey}/analytics coords":  "B3 coordinates",
	"/api/nodes/{pubkey}/analytics text":    "B3 coordinates",
	"/api/nodes/{pubkey}/health coords":     "B3 coordinates, B5 packets",
	"/api/nodes/{pubkey}/health packet":     "B5 packets",
	"/api/nodes/{pubkey}/health rawhex":     "B5 packets",
	"/api/nodes/{pubkey}/health text":       "B3 coordinates, B5 packets",
	"/api/nodes/{pubkey}/neighbors dist":    "B3 coordinates",
	"/api/nodes/{pubkey}/paths coords":      "B3 coordinates",
	"/api/nodes/{pubkey}/paths text":        "B3 coordinates",
	"/api/nodes/{pubkey}/reach coords":      "B3 coordinates",
	"/api/nodes/{pubkey}/reach dist":        "B3 coordinates",
	"/api/nodes/{pubkey}/reach text":        "B3 coordinates",
	"/api/nodes/{pubkey}/rx-coverage text":  "B4 companion GPS",
	"/api/observers coords":                 "B3 coordinates",
	"/api/observers text":                   "B3 coordinates",
	"/api/observers/{id}/analytics coords":  "B5 packets",
	"/api/observers/{id}/analytics packet":  "B5 packets",
	"/api/observers/{id}/analytics rawhex":  "B5 packets",
	"/api/observers/{id}/analytics text":    "B5 packets",
	"/api/packets coords":                   "B5 packets",
	"/api/packets packet":                   "B5 packets",
	"/api/packets rawhex":                   "B5 packets",
	"/api/packets text":                     "B5 packets",
	"/api/packets/{id} coords":              "B5 packets",
	"/api/packets/{id} packet":              "B5 packets",
	"/api/packets/{id} rawhex":              "B5 packets",
	"/api/packets/{id} text":                "B5 packets",
	"/api/planner/coverage coords":          "B3 coordinates",
	"/api/planner/coverage text":            "B3 coordinates",
	"/api/planner/relay coords":             "B3 coordinates",
	"/api/planner/relay dist":               "B3 coordinates",
	"/api/planner/relay text":               "B3 coordinates",
	"/api/planner/viewshed coords":          "B3 coordinates",
	"/api/planner/viewshed dist":            "B3 coordinates",
	"/api/planner/viewshed text":            "B3 coordinates",
	"/api/resolve-hops coords":              "B3 coordinates",
	"/api/resolve-hops text":                "B3 coordinates",
	"/api/rf-noise text":                    "B4 companion GPS",
	"/api/rx-coverage text":                 "B4 companion GPS",
	"POST /api/packets/observations packet": "B5 packets",
	"POST /api/packets/observations rawhex": "B5 packets",
	"oracle:elevation-known known-point":    "B3 coordinates",
	"ws:observation coords":                 "B5 packets",
	"ws:observation packet":                 "B5 packets",
	"ws:observation rawhex":                 "B5 packets",
	"ws:observation text":                   "B5 packets",
	"ws:packet coords":                      "B5 packets",
	"ws:packet packet":                      "B5 packets",
	"ws:packet rawhex":                      "B5 packets",
	"ws:packet text":                        "B5 packets",
	"ws:packet-nostore coords":              "B5 packets",
	"ws:packet-nostore packet":              "B5 packets",
	"ws:packet-nostore rawhex":              "B5 packets",
	"ws:packet-nostore text":                "B5 packets",
}
