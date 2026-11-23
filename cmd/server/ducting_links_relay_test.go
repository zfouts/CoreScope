package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Relay evidence of /api/ducting/links: consecutive certain hops of flood
// paths (ducting_links.go, ductingRelayLinkReceptions). Pubkeys are
// placeholder hex only (AGENTS.md PII rule).

// relayPK is a pubkey whose first byte is p, so 1-byte hops can target it.
func relayPK(p, n int) string { return fmt.Sprintf("%02x%062x", p, n) }

var (
	rO  = relayPK(0x0f, 1) // advert originator (companion: not path-capable)
	rR1 = relayPK(0x11, 2)
	rR2 = relayPK(0x22, 3)
	rR3 = relayPK(0x33, 4)
	rC1 = relayPK(0x44, 5) // rC1 and rC2 share the 1-byte prefix 44
	rC2 = relayPK(0x44, 6)
	rR5 = relayPK(0x55, 7)
)

func relayNodes() []nodeInfo {
	rep := func(pk string, lat, lon float64) nodeInfo {
		n := linksNode(pk, "r"+pk[:2], lat, lon)
		n.Role = "repeater"
		return n
	}
	o := linksNode(rO, "origin", 50, -2)
	o.Role = "companion"
	return []nodeInfo{o, rep(rR1, 50, 0), rep(rR2, 50, 2), rep(rR3, 50, 4), rep(rC1, 50, 6), rep(rC2, 50, 7), rep(rR5, 50, 8)}
}

// relayObs is an observation whose path is bound to its flood frame at
// 1-byte hops, as the store sets relayHopHexLen on load.
func relayObs(id int, observer, path string, at time.Time) *StoreObs {
	o := linksObs(id, observer, path, at)
	o.relayHopHexLen = 2
	return o
}

func relayTx(id, route, payload int, decoded string, obs ...*StoreObs) *StoreTx {
	tx := dhTx(route, payload, decoded)
	tx.ID = id
	tx.Observations = obs
	return tx
}

func relayAdvert(id int, origin, extra string, obs ...*StoreObs) *StoreTx {
	return relayTx(id, RouteFlood, PayloadADVERT, `{"type":"ADVERT","pubKey":"`+origin+`"`+extra+`}`, obs...)
}

type relayPair struct {
	id   int
	a, b string
}

func relayGather(t *testing.T, cfg *Config, txs ...*StoreTx) (ductingLinksInput, []relayPair) {
	t.Helper()
	srv := linksServer(cfg, relayNodes(), txs...)
	in, err := srv.ductingLinksInputFor(7, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []relayPair
	for _, r := range in.Recv {
		if r.via != ductingViaRelay {
			continue
		}
		got = append(got, relayPair{r.obsID, r.a, r.b})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].id != got[j].id {
			return got[i].id < got[j].id
		}
		if got[i].a != got[j].a {
			return got[i].a < got[j].a
		}
		return got[i].b < got[j].b
	})
	return in, got
}

func relayWant(p ...relayPair) []relayPair {
	sort.Slice(p, func(i, j int) bool {
		if p[i].id != p[j].id {
			return p[i].id < p[j].id
		}
		if p[i].a != p[j].a {
			return p[i].a < p[j].a
		}
		return p[i].b < p[j].b
	})
	return p
}

func TestDuctingLinksRelayPathPairsAndDedup(t *testing.T) {
	at := linksNow.Add(-2 * time.Hour)
	// Two observers with free-form names (no node, no position) report the
	// same advert along different lengths of the same route.
	tx := relayAdvert(1, rO, "",
		relayObs(10, "turtle-rock", `["11","22","33"]`, at),
		relayObs(11, "txmesh_bot", `["11","22"]`, at.Add(-time.Minute)),
		relayObs(12, "turtle-rock", `["11","22","33"]`, at.Add(time.Minute)),
	)
	in, got := relayGather(t, &Config{}, tx)
	want := relayWant(relayPair{1, rO, rR1}, relayPair{1, rR1, rR2}, relayPair{1, rR2, rR3})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v\nwant  %v", got, want)
	}
	for _, r := range in.Recv {
		if r.via == ductingViaObserver {
			t.Errorf("observer reception from a free-form observer: %+v", r)
		}
		if r.t != at.Add(-time.Minute).Unix() {
			t.Errorf("time %d, want the earliest observation", r.t)
		}
		if r.snr != nil || r.rssi != nil {
			t.Errorf("relay reception carries the observer's signal: %+v", r)
		}
	}
}

func TestDuctingLinksRelayUncertainHop(t *testing.T) {
	at := linksNow.Add(-time.Hour)
	_, got := relayGather(t, &Config{},
		// 44 is two repeaters: only the pairs around it are dropped.
		relayTx(2, RouteFlood, PayloadGRP_TXT, `{"type":"GRP_TXT"}`, relayObs(20, "x", `["11","22","44","33","55"]`, at)),
		// An unknown hop (no node) breaks its pairs too.
		relayTx(3, RouteTransportFlood, PayloadGRP_TXT, `{"type":"GRP_TXT"}`, relayObs(30, "x", `["11","99","22"]`, at)),
	)
	want := relayWant(relayPair{2, rR1, rR2}, relayPair{2, rR3, rR5})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v\nwant  %v", got, want)
	}
}

func TestDuctingLinksRelayExclusions(t *testing.T) {
	at := linksNow.Add(-time.Hour)
	own := relayObs(52, "x", `["11","22"]`, at)
	own.Direction = "tx"
	unbound := relayObs(61, "x", `["11","22"]`, at)
	unbound.relayHopHexLen = 0 // path not bound to its frame: no certain hop
	_, got := relayGather(t, &Config{ObserverBlacklist: []string{"bad-observer"}, NodeBlacklist: []string{rR3}},
		// Direct route: the path is the remaining route.
		relayTx(1, RouteDirect, PayloadADVERT, `{"type":"ADVERT","pubKey":"`+rO+`"}`, relayObs(10, "x", `["11","22"]`, at)),
		// Failed advert signature: nothing from the transmission.
		relayAdvert(2, rO, `,"signatureValid":false`, relayObs(20, "x", `["11","22"]`, at)),
		// ANON_REQ: its key is ephemeral, not a node; the hops still count.
		relayTx(3, RouteFlood, PayloadANON_REQ, `{"type":"ANON_REQ","pubKey":"`+rO+`"}`, relayObs(30, "x", `["11","22"]`, at)),
		// Blacklisted observer and own transmission: ignored.
		relayTx(4, RouteFlood, PayloadGRP_TXT, `{}`, relayObs(40, "bad-observer", `["11","22"]`, at)),
		relayTx(5, RouteFlood, PayloadGRP_TXT, `{}`, own),
		relayTx(6, RouteFlood, PayloadGRP_TXT, `{}`, unbound),
		// Before the period.
		relayTx(7, RouteFlood, PayloadGRP_TXT, `{}`, relayObs(70, "x", `["11","22"]`, linksNow.Add(-8*24*time.Hour))),
		// Hidden node (NodeBlacklist): its pairs go, the others stay.
		relayTx(8, RouteFlood, PayloadGRP_TXT, `{}`, relayObs(80, "x", `["11","22","33"]`, at)),
		// Unchecked advert signature counts; the origin pair is included.
		relayAdvert(9, rO, "", relayObs(90, "x", `["11"]`, at)),
		// A path through the originator itself is no link.
		relayAdvert(10, rR1, "", relayObs(100, "x", `["11","22"]`, at)),
	)
	want := relayWant(relayPair{3, rR1, rR2}, relayPair{8, rR1, rR2}, relayPair{9, rO, rR1}, relayPair{10, rR1, rR2})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v\nwant  %v", got, want)
	}
}

// A path visiting the same link twice, or two observers' paths sharing
// links, still give one reception per (transmission, pair).
func TestDuctingLinksRelayDedupPerTransmission(t *testing.T) {
	at := linksNow.Add(-time.Hour)
	_, got := relayGather(t, &Config{},
		relayTx(1, RouteFlood, PayloadGRP_TXT, `{}`,
			relayObs(10, "a", `["11","22","11","22"]`, at),
			relayObs(11, "b", `["11","22","33"]`, at)),
		relayTx(2, RouteFlood, PayloadGRP_TXT, `{}`, relayObs(20, "a", `["11","22"]`, at)),
	)
	want := relayWant(relayPair{1, rR1, rR2}, relayPair{1, rR2, rR1}, relayPair{1, rR2, rR3}, relayPair{2, rR1, rR2})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs %v\nwant  %v", got, want)
	}
}

// End to end: via and signal fields in the answer, and memo keys that keep
// an observer and a relay reception with the same numeric id apart.
func TestDuctingLinksRelayInResponse(t *testing.T) {
	at := linksNow.Add(-time.Hour)
	// R2 is an observer node and heard R1 directly, one grid step earlier
	// (receptions are listed one per step).
	observer := relayObs(1, rR2, `[]`, at.Add(-3*time.Hour))
	observer.relayHopHexLen = 0
	srv := linksServer(&Config{}, relayNodes(),
		relayAdvert(5, rR1, "", observer),
		relayTx(1, RouteFlood, PayloadGRP_TXT, `{}`, relayObs(2, "free-form", `["11","22"]`, at.Add(time.Minute))),
	)
	src := newLinksFakeSrc()
	src.add(linksGrid(t, 0, at.Unix(), linksAll(linksDuct)))
	src.add(linksGrid(t, 0, at.Add(-3*time.Hour).Unix(), linksAll(linksDuct)))
	st := newDuctingLinksState()
	st.now = func() time.Time { return linksNow }
	w := linksGet(st, srv, src, "", 7)
	r := linksDecode(t, w)
	if r.Status != "ok" || len(r.Links) != 1 || r.Links[0].Count != 2 || r.Summary.Receptions != 2 || r.Summary.DuringDuct != 2 {
		t.Fatalf("resp = %+v", r)
	}
	recs := r.Links[0].Receptions
	if recs[0].Via != "relay" || recs[0].SNR != nil || recs[1].Via != "observer" || recs[1].SNR == nil {
		t.Errorf("receptions = %+v", recs)
	}
	if !strings.Contains(w.Body.String(), `"via":"relay"`) || !strings.Contains(w.Body.String(), `"via":"observer"`) {
		t.Errorf("body lacks via: %s", w.Body.String())
	}
	if st.memo.len() != 2 {
		t.Errorf("memo entries = %d, want one per reception", st.memo.len())
	}
}

// Scanning s.packets in lock-sized chunks gives what one pass gives,
// including across chunk boundaries and with the hop cache warm.
func TestDuctingRelayLinkReceptionsChunked(t *testing.T) {
	at := linksNow.Add(-time.Hour)
	paths := []string{`["11","22","33"]`, `["11","44","33"]`, `["22","33","55"]`, `["11"]`}
	for _, n := range []int{0, 1, ductingScanChunk, ductingScanChunk + 1, 3*ductingScanChunk + 5} {
		var txs []*StoreTx
		for i := 0; i < n; i++ {
			txs = append(txs, relayAdvert(i+1, rO, "", relayObs(i+1, "x", paths[i%len(paths)], at)))
		}
		srv := linksServer(&Config{}, relayNodes(), txs...)
		_, pm := srv.store.getCachedNodesAndPM()
		ends, err := srv.ductingLinkEnds(srv.store.nodeCache)
		if err != nil {
			t.Fatal(err)
		}
		from, to := linksNow.Unix()-86400, linksNow.Unix()
		var buf []ductingRelayTx
		for _, tx := range txs {
			buf = ductingRelayCopy(buf, tx, srv.cfg, from, to)
		}
		res := &ductingHopResolver{pm: pm, cache: map[string]string{}}
		var want []ductingLinkObs
		for i := range buf {
			want = ductingRelayReceptions(want, &buf[i], res, ends)
		}
		got := srv.store.ductingRelayLinkReceptions(ends, pm, srv.cfg, from, to)
		if !reflect.DeepEqual(got, want) || (n > 10 && len(got) == 0) {
			t.Fatalf("n=%d: %d receptions, want %d", n, len(got), len(want))
		}
	}
}

// BenchmarkDuctingRelayLinkReceptions: the relay scan over a 30k-packet
// store, every packet a flood with three observers reporting 4- to 6-hop
// paths over 300 repeaters with distinct 1-byte-free 2-byte hops.
func BenchmarkDuctingRelayLinkReceptions(b *testing.B) {
	var nodes []nodeInfo
	for i := 0; i < 300; i++ {
		n := linksNode(fmt.Sprintf("%04x%060x", 0x1000+i, i), "r", 40+float64(i%20), float64(i%30))
		n.Role = "repeater"
		nodes = append(nodes, n)
	}
	hop := func(i int) string { return fmt.Sprintf(`"%04x"`, 0x1000+i%300) }
	var txs []*StoreTx
	id := 1
	for i := 0; i < 30000; i++ {
		var obs []*StoreObs
		for k := 0; k < 3; k++ {
			hops := make([]string, 4+k)
			for h := range hops {
				hops[h] = hop(i*7 + h*13)
			}
			o := linksObs(id, "observer", "["+strings.Join(hops, ",")+"]", linksNow.Add(-time.Duration(i%5000)*time.Minute))
			o.relayHopHexLen = 4
			obs = append(obs, o)
			id++
		}
		txs = append(txs, relayTx(i+1, RouteFlood, PayloadGRP_TXT, `{}`, obs...))
	}
	srv := linksServer(&Config{}, nodes, txs...)
	_, pm := srv.store.getCachedNodesAndPM()
	ends, _ := srv.ductingLinkEnds(nodes)
	from, to := linksNow.Unix()-7*86400, linksNow.Unix()
	b.ReportAllocs()
	b.ResetTimer()
	var n int
	for i := 0; i < b.N; i++ {
		n = len(srv.store.ductingRelayLinkReceptions(ends, pm, srv.cfg, from, to))
	}
	if n == 0 {
		b.Fatal("no receptions")
	}
	b.ReportMetric(float64(n), "receptions")
}

// A hop whose prefix a retired node (inactive_nodes) also had is not
// certain: the uniqueHopPrefix half of the liveness rule.
func TestDuctingLinksRelayRetiredPrefix(t *testing.T) {
	at := linksNow.Add(-time.Hour)
	srv := linksServer(&Config{}, relayNodes(),
		relayTx(1, RouteFlood, PayloadGRP_TXT, `{}`, relayObs(10, "x", `["11","22","33"]`, at)))
	srv.store.nodePM.retired = map[string]struct{}{"33": {}}
	in, err := srv.ductingLinksInputFor(7, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Recv) != 1 || in.Recv[0].a != rR1 || in.Recv[0].b != rR2 {
		t.Fatalf("receptions = %+v, want only R1->R2", in.Recv)
	}
}
