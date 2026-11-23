package grib2

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const sampleIdx = `1:0:d=2026111918:PRMSL:mean sea level:6 hour fcst:
2:1002345:d=2026111918:TMP:850 mb:6 hour fcst:
3:1100000:d=2026111918:RH:2 m above ground:6 hour fcst:
4:1200000:d=2026111918:APCP:surface:0-6 hour acc fcst:prob >2.5:
`

func TestParseIndex(t *testing.T) {
	es, err := ParseIndex([]byte(sampleIdx))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 4 {
		t.Fatalf("%d entries", len(es))
	}
	want := IndexEntry{Num: 2, Offset: 1002345, RefDate: "2026111918", Variable: "TMP", Level: "850 mb", Forecast: "6 hour fcst"}
	if es[1] != want {
		t.Errorf("entry %+v, want %+v", es[1], want)
	}
	if es[3].Forecast != "0-6 hour acc fcst" {
		t.Errorf("extra descriptor changed forecast: %q", es[3].Forecast)
	}

	// CRLF line endings and no final newline are accepted.
	crlf := strings.TrimSuffix(strings.ReplaceAll(sampleIdx, "\n", "\r\n"), "\r\n")
	es2, err := ParseIndex([]byte(crlf))
	if err != nil || len(es2) != 4 || es2[2] != es[2] {
		t.Fatalf("CRLF: %v %+v", err, es2)
	}
}

func TestParseIndexRealFixture(t *testing.T) {
	es, err := ParseIndex(readFixture(t, "gfs.t18z.pgrb2.1p00.f006.idx"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range es {
		if e.Variable == "TMP" && e.Level == "850 mb" {
			found = true
			if e.Num != 462 || e.Offset != 28302725 || e.RefDate != "2026111918" || e.Forecast != "6 hour fcst" {
				t.Errorf("TMP 850 mb entry %+v", e)
			}
		}
	}
	if !found {
		t.Error("TMP 850 mb not found")
	}
}

func TestParseIndexRejects(t *testing.T) {
	good := "1:0:d=2026111918:TMP:850 mb:6 hour fcst:\n"
	cases := map[string]string{
		"empty":                "",
		"only newline":         "\n",
		"blank line":           good + "\n" + "2:10:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"too few fields":       "1:0:d=2026111918:TMP:850 mb:\n",
		"no trailing colon":    "1:0:d=2026111918:TMP:850 mb:6 hour fcst\n",
		"bad number":           "x:0:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"zero number":          "0:0:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"sub-message number":   "1.1:0:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"signed number":        "+1:0:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"negative offset":      "1:-5:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"offset overflow":      "1:99999999999999999999:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"empty offset":         "1::d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"bad date prefix":      "1:0:D=2026111918:TMP:850 mb:6 hour fcst:\n",
		"short date":           "1:0:d=20261119:TMP:850 mb:6 hour fcst:\n",
		"non-digit date":       "1:0:d=20261119xx:TMP:850 mb:6 hour fcst:\n",
		"empty variable":       "1:0:d=2026111918::850 mb:6 hour fcst:\n",
		"empty level":          "1:0:d=2026111918:TMP::6 hour fcst:\n",
		"empty forecast":       "1:0:d=2026111918:TMP:850 mb::\n",
		"offsets not increase": good + "2:0:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"offsets go backwards": "1:10:d=2026111918:TMP:850 mb:6 hour fcst:\n2:5:d=2026111918:TMP:850 mb:6 hour fcst:\n",
		"garbage after good":   good + "hello\n",
		"binary":               "\x00\x01\x02",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if es, err := ParseIndex([]byte(in)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v, %v; want ErrCorrupt", es, err)
			}
		})
	}
}

func TestParseIndexLimits(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= MaxIndexLines; i++ {
		fmt.Fprintf(&b, "%d:%d:d=2026111918:TMP:850 mb:6 hour fcst:\n", i, i*100)
	}
	if es, err := ParseIndex([]byte(b.String())); err != nil || len(es) != MaxIndexLines {
		t.Fatalf("%d lines: %v (%d entries)", MaxIndexLines, err, len(es))
	}
	fmt.Fprintf(&b, "%d:%d:d=2026111918:TMP:850 mb:6 hour fcst:\n", MaxIndexLines+1, (MaxIndexLines+1)*100)
	if _, err := ParseIndex([]byte(b.String())); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%d lines: %v, want ErrTooLarge", MaxIndexLines+1, err)
	}
	big := make([]byte, maxIndexBytes+1)
	if _, err := ParseIndex(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized index: %v, want ErrTooLarge", err)
	}
}

func TestIndexEntryRange(t *testing.T) {
	e := IndexEntry{Offset: 100}
	cases := []struct {
		name       string
		next       *IndexEntry
		size       int64
		start, end int64
		ok         bool
	}{
		{"middle", &IndexEntry{Offset: 250}, 0, 100, 249, true},
		{"one byte", &IndexEntry{Offset: 101}, 0, 100, 100, true},
		{"empty", &IndexEntry{Offset: 100}, 0, 0, 0, false},
		{"backwards", &IndexEntry{Offset: 50}, 1000, 0, 0, false},
		{"last entry", nil, 1000, 100, 999, true},
		{"last entry size unknown", nil, 0, 0, 0, false},
		{"last entry negative size", nil, -1, 0, 0, false},
		{"last entry past end", nil, 100, 0, 0, false},
		{"exactly max", &IndexEntry{Offset: 100 + MaxMessageBytes}, 0, 100, 99 + MaxMessageBytes, true},
		{"over max", &IndexEntry{Offset: 101 + MaxMessageBytes}, 0, 0, 0, false},
		{"last over max", nil, 101 + MaxMessageBytes, 0, 0, false},
	}
	for _, tc := range cases {
		s, en, ok := e.Range(tc.next, tc.size)
		if s != tc.start || en != tc.end || ok != tc.ok {
			t.Errorf("%s: got (%d, %d, %v), want (%d, %d, %v)", tc.name, s, en, ok, tc.start, tc.end, tc.ok)
		}
	}
	if _, _, ok := (IndexEntry{Offset: -1}).Range(nil, 100); ok {
		t.Error("negative offset accepted")
	}
}

// TestIndexRangeMatchesFixtures checks that the idx ranges of the real
// fixtures cut out exactly the messages that were downloaded with them.
func TestIndexRangeMatchesFixtures(t *testing.T) {
	es, err := ParseIndex(readFixture(t, "gfs.t18z.pgrb2.1p00.f006.idx"))
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{
		"TMP/850 mb":          int64(len(readFixture(t, "tmp850_drt53.grib2"))),
		"RH/2 m above ground": int64(len(readFixture(t, "rh2m_drt53.grib2"))),
		"HGT/surface":         int64(len(readFixture(t, "hgtsfc_drt53.grib2"))),
		"WEASD/surface":       int64(len(readFixture(t, "weasd_drt53_bitmap.grib2"))),
	}
	for i := range es[:len(es)-1] {
		want, ok := sizes[es[i].Variable+"/"+es[i].Level]
		if !ok || es[i].Forecast != "6 hour fcst" {
			continue
		}
		s, e, ok := es[i].Range(&es[i+1], 0)
		if !ok || e-s+1 != want {
			t.Errorf("%s %s: range (%d, %d, %v), fixture is %d bytes", es[i].Variable, es[i].Level, s, e, ok, want)
		}
	}
}
