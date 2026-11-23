package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	ducting "github.com/meshcore-analyzer/ducting"
)

// The conditional analysis of /api/ducting/links: (listed pair, stored
// step) combinations binned by path mean gradient, heard or not, overall and
// over the intermittent pairs only (ducting_links.go). Pubkeys are
// placeholder hex only.

// An edge value is in the more negative bin, as on the map legend: the
// bins are > -40, (-50, -40], (-62, -50], <= -62.
func TestDuctingGradientBinEdges(t *testing.T) {
	for _, c := range []struct {
		n   float64
		bin int
	}{
		{-10, 0}, {-39.99, 0},
		{-40, 1}, {-49.99, 1},
		{-50, 2}, {-61.99, 2},
		{-62, 3}, {ducting.HighGradientNk, 3}, {-200, 3},
	} {
		if got := ductingGradientBin(c.n); got != c.bin {
			t.Errorf("bin(%v) = %d, want %d", c.n, got, c.bin)
		}
	}
	b := ductingGradientBins()
	want := [][2]any{{nil, -40.0}, {-40.0, -50.0}, {-50.0, -62.0}, {-62.0, nil}}
	for i, w := range want {
		if deref(b[i].FromNkm) != w[0] || deref(b[i].ToNkm) != w[1] {
			t.Errorf("bin %d edges %v..%v, want %v..%v", i, deref(b[i].FromNkm), deref(b[i].ToNkm), w[0], w[1])
		}
	}
}

func gradCombos(spec string) []ductingGradientCombo {
	// spec: one "<bin><h|.>" per combination, e.g. "0h1.3h".
	var out []ductingGradientCombo
	for i := 0; i+1 < len(spec); i += 2 {
		out = append(out, ductingGradientCombo{bin: spec[i] - '0', heard: spec[i+1] == 'h'})
	}
	return out
}

func binCounts(b []DuctingGradientBin) [][2]int {
	out := make([][2]int, len(b))
	for i := range b {
		out[i] = [2]int{b[i].PathSteps, b[i].HeardSteps}
	}
	return out
}

// Persistent (>= 90% of its steps heard) pairs count only in byGradient;
// intermittent ones in both; a pair never heard is in neither split.
func TestDuctingGradientSummarySplit(t *testing.T) {
	perPair := [][]ductingGradientCombo{
		gradCombos("0h0h1h1h2h2h3h3h3h3."), // 9 of 10: exactly 90%, persistent
		gradCombos("0h0.1h1.2.2.3h3.3.3."), // 3 of 10: intermittent
		gradCombos("0.1.2.3."),             // never heard
		nil,                                // no step with data
	}
	all, inter, n := ductingGradientSummary(perPair)
	if want := [][2]int{{5, 3}, {5, 3}, {5, 2}, {9, 4}}; !equalCounts(binCounts(all), want) {
		t.Errorf("all = %v, want %v", binCounts(all), want)
	}
	if want := [][2]int{{2, 1}, {2, 1}, {2, 0}, {4, 1}}; !equalCounts(binCounts(inter), want) || n != 1 {
		t.Errorf("intermittent = %v (%d pairs), want %v (1)", binCounts(inter), n, want)
	}
	// Just under 90%: intermittent.
	_, _, n = ductingGradientSummary([][]ductingGradientCombo{gradCombos(strings.Repeat("0h", 89) + strings.Repeat("0.", 11))})
	if n != 1 {
		t.Errorf("89%% heard: intermittent pairs %d, want 1", n)
	}
}

func equalCounts(a, b [][2]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// End to end over hand-built steps: one per bin, a step without N values
// and a step without data (both skipped), a step before the period (not a
// combination), a persistent and an intermittent pair.
func TestDuctingLinksByGradient(t *testing.T) {
	h := int64(3600)
	now := linksNow.Unix()
	s1, s2, s3, s4 := now-12*h, now-9*h, now-6*h, now-3*h
	sNoN, sNoData, sOld := now-15*h, now-18*h, now-9*86400
	src := newLinksFakeSrc()
	uniform := func(vt int64, r ducting.Result) { src.add(linksGrid(t, 0, vt, linksAll(r))) }
	uniform(s1, ducting.Result{Class: ducting.ClassNormal, MeanGradNkm: -30}) // bin 0
	uniform(s2, ducting.Result{Class: ducting.ClassNormal, MeanGradNkm: -40}) // bin 1 (edge)
	uniform(s3, ducting.Result{Class: ducting.ClassNormal, MeanGradNkm: -50}) // bin 2 (edge)
	uniform(s4, ducting.Result{Class: ducting.ClassSuper, MeanGradNkm: -62})  // bin 3 (edge)
	uniform(sNoN, linksNormal)                                                // data, no N: skipped
	uniform(sNoData, linksNoData)                                             // no data: skipped
	uniform(sOld, ducting.Result{Class: ducting.ClassNormal, MeanGradNkm: -90})

	// P (intermittent): heard at s1 and s3 of its 4 steps with N.
	// Q (persistent): heard at every step, the skipped ones and the old one too.
	in := linksIn(linksEnd(lpA, 50, 0, 5), linksEnd(lpB, 50, 3, 5), linksEnd(lpC, 45, 0, 5), linksEnd(lpD, 45, 3, 5))
	id := 0
	hear := func(a, b string, at int64) { id++; in.Recv = append(in.Recv, linksRecv(id, at+600, a, b)) }
	hear(lpA, lpB, s1)
	hear(lpA, lpB, s3)
	hear(lpA, lpB, s3+1200) // a second reception in the same step: still one heard step
	for _, s := range []int64{s1, s2, s3, s4, sNoN, sNoData, sOld} {
		hear(lpC, lpD, s)
	}
	sum := mustBuild(t, in, src).Summary
	if want := [][2]int{{2, 2}, {2, 1}, {2, 2}, {2, 1}}; !equalCounts(binCounts(sum.ByGradient), want) {
		t.Errorf("byGradient = %v, want %v", binCounts(sum.ByGradient), want)
	}
	if want := [][2]int{{1, 1}, {1, 0}, {1, 1}, {1, 0}}; !equalCounts(binCounts(sum.ByGradientIntermittent), want) || sum.IntermittentPairs != 1 {
		t.Errorf("intermittent = %v (%d pairs), want %v (1)", binCounts(sum.ByGradientIntermittent), sum.IntermittentPairs, want)
	}
}

// JSON names, and the four empty bins of an answer without data.
func TestDuctingLinksByGradientJSON(t *testing.T) {
	b, err := json.Marshal(ductingLinksEmptySummary("smooth-earth"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		`"byGradient":[{"fromNkm":null,"toNkm":-40,"pathSteps":0,"heardSteps":0},{"fromNkm":-40,"toNkm":-50,`,
		`{"fromNkm":-62,"toNkm":null,"pathSteps":0,"heardSteps":0}]`,
		`"intermittentPairs":0`, `"byGradientIntermittent":[{"fromNkm":null`,
	} {
		if !strings.Contains(string(b), f) {
			t.Errorf("summary lacks %s: %s", f, b)
		}
	}
	srv := &Server{cfg: &Config{}}
	st := newDuctingLinksState()
	st.now = func() time.Time { return linksNow }
	r := linksDecode(t, linksGet(st, srv, newLinksFakeSrc(), "", 7))
	if len(r.Summary.ByGradient) != 4 || len(r.Summary.ByGradientIntermittent) != 4 {
		t.Errorf("pending bins: %+v", r.Summary)
	}
}
