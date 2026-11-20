package dbschema

import (
	"database/sql"
	"fmt"
)

// ducting_grid holds the global tropospheric-ducting forecast the ingestor
// computes from NOAA GFS: one row per (model, valid_time), data an encoded
// grid (internal/ducting). All times are unix seconds. A row is replaced only
// by a strictly newer run_time (cmd/ingestor/ducting_store.go).
//
// Not required by AssertReady: a server that comes up before the ingestor
// created it serves no grid and probes again on the next request.
//
// PREFLIGHT: async=false reason="creates one empty table keyed by (model, valid_time); no scan of existing data"
func ensureDuctingGrid(rw *sql.DB) error {
	if _, err := rw.Exec(`
		CREATE TABLE IF NOT EXISTS ducting_grid (
			model      TEXT NOT NULL,
			valid_time INTEGER NOT NULL,
			run_time   INTEGER NOT NULL,
			lead_hours INTEGER NOT NULL,
			data       BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (model, valid_time)
		)
	`); err != nil {
		return fmt.Errorf("create ducting_grid: %w", err)
	}
	return nil
}
