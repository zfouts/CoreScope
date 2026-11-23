package main

import "database/sql"

// ducting_grid read path. The table is created and written by the ingestor
// only (cmd/ingestor/ducting_store.go); the server just SELECTs. A missing
// table (ingestor not yet upgraded, or ducting off) reads as empty.

// ductingGridRows lists the model's stored steps without their blobs,
// ascending by valid_time. (nil, nil) when the table does not exist.
func (db *DB) ductingGridRows(model string) ([]ductingGridRow, error) {
	if !db.tablePresent("ducting_grid") {
		return nil, nil
	}
	rows, err := db.conn.Query(`SELECT valid_time, run_time, lead_hours, created_at
		FROM ducting_grid WHERE model = ? ORDER BY valid_time ASC`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ductingGridRow
	for rows.Next() {
		var r ductingGridRow
		if err := rows.Scan(&r.ValidTime, &r.RunTime, &r.LeadHours, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ductingGridBlob returns the encoded grid of one step and its run_time.
// sql.ErrNoRows when the step (or the table) is absent.
func (db *DB) ductingGridBlob(model string, validTime int64) (data []byte, runTime int64, err error) {
	if !db.tablePresent("ducting_grid") {
		return nil, 0, sql.ErrNoRows
	}
	err = db.conn.QueryRow(`SELECT data, run_time FROM ducting_grid WHERE model = ? AND valid_time = ?`,
		model, validTime).Scan(&data, &runTime)
	return data, runTime, err
}
