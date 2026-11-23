package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDuctingGridConfigDefaults(t *testing.T) {
	for _, c := range []*DuctingConfig{nil, {}, {Enabled: true}} {
		r, warns := c.resolved()
		if len(warns) != 0 {
			t.Fatalf("warnings %v", warns)
		}
		if r.HistoryDays != 7 || r.ForecastHours != 48 || r.BaseURL != "https://noaa-gfs-bdp-pds.s3.amazonaws.com" {
			t.Fatalf("defaults %+v", r)
		}
	}
}

func TestDuctingGridConfigClamps(t *testing.T) {
	for _, tc := range []struct {
		history, forecast int
		wantH, wantF      int
		warn              bool
	}{
		{1, 1, 1, 3, false},
		{30, 120, 30, 120, false},
		{31, 121, 30, 120, true},
		{-1, -5, 1, 3, true},
		{5, 49, 5, 51, false},
		{5, 50, 5, 51, false},
		{5, 51, 5, 51, false},
		{5, 4, 5, 6, false},
	} {
		r, warns := (&DuctingConfig{HistoryDays: tc.history, ForecastHours: tc.forecast}).resolved()
		if r.HistoryDays != tc.wantH || r.ForecastHours != tc.wantF || (len(warns) > 0) != tc.warn {
			t.Errorf("history %d forecast %d -> %+v %v", tc.history, tc.forecast, r, warns)
		}
	}
}

func TestDuctingGridConfigBaseURL(t *testing.T) {
	const def = ductingGridDefaultBaseURL
	for _, tc := range []struct{ in, want string }{
		{"https://mirror.example.org/gfs/", "https://mirror.example.org/gfs"},
		{"https://mirror.example.org:8443", "https://mirror.example.org:8443"},
		{"http://127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"http://127.8.9.10", "http://127.8.9.10"},
		{"http://[::1]:8080/", "http://[::1]:8080"},
		{"http://localhost:8080", "http://localhost:8080"},
		{"http://mirror.example.org", def},              // plain http off loopback
		{"http://10.0.0.5", def},                        // private but not loopback
		{"http://localhost.example.org", def},           // not localhost
		{"https://user:secret@mirror.example.org", def}, // credentials
		{"https://mirror.example.org/?token=x", def},    // query
		{"https://mirror.example.org/#frag", def},       // fragment
		{"https:///nohost", def},
		{"ftp://mirror.example.org", def},
		{"mirror.example.org", def},
		{"file:///etc/passwd", def},
	} {
		r, warns := (&DuctingConfig{GridBaseURL: tc.in}).resolved()
		if r.BaseURL != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, r.BaseURL, tc.want)
		}
		if (tc.want == def) != (len(warns) == 1) {
			t.Errorf("%q: warnings %v", tc.in, warns)
		}
		for _, w := range warns {
			if strings.Contains(w, "secret") || strings.Contains(w, "example") || strings.Contains(w, "token") {
				t.Errorf("warning echoes the URL: %q", w)
			}
		}
	}
}

func TestDuctingGridConfigEnabled(t *testing.T) {
	for _, tc := range []struct {
		json string
		want bool
	}{
		{`{}`, false},
		{`{"ducting":{"enabled":false}}`, false},
		{`{"ducting":{"enabled":true}}`, true},
		{`{"ducting":{"enabled":true,"source":"gfs-grid"}}`, true},
		{`{"ducting":{"enabled":true,"source":"open-meteo"}}`, false},
		{`{"ducting":{"enabled":true,"source":"bogus"}}`, true},
		{`{"ducting":{"enabled":true,"historyDays":3,"forecastHours":24,"gridBaseUrl":"https://m.example.org","model":"icon","maxPoints":5}}`, true},
	} {
		var c Config
		if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
			t.Fatal(err)
		}
		if got := c.DuctingGridEnabled(); got != tc.want {
			t.Errorf("%s: enabled %v, want %v", tc.json, got, tc.want)
		}
	}
	var c Config
	_ = json.Unmarshal([]byte(`{"ducting":{"enabled":true,"historyDays":3,"forecastHours":24,"gridBaseUrl":"https://m.example.org"}}`), &c)
	if r, _ := c.Ducting.resolved(); r.HistoryDays != 3 || r.ForecastHours != 24 || r.BaseURL != "https://m.example.org" {
		t.Fatalf("parsed %+v", r)
	}
	if _, warns := (&DuctingConfig{Source: "bogus"}).resolved(); len(warns) != 1 || !strings.Contains(warns[0], "ducting.source") {
		t.Fatalf("unknown source warnings %v", warns)
	}
}
