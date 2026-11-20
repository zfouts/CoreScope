package ducting

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// HighGradientNk must equal the start of the map legend's "High" band in
// public/map-ducting.js (STOP_N[4], labelled "High" via BAR_POS[4]); the
// long-link analysis and the map would otherwise disagree about what
// "High" means.
func TestHighGradientMatchesMapLegend(t *testing.T) {
	src, err := os.ReadFile("../../public/map-ducting.js")
	if err != nil {
		t.Skipf("frontend not in this tree: %v", err)
	}
	m := regexp.MustCompile(`var STOP_N = \[([^\]]*)\]`).FindSubmatch(src)
	if m == nil {
		t.Fatal("STOP_N not found in public/map-ducting.js")
	}
	parts := strings.Split(string(m[1]), ",")
	if len(parts) < 5 {
		t.Fatalf("STOP_N has %d stops", len(parts))
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64)
	if err != nil {
		t.Fatal(err)
	}
	if v != HighGradientNk {
		t.Fatalf("STOP_N[4] = %v, HighGradientNk = %v", v, HighGradientNk)
	}
	if !regexp.MustCompile(`\[BAR_POS\[4\], 'High'\]`).Match(src) {
		t.Fatal("the legend's 'High' label no longer sits at BAR_POS[4]")
	}
}
