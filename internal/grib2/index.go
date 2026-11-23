package grib2

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const maxIndexBytes = 1 << 20 // .idx inventories larger than this are refused

// ParseIndex parses a wgrib2 .idx inventory. Each line must look like
// "num:offset:d=YYYYMMDDHH:VAR:LEVEL:FCST:" (further colon-separated
// descriptors before the trailing colon are allowed and ignored). Offsets
// must be non-negative and strictly increasing. A malformed line is an
// error, never skipped.
func ParseIndex(idx []byte) ([]IndexEntry, error) {
	if len(idx) > maxIndexBytes {
		return nil, fmt.Errorf("%w: index is %d bytes", ErrTooLarge, len(idx))
	}
	text := bytes.TrimSuffix(idx, []byte("\n"))
	if len(text) == 0 {
		return nil, fmt.Errorf("%w: empty index", ErrCorrupt)
	}
	if bytes.Count(text, []byte("\n")) >= MaxIndexLines {
		return nil, fmt.Errorf("%w: index has more than %d lines", ErrTooLarge, MaxIndexLines)
	}
	lines := bytes.Split(text, []byte("\n"))
	out := make([]IndexEntry, 0, len(lines))
	for i, line := range lines {
		e, err := parseIndexLine(string(bytes.TrimSuffix(line, []byte("\r"))))
		if err != nil {
			return nil, fmt.Errorf("%w: index line %d: %v", ErrCorrupt, i+1, err)
		}
		if len(out) > 0 && e.Offset <= out[len(out)-1].Offset {
			return nil, fmt.Errorf("%w: index line %d: offset %d does not increase", ErrCorrupt, i+1, e.Offset)
		}
		out = append(out, e)
	}
	return out, nil
}

func parseIndexLine(line string) (IndexEntry, error) {
	var e IndexEntry
	parts := strings.Split(line, ":")
	if len(parts) < 7 || parts[len(parts)-1] != "" {
		return e, fmt.Errorf("expected num:offset:d=date:var:level:forecast:")
	}
	if !allDigits(parts[0]) || !allDigits(parts[1]) {
		return e, fmt.Errorf("message number and offset must be decimal digits")
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil || num < 1 {
		return e, fmt.Errorf("bad message number")
	}
	off, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || off < 0 {
		return e, fmt.Errorf("bad offset")
	}
	date := parts[2]
	if len(date) != 12 || date[:2] != "d=" || !allDigits(date[2:]) {
		return e, fmt.Errorf("bad reference date")
	}
	if parts[3] == "" || parts[4] == "" || parts[5] == "" {
		return e, fmt.Errorf("empty variable, level or forecast")
	}
	e.Num, e.Offset, e.RefDate = num, off, date[2:]
	e.Variable, e.Level, e.Forecast = parts[3], parts[4], parts[5]
	return e, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Range returns the inclusive byte range of the message e describes: up to
// the byte before next's offset, or to the end of a file of fileSize bytes
// when e is the last entry (next == nil; fileSize <= 0 means unknown, so
// ok is false). ok is false when the range is empty or larger than
// MaxMessageBytes.
func (e IndexEntry) Range(next *IndexEntry, fileSize int64) (start, end int64, ok bool) {
	if e.Offset < 0 {
		return 0, 0, false
	}
	start = e.Offset
	switch {
	case next != nil:
		end = next.Offset - 1
	case fileSize > 0:
		end = fileSize - 1
	default:
		return 0, 0, false
	}
	if end < start || end-start+1 > MaxMessageBytes {
		return 0, 0, false
	}
	return start, end, true
}
