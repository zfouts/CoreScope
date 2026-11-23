package main

import (
	"sort"
	"strings"
	"testing"
)

// TestPgNodeCoordsFuzzed runs every node coordinate reader (node_coords.go)
// on both backends with location privacy on. The fuzzer is keyed by pubkey
// and the stored position, so both backends must serve the same approximate
// position, and the exact readers the same stored one.
func TestPgNodeCoordsFuzzed(t *testing.T) {
	d := newDualTestDB(t)
	seedPgDBQueries(t, d)
	fuzz := cellTestFuzzer(t, cellTestRadiusM)
	d.sqlite.setLocationFuzzer(fuzz)
	d.pg.setLocationFuzzer(fuzz)
	t.Cleanup(func() {
		d.sqlite.setLocationFuzzer(nil)
		d.pg.setLocationFuzzer(nil)
	})
	const storedLat, storedLon = 37.3, -121.9 // pgqPkA as seeded

	// served fails unless a served position of pgqPkA is approximate.
	served := func(t *testing.T, name string, lat, lon any) {
		t.Helper()
		la, ok1 := lat.(float64)
		lo, ok2 := lon.(float64)
		if !ok1 || !ok2 {
			t.Fatalf("%s: no served position: %v, %v", name, lat, lon)
		}
		if la == storedLat && lo == storedLon {
			t.Fatalf("%s served the stored position", name)
		}
		if wLa, wLo := fuzz.Fuzz(pgqPkA, storedLat, storedLon); la != wLa || lo != wLo {
			t.Fatalf("%s served (%v, %v), want the fuzzed (%v, %v)", name, la, lo, wLa, wLo)
		}
	}

	t.Run("scanNodeRow", func(t *testing.T) {
		nodes := same(t, d, "GetNodes", false, func(db *DB) ([]map[string]any, error) {
			n, _, _, err := db.GetNodes(NodeQuery{SortBy: "name"})
			return n, err
		})
		found := false
		for _, n := range nodes {
			switch n["public_key"] {
			case pgqPkA:
				found = true
				served(t, "GetNodes", n["lat"], n["lon"])
			case pgqPkB: // no stored position
				if n["lat"] != nil || n["lon"] != nil {
					t.Fatalf("GetNodes: position for a node without one: %v", n)
				}
			}
		}
		if !found {
			t.Fatalf("GetNodes: %s missing", pgqPkA)
		}
		one := same(t, d, "GetNodeByPubkey", false, func(db *DB) (map[string]any, error) { return db.GetNodeByPubkey(pgqPkA) })
		served(t, "GetNodeByPubkey", one["lat"], one["lon"])
	})
	t.Run("getAllNodes", func(t *testing.T) {
		nodes := same(t, d, "getAllNodes", false, func(db *DB) ([]nodeInfo, error) {
			n := NewPacketStore(db, &PacketStoreConfig{}).getAllNodes()
			sort.Slice(n, func(i, j int) bool { return n[i].PublicKey < n[j].PublicKey })
			return n, nil
		})
		for _, n := range nodes {
			if n.PublicKey != pgqPkA {
				continue
			}
			served(t, "getAllNodes", n.Lat, n.Lon)
			if !n.hasExact || n.exactLat != storedLat || n.exactLon != storedLon {
				t.Fatalf("getAllNodes exact position: %+v", n)
			}
			return
		}
		t.Fatalf("getAllNodes: %s missing", pgqPkA)
	})
	t.Run("bulkHealthNodes", func(t *testing.T) {
		nodes := same(t, d, "bulkHealthNodes", false, func(db *DB) ([]bulkHealthNode, error) {
			return NewPacketStore(db, &PacketStoreConfig{}).bulkHealthNodes(10, false, nil)
		})
		for _, n := range nodes {
			if n.pk == pgqPkA {
				served(t, "bulkHealthNodes", n.lat, n.lon)
				return
			}
		}
		t.Fatalf("bulkHealthNodes: %s missing", pgqPkA)
	})
	t.Run("GetNodeLocationsByKeys", func(t *testing.T) {
		locs := same(t, d, "GetNodeLocationsByKeys", false, func(db *DB) (map[string]nodeLocation, error) {
			return db.GetNodeLocationsByKeys([]string{strings.ToUpper(pgqPkA), pgqPkB}), nil
		})
		served(t, "GetNodeLocationsByKeys", locs[pgqPkA].Lat, locs[pgqPkA].Lon)
		if b, ok := locs[pgqPkB]; !ok || b.Lat != nil || b.Lon != nil {
			t.Fatalf("GetNodeLocationsByKeys no-position row: %+v %v", b, ok)
		}
	})
	// The exact readers classify by the stored position on both backends.
	t.Run("exactNodePositions", func(t *testing.T) {
		exact := same(t, d, "exactNodePositions", false, func(db *DB) (map[string]exactNodePos, error) {
			return db.exactNodePositions([]string{pgqPkA, pgqPkB, "nope"})
		})
		if p := exact[pgqPkA]; !p.ok || p.lat != storedLat || p.lon != storedLon {
			t.Fatalf("exactNodePositions: %+v", p)
		}
		if p, ok := exact[pgqPkB]; !ok || p.ok {
			t.Fatalf("exactNodePositions no-position row: %+v %v", p, ok)
		}
		if len(exact) != 2 {
			t.Fatalf("exactNodePositions: %v", exact)
		}
	})
	t.Run("GetNodePubkeysInArea", func(t *testing.T) {
		lo, hi, lonLo, lonHi := 30.0, 60.0, -125.0, 1.0
		same(t, d, "GetNodePubkeysInArea", false, func(db *DB) ([]string, error) {
			return db.GetNodePubkeysInArea(AreaEntry{LatMin: &lo, LatMax: &hi, LonMin: &lonLo, LonMax: &lonHi})
		})
	})
	t.Run("GetNodesForGeoPrune", func(t *testing.T) {
		nodes := same(t, d, "GetNodesForGeoPrune", false, func(db *DB) ([]NodeForGeoPrune, error) { return db.GetNodesForGeoPrune() })
		for _, n := range nodes {
			if n.PubKey == pgqPkA && (n.Lat == nil || *n.Lat != storedLat || n.Lon == nil || *n.Lon != storedLon) {
				t.Fatalf("GetNodesForGeoPrune must read the stored position: %+v", n)
			}
		}
	})
}
