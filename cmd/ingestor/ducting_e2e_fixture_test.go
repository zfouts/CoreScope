package main

// Fixture generator for the global-grid ducting E2E
// (tests/e2e/test-map-ducting-grid-e2e.js). Test-only: it never ships.
//
//	cd cmd/ingestor && go test -count=1 -run '^TestDuctingE2EFixture$' . \
//	    -args -ducting-e2e-fixture=/abs/path/to/copy-of-e2e-fixture.db
//
// writes into that (already migrated) database, through the ingestor's own
// write paths, exactly what a running ingestor would have stored:
//
//   - ducting_grid: one row per valid time every 3 h from 48 h before to
//     48 h after now (relative to when it runs, so the slider always has
//     past and forecast steps), on a 0.5° lattice covering every node with a
//     position, encoded with ducting.Encode and written by
//     UpsertDuctingGrid. The field is synthetic: a duct band (surface duct
//     at its core, then elevated duct, then super-refraction) that moves
//     with time, normal elsewhere.
//   - Two long links: signed zero-hop ADVERTs from two new repeaters, each
//     heard directly by an observer that is a node with a position, about
//     110 km apart (well beyond the radio horizon), decoded and stored by
//     DecodePacket / BuildPacketData / InsertTransmission / UpsertNode as
//     the MQTT handler does. One is heard at the newest analysis time with
//     a surface duct along its whole path; the other 6 h earlier with none.
//     The first sender's name is HTML, so the E2E can check it is escaped.
//
// Without the flag the test runs the generator on a small temporary
// database and checks what it wrote, so the generator itself is covered by
// the ordinary ingestor suite.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
	ducting "github.com/meshcore-analyzer/ducting"
)

var ductingE2EFixturePath = flag.String("ducting-e2e-fixture", "", "write the ducting grid E2E fixture into this SQLite database")

const (
	e2eDuctStepS      = 3 * 3600 // spacing of the stored valid times
	e2eDuctRunS       = 6 * 3600 // GFS cycle spacing
	e2eDuctSpanS      = 48 * 3600
	e2eDuctLatticeDeg = 0.5
	e2eDuctPadDeg     = 2.0
	e2eDuctPathDeg    = 0.6 // lattice points this close to a link's path follow its override
	// e2eDuctEvilName is the first sender's advert name: HTML that runs
	// script if anything renders it as markup. At most 32 bytes (the
	// firmware's name buffer; the decoder truncates longer names).
	e2eDuctEvilName  = `<img src=x onerror=__dx=1>&"Q"`
	e2eDuctPlainName = "E2E Duct Far Repeater"
)

// e2eDuctLink is one seeded long link.
type e2eDuctLink struct {
	name        string
	seed        string // ed25519 key seed (hashed), so keys are stable across runs
	observerID  string // observers.id as stored
	latB, lonB  float64
	dLat, dLon  float64 // sender position relative to the observer
	ducted      bool    // a surface duct along the path at the reception step, else none
	rxOffsetsS  []int64 // reception times relative to the link's step
	stepOffsetS int64   // the link's step relative to the newest step <= now
	senderPub   string  // filled in: lowercase hex pubkey
	latA, lonA  float64 // filled in
	rx          []int64 // filled in: reception times
}

// e2eDuctFixture is what the generator wrote.
type e2eDuctFixture struct {
	times   []int64
	newest  int64 // newest stored time <= now
	runTime int64
	lat0    float64
	lon0    float64
	ni, nj  int
	links   []*e2eDuctLink
}

func floorTo(t, step int64) int64 { return t - ((t%step)+step)%step }

// e2eDuctClass is the synthetic field at a lattice point for a band centre
// line through (cLat, cLon) running east-north-east.
func e2eDuctResult(lat, lon, cLat, cLon float64) ducting.Result {
	d := math.Abs((lat - cLat) - 0.3*(lon-cLon))
	n := -100 + 70*math.Min(1, d/2.0) // -100 N/km on the band's axis to -30 at 2° away
	switch {
	case d < 0.35:
		return ducting.Result{Class: ducting.ClassSurface, FminMHz: 400 + 400*d, BaseM: 0, TopM: 180, DeltaM: 14, MinGradNkm: -210, MeanGradNkm: n}
	case d < 0.7:
		return ducting.Result{Class: ducting.ClassElev, FminMHz: 1400, BaseM: 600, TopM: 780, DeltaM: 4, MinGradNkm: -170, MeanGradNkm: n}
	case d < 1.3:
		return ducting.Result{Class: ducting.ClassSuper, MinGradNkm: -92, MeanGradNkm: n}
	}
	return ducting.Result{Class: ducting.ClassNormal, MinGradNkm: -40, MeanGradNkm: n}
}

// e2eDuctSegDeg is the distance in degrees (longitude scaled by cos lat) from
// a point to the segment a-b.
func e2eDuctSegDeg(lat, lon, latA, lonA, latB, lonB float64) float64 {
	k := math.Cos((latA + latB) / 2 * math.Pi / 180)
	px, py := (lon-lonA)*k, lat-latA
	dx, dy := (lonB-lonA)*k, latB-latA
	u := 0.0
	if l2 := dx*dx + dy*dy; l2 > 0 {
		u = math.Max(0, math.Min(1, (px*dx+py*dy)/l2))
	}
	return math.Hypot(px-u*dx, py-u*dy)
}

// e2eDuctAdvertHex builds a signed zero-hop flood ADVERT (header: route
// FLOOD, payload ADVERT; path_len 0) from a repeater with a position and a
// name. Layout: firmware docs/payloads.md (pubkey, timestamp, signature
// over pubkey+timestamp+appdata, appdata = flags, lat/lon x1e6, name).
func e2eDuctAdvertHex(priv ed25519.PrivateKey, ts int64, lat, lon float64, name string) string {
	pub := priv.Public().(ed25519.PublicKey)
	app := []byte{0x80 | 0x10 | 0x02} // has name, has location, ADV_TYPE_REPEATER
	app = binary.LittleEndian.AppendUint32(app, uint32(int32(math.Round(lat*1e6))))
	app = binary.LittleEndian.AppendUint32(app, uint32(int32(math.Round(lon*1e6))))
	app = append(app, name...)
	msg := append(append(append([]byte{}, pub...), binary.LittleEndian.AppendUint32(nil, uint32(ts))...), app...)
	sig := ed25519.Sign(priv, msg)
	pkt := []byte{0x01 | 0x04<<2, 0x00}
	pkt = append(pkt, pub...)
	pkt = binary.LittleEndian.AppendUint32(pkt, uint32(ts))
	pkt = append(pkt, sig...)
	pkt = append(pkt, app...)
	return strings.ToUpper(hex.EncodeToString(pkt))
}

// e2eDuctNodeExtent is the 5th..95th percentile box of the node positions.
func e2eDuctNodeExtent(db *sql.DB) (minLat, maxLat, minLon, maxLon float64, err error) {
	rows, err := db.Query(`SELECT lat, lon FROM nodes
		WHERE lat IS NOT NULL AND lon IS NOT NULL AND NOT (lat = 0 AND lon = 0)`)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("node positions: %w", err)
	}
	defer rows.Close()
	var lats, lons []float64
	for rows.Next() {
		var la, lo float64
		if err := rows.Scan(&la, &lo); err != nil {
			return 0, 0, 0, 0, err
		}
		lats, lons = append(lats, la), append(lons, lo)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, err
	}
	if len(lats) == 0 {
		return 0, 0, 0, 0, fmt.Errorf("no node has a position")
	}
	sort.Float64s(lats)
	sort.Float64s(lons)
	lo, hi := len(lats)*5/100, len(lats)-1-len(lats)*5/100
	return lats[lo], lats[hi], lons[lo], lons[hi], nil
}

// e2eDuctOpen opens a database the way OpenStoreWithInterval does (writer
// DSN, ingestor schema, dbschema.Apply, prepared statements) but schedules
// none of its background migrations, so the generator's writes are all
// that changes.
func e2eDuctOpen(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbschema.WriterDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := applySchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	if err := dbschema.Apply(db, log.Printf); err != nil {
		db.Close()
		return nil, fmt.Errorf("dbschema.Apply: %w", err)
	}
	s, err := newStore(db, path, 300)
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// writeDuctingE2EFixture writes the grid rows and the long links into the
// database at path, relative to now.
func writeDuctingE2EFixture(path string, now time.Time) (*e2eDuctFixture, error) {
	s, err := e2eDuctOpen(path)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	nowU := now.Unix()
	fx := &e2eDuctFixture{newest: floorTo(nowU, e2eDuctStepS), runTime: floorTo(nowU, e2eDuctRunS)}

	// Observers that are nodes with a position, the receiving ends.
	rows, err := s.db.Query(`SELECT o.id, n.lat, n.lon FROM observers o
		JOIN nodes n ON lower(n.public_key) = lower(o.id)
		WHERE n.lat IS NOT NULL AND n.lon IS NOT NULL AND NOT (n.lat = 0 AND n.lon = 0)
		ORDER BY lower(o.id) LIMIT 2`)
	if err != nil {
		return nil, fmt.Errorf("observer nodes: %w", err)
	}
	type obsNode struct {
		id       string
		lat, lon float64
	}
	var obs []obsNode
	for rows.Next() {
		var o obsNode
		if err := rows.Scan(&o.id, &o.lat, &o.lon); err != nil {
			rows.Close()
			return nil, err
		}
		obs = append(obs, o)
	}
	rows.Close()
	if len(obs) == 0 {
		return nil, fmt.Errorf("no observer is a node with a position: nothing can hear a long link")
	}
	o2 := obs[len(obs)-1]
	fx.links = []*e2eDuctLink{
		{name: e2eDuctEvilName, seed: "corescope-ducting-e2e-evil", observerID: obs[0].id, latB: obs[0].lat, lonB: obs[0].lon,
			dLat: 0.7, dLon: 0.9, ducted: true, rxOffsetsS: []int64{0, -24 * 3600}},
		{name: e2eDuctPlainName, seed: "corescope-ducting-e2e-plain", observerID: o2.id, latB: o2.lat, lonB: o2.lon,
			dLat: -0.7, dLon: 0.9, ducted: false, rxOffsetsS: []int64{0}, stepOffsetS: -6 * 3600},
	}
	for _, l := range fx.links {
		l.latA, l.lonA = l.latB+l.dLat, l.lonB+l.dLon
	}

	// Lattice over the nodes' area (the 5th to 95th percentile of their
	// positions, so a few stray positions such as 0,lon do not stretch it
	// over half the world) and the new senders, padded.
	minLat, maxLat, minLon, maxLon, err := e2eDuctNodeExtent(s.db)
	if err != nil {
		return nil, err
	}
	for _, l := range fx.links {
		minLat, maxLat = math.Min(minLat, l.latB), math.Max(maxLat, l.latB)
		minLon, maxLon = math.Min(minLon, l.lonB), math.Max(maxLon, l.lonB)
		minLat, maxLat = math.Min(minLat, l.latA), math.Max(maxLat, l.latA)
		minLon, maxLon = math.Min(minLon, l.lonA), math.Max(maxLon, l.lonA)
	}
	st := e2eDuctLatticeDeg
	north := math.Min(90, math.Ceil((maxLat+e2eDuctPadDeg)/st)*st)
	south := math.Max(-90, math.Floor((minLat-e2eDuctPadDeg)/st)*st)
	west := math.Floor((minLon-e2eDuctPadDeg)/st) * st
	east := math.Ceil((maxLon+e2eDuctPadDeg)/st) * st
	fx.lat0, fx.lon0 = north, west
	fx.nj = int(math.Round((north-south)/st)) + 1
	fx.ni = int(math.Round((east-west)/st)) + 1
	midLat, midLon := (minLat+maxLat)/2, (minLon+maxLon)/2
	amp := math.Max(0.5, (maxLat-minLat)/2)

	for t := fx.newest - e2eDuctSpanS; t <= fx.newest+e2eDuctSpanS; t += e2eDuctStepS {
		fx.times = append(fx.times, t)
	}
	for k, t := range fx.times {
		run := fx.runTime
		if t <= fx.runTime {
			run = floorTo(t, e2eDuctRunS)
		}
		created := run + 4*3600
		if run == fx.runTime {
			created = nowU
		}
		g, err := ducting.NewGrid(ductingGridModel, run, t, fx.lat0, fx.lon0, -st, st, fx.ni, fx.nj)
		if err != nil {
			return nil, err
		}
		cLat := midLat + amp*math.Sin(2*math.Pi*float64(k)/12)
		for idx := 0; idx < fx.ni*fx.nj; idx++ {
			lat, lon := g.LatLon(idx)
			r := e2eDuctResult(lat, lon, cLat, midLon)
			for _, l := range fx.links {
				if t != fx.newest+l.stepOffsetS || e2eDuctSegDeg(lat, lon, l.latA, l.lonA, l.latB, l.lonB) > e2eDuctPathDeg {
					continue
				}
				if l.ducted {
					r = ducting.Result{Class: ducting.ClassSurface, FminMHz: 420, TopM: 180, DeltaM: 14, MinGradNkm: -210, MeanGradNkm: -96}
				} else {
					r = ducting.Result{Class: ducting.ClassNormal, MinGradNkm: -40, MeanGradNkm: -30}
				}
			}
			g.Set(idx, r)
		}
		data, err := ducting.Encode(g)
		if err != nil {
			return nil, err
		}
		if _, err := s.UpsertDuctingGrid(ductingGridModel, t, run, int((t-run)/3600), data, created); err != nil {
			return nil, fmt.Errorf("upsert step %d: %w", t, err)
		}
	}

	// The long links, through the MQTT handler's ADVERT path.
	snr, rssi := -11.5, -117.0
	dir := "rx"
	for _, l := range fx.links {
		sum := sha256.Sum256([]byte(l.seed))
		priv := ed25519.NewKeyFromSeed(sum[:])
		l.senderPub = hex.EncodeToString(priv.Public().(ed25519.PublicKey))
		for _, off := range l.rxOffsetsS {
			rx := fx.newest + l.stepOffsetS + off
			raw := e2eDuctAdvertHex(priv, rx, l.latA, l.lonA, l.name)
			decoded, err := DecodePacket(raw, nil, true)
			if err != nil {
				return nil, fmt.Errorf("decode seeded advert: %w", err)
			}
			if ok, why := ValidateAdvert(&decoded.Payload); !ok {
				return nil, fmt.Errorf("seeded advert invalid: %s", why)
			}
			if v := decoded.Payload.SignatureValid; v == nil || !*v {
				return nil, fmt.Errorf("seeded advert signature does not verify")
			}
			msg := &MQTTPacketMessage{Raw: raw, SNR: &snr, RSSI: &rssi, Direction: &dir, Origin: "e2e"}
			pd := BuildPacketData(msg, decoded, l.observerID, "", nil)
			ts := time.Unix(rx, 0).UTC().Format(time.RFC3339)
			pd.Timestamp = ts // heard then, not now (BuildPacketData stamps ingest time)
			isNew, err := s.InsertTransmission(pd)
			if err != nil {
				return nil, fmt.Errorf("insert seeded advert: %w", err)
			}
			if err := s.UpsertNode(decoded.Payload.PubKey, decoded.Payload.Name, advertRole(decoded.Payload.Flags), decoded.Payload.Lat, decoded.Payload.Lon, ts); err != nil {
				return nil, fmt.Errorf("upsert seeded node: %w", err)
			}
			if isNew {
				if err := s.IncrementAdvertCount(decoded.Payload.PubKey); err != nil {
					return nil, err
				}
			}
			l.rx = append(l.rx, rx)
		}
	}
	return fx, nil
}

// checkDuctingE2EFixture reads back what the generator wrote, the way the
// server will: rows, decoded steps, the links' path duct shares (868 MHz,
// 25 km samples as /api/ducting/links), and the stored receptions.
func checkDuctingE2EFixture(t *testing.T, path string, fx *e2eDuctFixture, now time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT valid_time, run_time, lead_hours, data FROM ducting_grid WHERE model = ? ORDER BY valid_time`, ductingGridModel)
	if err != nil {
		t.Fatal(err)
	}
	grids := map[int64]*ducting.Grid{}
	var times []int64
	for rows.Next() {
		var vt, rt int64
		var lead int
		var data []byte
		if err := rows.Scan(&vt, &rt, &lead, &data); err != nil {
			t.Fatal(err)
		}
		g, err := ducting.Decode(data)
		if err != nil {
			t.Fatalf("step %d does not decode: %v", vt, err)
		}
		if g.ValidTime != vt || g.RunTime != rt || int64(lead)*3600 != vt-rt || rt > fx.runTime {
			t.Fatalf("step %d: grid valid %d run %d, row run %d lead %d", vt, g.ValidTime, g.RunTime, rt, lead)
		}
		grids[vt] = g
		times = append(times, vt)
	}
	rows.Close()
	if len(times) != 33 {
		t.Fatalf("%d stored steps, want 33 (48 h back to 48 h ahead every 3 h)", len(times))
	}
	nowU := now.Unix()
	for i, vt := range times {
		if i > 0 && vt-times[i-1] != e2eDuctStepS {
			t.Fatalf("steps %d and %d are not 3 h apart", times[i-1], vt)
		}
	}
	if !(times[0] <= nowU-e2eDuctSpanS+e2eDuctStepS && times[len(times)-1] > nowU+e2eDuctSpanS-e2eDuctStepS) {
		t.Fatalf("steps %d..%d do not span now %d +-48 h", times[0], times[len(times)-1], nowU)
	}
	seen := map[uint8]bool{}
	for _, c := range grids[fx.newest].C {
		seen[c] = true
	}
	for _, c := range []uint8{ducting.ClassNormal, ducting.ClassSuper, ducting.ClassElev, ducting.ClassSurface} {
		if !seen[c] {
			t.Errorf("no class %d point at the newest step", c)
		}
	}
	a, b := grids[times[0]], grids[times[3]]
	same := true
	for i := range a.C {
		if a.C[i] != b.C[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("the field does not change over time; the slider could not change it")
	}

	for _, l := range fx.links {
		g := grids[fx.newest+l.stepOffsetS]
		if g == nil {
			t.Fatalf("no step for link %q", l.name)
		}
		ps := g.PathStats(l.latA, l.lonA, l.latB, l.lonB, 868, 25)
		frac := float64(ps.Duct) / float64(ps.Samples-ps.NoData)
		if l.ducted != (frac >= 0.5) {
			t.Errorf("link %q: duct share %.2f, want ducted=%v", l.name, frac, l.ducted)
		}
		if _, ok := g.Nearest(l.latA, l.lonA); !ok {
			t.Errorf("link %q sender outside the lattice", l.name)
		}
		var name string
		if err := db.QueryRow(`SELECT name FROM nodes WHERE lower(public_key) = ? AND lat IS NOT NULL`, l.senderPub).Scan(&name); err != nil || name != l.name {
			t.Errorf("link %q: node row %q, %v", l.name, name, err)
		}
		for _, rx := range l.rx {
			var n int
			err := db.QueryRow(`SELECT COUNT(*) FROM observations o
				JOIN transmissions t ON t.id = o.transmission_id
				JOIN observers ob ON ob.idx = o.observer_idx
				WHERE t.payload_type = 4 AND lower(t.from_pubkey) = ? AND ob.id = ?
				  AND o.path_json = '[]' AND o.timestamp = ? AND t.decoded_json LIKE '%"signatureValid":true%'`,
				l.senderPub, l.observerID, rx).Scan(&n)
			if err != nil || n != 1 {
				t.Errorf("link %q reception at %d: %d stored zero-hop observations (%v)", l.name, rx, n, err)
			}
		}
	}
}

// TestDuctingE2EFixture runs the generator: into -ducting-e2e-fixture when
// given (the E2E setup), else into a temporary database with two observer
// nodes, and checks the result either way.
func TestDuctingE2EFixture(t *testing.T) {
	path := *ductingE2EFixturePath
	if path == "" {
		path = filepath.Join(t.TempDir(), "ducting-e2e.db")
		s, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		seen := time.Now().UTC().Format(time.RFC3339)
		for i, p := range [][2]float64{{37.30, -121.90}, {37.80, -122.25}} {
			pk := strings.Repeat(fmt.Sprintf("%02x", 0xa0+i), 32)
			lat, lon := p[0], p[1]
			if err := s.UpsertNode(pk, fmt.Sprintf("Observer %d", i), "repeater", &lat, &lon, seen); err != nil {
				t.Fatal(err)
			}
			if err := s.UpsertObserver(strings.ToUpper(pk), fmt.Sprintf("Observer %d", i), "SJC", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	fx, err := writeDuctingE2EFixture(path, now)
	if err != nil {
		t.Fatal(err)
	}
	checkDuctingE2EFixture(t, path, fx, now)
	t.Logf("ducting E2E fixture: %d steps %d..%d (newest <= now %d, run %d), lattice %dx%d from %.1f,%.1f, links %s @%v, %s @%v",
		len(fx.times), fx.times[0], fx.times[len(fx.times)-1], fx.newest, fx.runTime, fx.ni, fx.nj, fx.lat0, fx.lon0,
		fx.links[0].senderPub[:8], fx.links[0].rx, fx.links[1].senderPub[:8], fx.links[1].rx)
}
