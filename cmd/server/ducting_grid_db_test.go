package main

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/meshcore-analyzer/dbschema"
)

func seedDuctingGrid(t *testing.T, d *dualDB) {
	t.Helper()
	for _, r := range []struct {
		model      string
		valid, run int64
		lead       int
		data       []byte
	}{
		{"gfs", 7200, 100, 3, []byte{0, 1, 2, 0, 255}},
		{"gfs", 3600, 100, 0, []byte("a")},
		{"gfs", 10800, 200, 6, []byte{}},
		{"other", 3600, 50, 0, []byte("z")},
	} {
		d.exec(t, `INSERT INTO ducting_grid (model, valid_time, run_time, lead_hours, data, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			r.model, r.valid, r.run, r.lead, r.data, r.run+1)
	}
}

func TestDuctingGridRows(t *testing.T) {
	d := newDualTestDB(t)
	seedDuctingGrid(t, d)
	got := requireSame(t, d, "ductingGridRows", func(db *DB) ([]ductingGridRow, error) { return db.ductingGridRows("gfs") })
	want := []ductingGridRow{
		{ValidTime: 3600, RunTime: 100, LeadHours: 0, CreatedAt: 101},
		{ValidTime: 7200, RunTime: 100, LeadHours: 3, CreatedAt: 101},
		{ValidTime: 10800, RunTime: 200, LeadHours: 6, CreatedAt: 201},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	none := requireSame(t, d, "unknown model", func(db *DB) ([]ductingGridRow, error) { return db.ductingGridRows("nope") })
	if len(none) != 0 {
		t.Fatalf("unknown model rows = %v", none)
	}
}

func TestDuctingGridBlob(t *testing.T) {
	d := newDualTestDB(t)
	seedDuctingGrid(t, d)
	type res struct {
		Data []byte
		Run  int64
	}
	got := requireSame(t, d, "ductingGridBlob", func(db *DB) (res, error) {
		data, run, err := db.ductingGridBlob("gfs", 7200)
		return res{data, run}, err
	})
	if !bytes.Equal(got.Data, []byte{0, 1, 2, 0, 255}) || got.Run != 100 {
		t.Fatalf("blob = %v run %d", got.Data, got.Run)
	}
	for name, db := range map[string]*DB{"sqlite": d.sqlite, "postgres": d.pg} {
		if _, _, err := db.ductingGridBlob("gfs", 1); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s absent step: err = %v, want sql.ErrNoRows", name, err)
		}
	}
}

// A database the ingestor has not created ducting_grid in reads as empty.
func TestDuctingGridMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	rw, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(dbschema.SQLiteEndState); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`DROP TABLE ducting_grid`); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.conn.Close()
	rows, err := db.ductingGridRows("gfs")
	if rows != nil || err != nil {
		t.Fatalf("rows = %v, err = %v; want nil, nil", rows, err)
	}
	if _, _, err := db.ductingGridBlob("gfs", 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("blob err = %v, want sql.ErrNoRows", err)
	}
}
