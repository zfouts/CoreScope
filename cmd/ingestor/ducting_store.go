package main

import (
	"database/sql"
)

// ducting_grid write path (ingestor-owned; the server only reads the table).
// One row per (model, valid_time), all times unix seconds. See
// internal/dbschema/ducting_grid.go.

// UpsertDuctingGrid stores one forecast step. A row already stored for
// (model, validTime) is replaced only when runTime is strictly newer, in the
// one statement, so a concurrent or repeated fetch of an older run can never
// overwrite a newer one. written reports whether a row was inserted or
// replaced.
func (s *Store) UpsertDuctingGrid(model string, validTime, runTime int64, leadHours int, data []byte, createdAt int64) (written bool, err error) {
	if data == nil {
		data = []byte{} // a nil slice would bind as NULL and violate NOT NULL
	}
	err = s.WriterTx("ducting_grid_upsert", func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO ducting_grid (model, valid_time, run_time, lead_hours, data, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(model, valid_time) DO UPDATE SET
			  run_time = excluded.run_time,
			  lead_hours = excluded.lead_hours,
			  data = excluded.data,
			  created_at = excluded.created_at
			WHERE excluded.run_time > ducting_grid.run_time`,
			model, validTime, runTime, leadHours, data, createdAt)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		written = n > 0
		return nil
	})
	return written, err
}

// LatestDuctingRun returns the newest run_time stored for the model, 0 when
// it has no rows.
func (s *Store) LatestDuctingRun(model string) (int64, error) {
	var run int64
	err := s.WriterTx("ducting_grid_latest", func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COALESCE(MAX(run_time), 0) FROM ducting_grid WHERE model = ?`, model).Scan(&run)
	})
	return run, err
}

// DuctingRunLeads returns the lead hours stored for one run of the model,
// ascending.
func (s *Store) DuctingRunLeads(model string, runTime int64) ([]int, error) {
	var leads []int
	err := s.WriterTx("ducting_grid_leads", func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT lead_hours FROM ducting_grid WHERE model = ? AND run_time = ? ORDER BY lead_hours`, model, runTime)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l int
			if err := rows.Scan(&l); err != nil {
				return err
			}
			leads = append(leads, l)
		}
		return rows.Err()
	})
	return leads, err
}

// DuctingValidLeads returns lead_hours by valid_time for the model's rows
// with from <= valid_time < to.
func (s *Store) DuctingValidLeads(model string, from, to int64) (map[int64]int, error) {
	out := make(map[int64]int)
	err := s.WriterTx("ducting_grid_valid_leads", func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT valid_time, lead_hours FROM ducting_grid WHERE model = ? AND valid_time >= ? AND valid_time < ?`, model, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v int64
			var l int
			if err := rows.Scan(&v, &l); err != nil {
				return err
			}
			out[v] = l
		}
		return rows.Err()
	})
	return out, err
}

// PruneDuctingGrid deletes the model's steps with valid_time < before and
// returns how many rows went.
func (s *Store) PruneDuctingGrid(model string, before int64) (int64, error) {
	var n int64
	err := s.WriterTx("ducting_grid_prune", func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM ducting_grid WHERE model = ? AND valid_time < ?`, model, before)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
