package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

// ducting_grid store: runs on SQLite and, when $CORESCOPE_TEST_PG_DSN is set,
// on PostgreSQL.

func forEachDuctingBackend(t *testing.T, f func(t *testing.T, s *Store)) {
	t.Run("sqlite", func(t *testing.T) {
		s, err := OpenStore(filepath.Join(t.TempDir(), "d.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		f(t, s)
	})
	t.Run("postgres", func(t *testing.T) {
		f(t, openPostgresTestStore(t, filepath.Join(t.TempDir(), "d.db")))
	})
}

func ductingRow(t *testing.T, s *Store, model string, valid int64) (run int64, lead int, data []byte, created int64, found bool) {
	t.Helper()
	err := s.db.QueryRow(`SELECT run_time, lead_hours, data, created_at FROM ducting_grid WHERE model = ? AND valid_time = ?`,
		model, valid).Scan(&run, &lead, &data, &created)
	return run, lead, data, created, err == nil
}

func TestDuctingGridUpsertNewerReplaces(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		w, err := s.UpsertDuctingGrid("gfs", 1000, 100, 3, []byte("old"), 5)
		if err != nil || !w {
			t.Fatalf("insert: written=%v err=%v", w, err)
		}
		w, err = s.UpsertDuctingGrid("gfs", 1000, 200, 6, []byte("new"), 9)
		if err != nil || !w {
			t.Fatalf("newer: written=%v err=%v", w, err)
		}
		run, lead, data, created, ok := ductingRow(t, s, "gfs", 1000)
		if !ok || run != 200 || lead != 6 || string(data) != "new" || created != 9 {
			t.Fatalf("row = %d %d %q %d found=%v", run, lead, data, created, ok)
		}
	})
}

func TestDuctingGridUpsertOlderOrEqualKeeps(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		if _, err := s.UpsertDuctingGrid("gfs", 1000, 200, 3, []byte("keep"), 5); err != nil {
			t.Fatal(err)
		}
		for _, run := range []int64{199, 200} {
			w, err := s.UpsertDuctingGrid("gfs", 1000, run, 9, []byte("lose"), 99)
			if err != nil || w {
				t.Fatalf("run %d: written=%v err=%v, want false", run, w, err)
			}
		}
		gotRun, lead, data, created, _ := ductingRow(t, s, "gfs", 1000)
		if gotRun != 200 || lead != 3 || string(data) != "keep" || created != 5 {
			t.Fatalf("row changed: %d %d %q %d", gotRun, lead, data, created)
		}
	})
}

func TestDuctingGridModelsAreSeparate(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		s.UpsertDuctingGrid("gfs", 1000, 100, 0, []byte("a"), 1)
		w, err := s.UpsertDuctingGrid("other", 1000, 50, 0, []byte("b"), 1)
		if err != nil || !w {
			t.Fatalf("other model: written=%v err=%v", w, err)
		}
		if run, _ := s.LatestDuctingRun("gfs"); run != 100 {
			t.Fatalf("gfs latest = %d", run)
		}
	})
}

func TestDuctingGridLatestRun(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		if run, err := s.LatestDuctingRun("gfs"); err != nil || run != 0 {
			t.Fatalf("empty: %d %v", run, err)
		}
		s.UpsertDuctingGrid("gfs", 1000, 100, 0, []byte("a"), 1)
		s.UpsertDuctingGrid("gfs", 2000, 300, 0, []byte("b"), 1)
		s.UpsertDuctingGrid("gfs", 3000, 200, 0, []byte("c"), 1)
		if run, err := s.LatestDuctingRun("gfs"); err != nil || run != 300 {
			t.Fatalf("latest = %d %v, want 300", run, err)
		}
	})
}

func TestDuctingGridPrune(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		for _, v := range []int64{1000, 2000, 3000} {
			s.UpsertDuctingGrid("gfs", v, 1, 0, []byte("x"), 1)
		}
		s.UpsertDuctingGrid("other", 1000, 1, 0, []byte("x"), 1)
		n, err := s.PruneDuctingGrid("gfs", 3000)
		if err != nil || n != 2 {
			t.Fatalf("pruned %d %v, want 2", n, err)
		}
		if _, _, _, _, ok := ductingRow(t, s, "gfs", 3000); !ok {
			t.Error("valid_time == before must survive")
		}
		if _, _, _, _, ok := ductingRow(t, s, "other", 1000); !ok {
			t.Error("other model's row pruned")
		}
		if n, _ := s.PruneDuctingGrid("gfs", 3000); n != 0 {
			t.Errorf("second prune deleted %d", n)
		}
	})
}

func TestDuctingGridBlobRoundTrip(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		all := make([]byte, 0, 600)
		for i := 0; i < 600; i++ {
			all = append(all, byte(i))
		}
		for name, blob := range map[string][]byte{
			"all-bytes":   all,
			"zeros":       {0, 0, 0, 0},
			"leading-nul": {0, 'a', 0},
			"empty":       {},
			"nil":         nil,
		} {
			if _, err := s.UpsertDuctingGrid(name, 1, 1, 0, blob, 1); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			_, _, got, _, ok := ductingRow(t, s, name, 1)
			if !ok || !bytes.Equal(got, blob) {
				t.Errorf("%s: got %v want %v (found=%v)", name, got, blob, ok)
			}
		}
	})
}

func TestDuctingGridRunLeads(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		leads, err := s.DuctingRunLeads("gfs", 100)
		if err != nil || len(leads) != 0 {
			t.Fatalf("empty: %v %v", leads, err)
		}
		for _, r := range []struct {
			valid, run int64
			lead       int
		}{{106, 100, 6}, {100, 100, 0}, {103, 100, 3}, {109, 200, 9}} {
			if _, err := s.UpsertDuctingGrid("gfs", r.valid, r.run, r.lead, []byte("x"), 1); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.UpsertDuctingGrid("icon", 112, 100, 12, []byte("x"), 1); err != nil {
			t.Fatal(err)
		}
		leads, err = s.DuctingRunLeads("gfs", 100)
		if err != nil || len(leads) != 3 || leads[0] != 0 || leads[1] != 3 || leads[2] != 6 {
			t.Fatalf("leads %v %v", leads, err)
		}
	})
}

func TestDuctingGridValidLeads(t *testing.T) {
	forEachDuctingBackend(t, func(t *testing.T, s *Store) {
		for _, r := range []struct {
			valid, run int64
			lead       int
		}{{100, 100, 0}, {103, 94, 9}, {106, 106, 0}, {99, 99, 0}} {
			if _, err := s.UpsertDuctingGrid("gfs", r.valid, r.run, r.lead, []byte("x"), 1); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.UpsertDuctingGrid("icon", 101, 101, 0, []byte("x"), 1); err != nil {
			t.Fatal(err)
		}
		got, err := s.DuctingValidLeads("gfs", 100, 106)
		if err != nil || len(got) != 2 || got[100] != 0 || got[103] != 9 {
			t.Fatalf("leads %v %v", got, err)
		}
	})
}
