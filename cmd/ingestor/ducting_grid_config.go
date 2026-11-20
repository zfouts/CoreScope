package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DuctingConfig is the part of the shared `ducting` config block the
// ingestor reads: whether the global GFS grid job runs, how much history it
// keeps, how far ahead it computes and where it downloads from. The full
// block (and its semantics) is DuctingConfig in cmd/server/ducting_types.go;
// everything else in it belongs to the server.
type DuctingConfig struct {
	Enabled       bool   `json:"enabled"`
	Source        string `json:"source,omitempty"`        // "gfs-grid" (default) or "open-meteo"
	HistoryDays   int    `json:"historyDays,omitempty"`   // default 7, allowed 1..30
	ForecastHours int    `json:"forecastHours,omitempty"` // default 48; grid: multiple of 3, 3..120
	GridBaseURL   string `json:"gridBaseUrl,omitempty"`   // default ductingGridDefaultBaseURL
}

// Ducting grid config defaults and limits.
const (
	ductingSourceGrid      = "gfs-grid"
	ductingSourceOpenMeteo = "open-meteo"

	ductingGridDefaultBaseURL       = "https://noaa-gfs-bdp-pds.s3.amazonaws.com"
	ductingGridDefaultHistoryDays   = 7
	ductingGridMaxHistoryDays       = 30
	ductingGridDefaultForecastHours = 48
	ductingGridMaxForecastHours     = 120
	ductingGridStepHours            = 3
)

// ductingGridSettings is the resolved, validated job configuration.
type ductingGridSettings struct {
	HistoryDays   int
	ForecastHours int // multiple of ductingGridStepHours
	BaseURL       string
}

// DuctingGridEnabled reports whether the ingestor runs the global grid job:
// ducting on with source gfs-grid (the default source).
func (c *Config) DuctingGridEnabled() bool {
	if c == nil || c.Ducting == nil || !c.Ducting.Enabled {
		return false
	}
	return ductingSourceOf(c.Ducting.Source) == ductingSourceGrid
}

// ductingSourceOf maps the configured source to a known one; empty or
// unknown values mean the default, gfs-grid (resolved warns about unknown).
func ductingSourceOf(s string) string {
	if strings.TrimSpace(s) == ductingSourceOpenMeteo {
		return ductingSourceOpenMeteo
	}
	return ductingSourceGrid
}

// resolved applies defaults and limits. Out-of-range values are clamped (or
// fall back to the default) with a warning naming the key. Warnings never
// echo gridBaseUrl.
func (c *DuctingConfig) resolved() (ductingGridSettings, []string) {
	var r ductingGridSettings
	var warns []string
	if c == nil {
		c = &DuctingConfig{}
	}
	if s := strings.TrimSpace(c.Source); s != "" && s != ductingSourceGrid && s != ductingSourceOpenMeteo {
		warns = append(warns, fmt.Sprintf("ducting.source %q is not supported, using %q", s, ductingSourceGrid))
	}

	switch h := c.HistoryDays; {
	case h == 0:
		r.HistoryDays = ductingGridDefaultHistoryDays
	case h < 1:
		warns = append(warns, "ducting.historyDays must be 1..30, using 1")
		r.HistoryDays = 1
	case h > ductingGridMaxHistoryDays:
		warns = append(warns, fmt.Sprintf("ducting.historyDays must be 1..30, using %d", ductingGridMaxHistoryDays))
		r.HistoryDays = ductingGridMaxHistoryDays
	default:
		r.HistoryDays = h
	}

	fh := c.ForecastHours
	switch {
	case fh == 0:
		fh = ductingGridDefaultForecastHours
	case fh < ductingGridStepHours:
		if fh < 0 {
			warns = append(warns, fmt.Sprintf("ducting.forecastHours must be 1..120, using %d for the grid", ductingGridStepHours))
		}
		fh = ductingGridStepHours
	case fh > ductingGridMaxForecastHours:
		warns = append(warns, fmt.Sprintf("ducting.forecastHours must be 1..120, using %d", ductingGridMaxForecastHours))
		fh = ductingGridMaxForecastHours
	}
	// GFS 0.5° steps are 3-hourly: round up so the requested horizon is covered.
	r.ForecastHours = (fh + ductingGridStepHours - 1) / ductingGridStepHours * ductingGridStepHours

	r.BaseURL = ductingGridDefaultBaseURL
	if c.GridBaseURL != "" {
		if u, ok := ductingGridBaseURL(c.GridBaseURL); ok {
			r.BaseURL = u
		} else {
			warns = append(warns, "ducting.gridBaseUrl must be an https URL with a host and no credentials, query or fragment (plain http only for a loopback host), using the default")
		}
	}
	return r, warns
}

// ductingGridBaseURL validates an operator-supplied NOAA mirror URL and
// trims trailing slashes. https with a host is required; plain http is
// accepted only for a loopback host (a local mirror, or a test server).
func ductingGridBaseURL(s string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", false
		}
	default:
		return "", false
	}
	return strings.TrimRight(u.String(), "/"), true
}

// isLoopbackHost reports whether host is localhost or a loopback IP
// (127.0.0.0/8, ::1). Names other than "localhost" are not resolved.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
