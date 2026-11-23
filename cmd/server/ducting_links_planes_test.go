package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	ducting "github.com/meshcore-analyzer/ducting"
)

// loadOnly hides LoadPlanes: the links analysis then takes the full Load path.
type loadOnly struct{ ductingStepSource }

// linksPerfFixture stores 56 global 0.5° steps (as the real service reads
// them) and ~2,100 receptions over 300 pairs.
func linksPerfFixture(t testing.TB) (*ductingGridFixture, ductingLinksInput) {
	f := newDuctingGridFixture(t)
	start := linksNow.Unix() - 7*86400
	var times []int64
	for s := 0; s < 56; s++ {
		vt := start + int64(s)*3*3600
		g, err := ducting.NewGrid("gfs", vt, vt, 90, 0, -0.5, 0.5, 720, 361)
		if err != nil {
			t.Fatal(err)
		}
		for i := range g.C {
			lat, lon := g.LatLon(i)
			switch {
			case (int(lat*2)+int(lon*2)+s)%5 == 0:
				g.Set(i, linksDuct)
			case (int(lat)+s)%3 == 0:
				g.Set(i, linksSuper)
			default:
				g.Set(i, linksNormal)
			}
		}
		f.put(t, vt, vt, vt, encodeDuctingGrid(t, g))
		times = append(times, vt)
	}
	f.clock.Store(linksNow.Unix())
	in := ductingLinksInput{Ends: map[string]DuctingLinkEnd{}, FreqMHz: 868, Days: 7}
	for i := 0; i < 300; i++ {
		a, bb := linksPK(10000+i), linksPK(20000+i)
		lat := 35 + float64(i%25)
		in.Ends[a] = linksEnd(a, lat, float64(i%40)-10, 5)
		in.Ends[bb] = linksEnd(bb, lat+0.3, float64(i%40)-8, 5)
		for k := 0; k < 7; k++ {
			in.Recv = append(in.Recv, linksRecv(i*7+k, times[(i*7+k)%len(times)]+600, a, bb))
		}
	}
	return f, in
}

// The plane-only decode path must give byte-identical answers to Load.
func TestDuctingLinksPlanesEqualsLoad(t *testing.T) {
	f, in := linksPerfFixture(t)
	var src ductingStepSource = f.svc
	if _, ok := src.(ductingPlaneLoader); !ok {
		t.Fatal("the grid service must implement ductingPlaneLoader")
	}
	fast, err := buildDuctingLinks(in, f.svc, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	full, err := buildDuctingLinks(in, loadOnly{f.svc}, linksNow)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(fast)
	b, _ := json.Marshal(full)
	if string(a) != string(b) || fast.Summary.WithGrid == 0 || fast.Summary.BaselineDuctFrac < 0 {
		t.Fatalf("plane-only decode changed the result:\n%s\n%s", a, b)
	}
}

func TestDuctingGridLoadPlanes(t *testing.T) {
	f := newDuctingGridFixture(t)
	f.putGrid(t, 3600, 3600, 3700)
	d := ducting.Decoder{N: true} // as the links analysis decodes: C, F and N
	g, err := f.svc.LoadPlanes(3600, &d)
	if err != nil || g.ValidTime != 3600 || g.RunTime != 3600 {
		t.Fatalf("LoadPlanes: %v %+v", err, g)
	}
	full, _ := f.svc.Load(3600)
	if g.PathStats(45, -5, 55, 5, 868, 50) != full.PathStats(45, -5, 55, 5, 868, 50) {
		t.Fatal("PathStats differs")
	}
	if _, err := f.svc.LoadPlanes(999, &d); !errors.Is(err, errDuctingStepAbsent) {
		t.Errorf("absent: %v", err)
	}
	// A header that does not match its row is refused and remembered.
	f.put(t, 7200, 7200, 7300, encodeDuctingGrid(t, testDuctingGrid(t, 9999, 7200)))
	for i := 0; i < 2; i++ {
		if _, err := f.svc.LoadPlanes(7200, &d); !errors.Is(err, errDuctingStepUnavailable) {
			t.Errorf("mismatched header: %v", err)
		}
	}
	f.put(t, 10800, 10800, 10900, []byte("not a grid"))
	if _, err := f.svc.LoadPlanes(10800, &d); !errors.Is(err, errDuctingStepUnavailable) {
		t.Errorf("garbage: %v", err)
	}
	// The decoder still works after failures.
	if g, err := f.svc.LoadPlanes(3600, &d); err != nil || g.ValidTime != 3600 {
		t.Fatalf("after failures: %v", err)
	}
}

// BenchmarkDuctingLinksBuildService: the cold rebuild through the real
// service (SQLite blobs), with the full Load path and with plane-only decode.
func BenchmarkDuctingLinksBuildService(b *testing.B) {
	f, in := linksPerfFixture(b)
	for _, bc := range []struct {
		name string
		src  ductingStepSource
	}{{"load", loadOnly{f.svc}}, {"planes", f.svc}} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				resp, err := buildDuctingLinks(in, bc.src, linksNow)
				if err != nil || resp.Summary.Links != 300 || resp.Summary.Receptions != 2100 {
					b.Fatalf("resp %+v err %v", resp.Summary, err)
				}
			}
		})
	}
}

// BenchmarkDuctingLinkReceptions: the store scan under the PacketStore read
// lock. 30k packets of which 10% adverts (3,000), 4 zero-hop observations
// each, all of them passing every filter (the worst case for the scan), and
// a 50k-advert store. "cold" clears the cached timestamp parse first.
func BenchmarkDuctingLinkReceptions(b *testing.B) {
	for _, nAdv := range []int{3000, 50000} {
		ends := map[string]DuctingLinkEnd{}
		var txs []*StoreTx
		id := 1
		for i := 0; i < nAdv; i++ {
			a := linksPK(100000 + i%500)
			ends[a] = linksEnd(a, 40, 0, 5)
			var obs []*StoreObs
			for k := 0; k < 4; k++ {
				o := linksPK(200000 + (i+k)%300)
				ends[o] = linksEnd(o, 41, 1, 5)
				obs = append(obs, linksObs(id, o, "[]", linksNow.Add(-time.Duration(id%1000)*time.Minute)))
				id++
			}
			txs = append(txs, linksAdvert(i+1, RouteDirect, a, `,"signatureValid":true`, obs...))
		}
		srv := linksServer(&Config{}, nil, txs...)
		from, to := linksNow.Unix()-7*86400, linksNow.Unix()
		for _, mode := range []string{"warm", "cold"} {
			b.Run(fmt.Sprintf("adverts=%d/%s", nAdv, mode), func(b *testing.B) {
				b.ReportAllocs()
				var n int
				for i := 0; i < b.N; i++ {
					if mode == "cold" {
						b.StopTimer()
						for _, tx := range txs {
							for _, o := range tx.Observations {
								o.tsUnixNano.Store(0)
							}
						}
						b.StartTimer()
					}
					n = len(srv.store.ductingLinkReceptions(ends, srv.cfg, from, to))
				}
				if n != nAdv*4 {
					b.Fatalf("%d receptions, want %d", n, nAdv*4)
				}
			})
		}
	}
}

// linksManyAdverts builds n adverts (every 7th with a failed signature, every
// 5th observation relayed) over a handful of nodes.
func linksManyAdverts(n int) (map[string]DuctingLinkEnd, []*StoreTx) {
	ends := map[string]DuctingLinkEnd{}
	var txs []*StoreTx
	id := 1
	for i := 0; i < n; i++ {
		a := linksPK(100 + i%40)
		ends[a] = linksEnd(a, 40, 0, 5)
		sig := "true"
		if i%7 == 0 {
			sig = "false"
		}
		var obs []*StoreObs
		for k := 0; k < 3; k++ {
			o := linksPK(500 + (i+k)%25)
			ends[o] = linksEnd(o, 41, 1, 5)
			path := "[]"
			if id%5 == 0 {
				path = `["aa"]`
			}
			obs = append(obs, linksObs(id, o, path, linksNow.Add(-time.Duration(id)*time.Minute)))
			id++
		}
		txs = append(txs, linksAdvert(i+1, RouteDirect, a, `,"signatureValid":`+sig, obs...))
	}
	return ends, txs
}

// Scanning in lock-sized chunks gives what one pass over the list gives,
// including across chunk boundaries.
func TestDuctingLinkReceptionsChunked(t *testing.T) {
	for _, n := range []int{0, 1, ductingScanChunk - 1, ductingScanChunk, ductingScanChunk + 1, 5*ductingScanChunk + 17} {
		ends, txs := linksManyAdverts(n)
		srv := linksServer(&Config{}, nil, txs...)
		from, to := linksNow.Unix()-3*86400, linksNow.Unix()
		var want []ductingLinkObs
		for _, tx := range txs {
			want = ductingAdvertReceptions(want, tx, ends, srv.cfg, from, to)
		}
		got := srv.store.ductingLinkReceptions(ends, srv.cfg, from, to)
		if !reflect.DeepEqual(got, want) || (n > 100 && len(got) == 0) {
			t.Fatalf("n=%d: %d receptions, want %d", n, len(got), len(want))
		}
	}
}

// Evictions (in-place compaction of the ADVERT list) between chunks must not
// crash the scan or invent receptions.
func TestDuctingLinkReceptionsDuringEviction(t *testing.T) {
	ends, txs := linksManyAdverts(4000)
	srv := linksServer(&Config{}, nil, txs...)
	from, to := linksNow.Unix()-30*86400, linksNow.Unix()
	all := map[int]bool{}
	for _, r := range srv.store.ductingLinkReceptions(ends, srv.cfg, from, to) {
		all[r.obsID] = true
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			srv.store.mu.Lock()
			l := srv.store.byPayloadType[PayloadADVERT]
			ids := map[int]bool{}
			for _, tx := range l[:min(50, len(l))] {
				ids[tx.ID] = true
			}
			srv.store.byPayloadType[PayloadADVERT] = slices.DeleteFunc(l, func(tx *StoreTx) bool { return ids[tx.ID] })
			srv.store.mu.Unlock()
			runtime.Gosched()
		}
	}()
	for i := 0; i < 20; i++ {
		for _, r := range srv.store.ductingLinkReceptions(ends, srv.cfg, from, to) {
			if !all[r.obsID] {
				t.Fatalf("invented reception %d", r.obsID)
			}
		}
	}
	<-done
}
