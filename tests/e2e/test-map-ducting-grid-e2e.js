/**
 * E2E for the tropospheric-ducting overlay with the stored global grid
 * (ducting.source "gfs-grid", the default): GET /api/ducting/meta, the
 * windows of /api/ducting/grid, the history/forecast timeline in the
 * movable, collapsible panel, the duct=1&ductT deep link and the observed
 * long links of /api/ducting/links,
 * against a real CoreScope server. The Open-Meteo source has its own suite
 * (test-map-ducting-e2e.js); both share tests/e2e/lib/ducting-e2e.js.
 *
 * The server only reads table ducting_grid; the ingestor writes it. The
 * fixture comes from the ingestor's own write path, relative to now, so the
 * timeline always has past ("analysis") and future ("forecast") steps:
 * cmd/ingestor/ducting_e2e_fixture_test.go writes 33 steps (48 h back to
 * 48 h ahead, every 3 h) over the fixture's nodes and three long links of
 * ~110 km (signed ADVERTs): heard by an observer node in the High band
 * (mean dN/dh -96 N/km along the path; sender named EVIL_NAME below), heard
 * by an observer node on a normal path (PLAIN_NAME), and relayed with one
 * certain 2-byte hop in the High band (RELAY_NAME, via "relay"). Keep these
 * names in step with that file. The server config has no LoS terrain, so
 * pairs count by the smooth-earth horizon (losMethod "smooth-earth"); no
 * terrain is ever downloaded.
 *
 * CI: GitHub Actions E2E job (.github/workflows/deploy.yml, e2e shard 1),
 * server on 13598, `suite 1 test-map-ducting-grid-e2e.js`.
 *
 * Locally (the fixture must be migrated first, as CI does; see AGENTS.md):
 *
 *   mkdir -p /tmp/ducting-grid && cp test-fixtures/e2e-fixture.db /tmp/ducting-grid/
 *   (cd cmd/ingestor && go test -count=1 -run '^TestDuctingE2EFixture$' . \
 *       -args -ducting-e2e-fixture=/tmp/ducting-grid/e2e-fixture.db)
 *   cat > /tmp/ducting-grid/config.json <<'JSON'
 *   { "port": 13598,
 *     "planner": { "enabled": true, "defaultRangeKm": 30, "newAntennaM": 10,
 *                  "existingAntennaM": 20, "maxAreaKm": 100, "nodeMarginKm": 20, "maxLegKm": 100 },
 *     "ducting": { "enabled": true, "source": "gfs-grid" } }
 *   JSON
 *   corescope-server -config-dir /tmp/ducting-grid -db /tmp/ducting-grid/e2e-fixture.db -port 13598 -public public &
 *   BASE_URL=http://localhost:13598 node tests/e2e/test-map-ducting-grid-e2e.js
 *
 * Env:
 *   BASE_URL       CoreScope server (default http://localhost:13598)
 *   CHROMIUM_PATH  optional browser binary
 */
'use strict';
const { chromium } = require('playwright');
const DE = require('./lib/ducting-e2e');
const { loraFreqsOf, inPageStatus, assert, sleep, openPicker, setOverlayUi, pageState, inPageOverlaps, cssVars,
  inPageFieldProbe, fieldProblems, outlineProblems, pointsIn, settled, panelState } = DE;

const BASE = (process.env.BASE_URL || 'http://localhost:13598').replace(/\/+$/, '');
const EVIL_NAME = '<img src=x onerror=__dx=1>&"Q"';
const PLAIN_NAME = 'E2E Duct Far Repeater';
const RELAY_NAME = 'E2E Duct Relay Origin';
const LINK_WINDOW_S = 90 * 60;   // map-ducting.js LINK_WINDOW_S
const LINK_ENHANCED_FRAC = 0.5;  // map-ducting.js LINK_ENHANCED_FRAC (server: ductingLinksDuctFrac)
const MINUS = '\u2212';
const grad = (v) => (Math.round(v) < 0 ? MINUS + Math.abs(Math.round(v)) : String(Math.round(v))) + ' N/km';
const pct = (f) => Math.round(f * 100) + '%';
const newPage = (browser, opts) => DE.newPage(browser, BASE, opts);
const gotoRoute = (page, hash, waitControl) => DE.gotoRoute(page, BASE, hash, waitControl);
const R = DE.createRunner();
const test = R.test;

async function getJson(path) {
  const r = await fetch(BASE + path, { signal: AbortSignal.timeout(20000) });
  if (!r.ok) throw new Error(`${path} -> HTTP ${r.status}`);
  return r.json();
}

// Latest time <= now, else the first (map-ducting.js ductTimeIndex).
function defaultIndex(times) {
  const now = Date.now() / 1000;
  let best = 0;
  times.forEach((t, i) => { if (t <= now) best = i; });
  return best;
}

// The links drawn at time t: a reception within +-90 min; kind of the
// nearest one: 'nodata' without a modelled path gradient (meanGradNkm null),
// else 'duct' (the emphasized colour) when half or more of the path is in
// the High band (highFrac >= 0.5), else 'plain'. The test's own reading of
// the endpoint contract.
function expectedLinks(links, t) {
  const out = [];
  for (const l of links) {
    const near = l.receptions.filter(r => Math.abs(r.t - t) <= LINK_WINDOW_S)
      .sort((x, y) => (Math.abs(x.t - t) - Math.abs(y.t - t)) || (x.t - y.t));
    if (!near.length) continue;
    const r = near[0];
    out.push({ link: l, kind: r.meanGradNkm == null ? 'nodata' : r.highFrac >= LINK_ENHANCED_FRAC ? 'duct' : 'plain' });
  }
  return out;
}
const segKey = (pts) => pts.map(p => p.map(v => v.toFixed(5)).join(',')).join(';');
const linkKey = (l) => segKey([[l.a.lat, l.a.lon], [l.b.lat, l.b.lon]]);

// ── In-page probes ──────────────────────────────────────────────────────────
const firstMap = 'window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0]';

// The overlay layer's window data (the cells drawn) and what it shows.
function inPageLayer() {
  const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
  let layer = null;
  if (m) m.eachLayer((l) => { if (l._mcDuct) layer = l; });
  if (!layer) return null;
  const d = layer._data;
  return {
    data: d ? { stepDeg: d.stepDeg, times: d.times, cells: d.cells } : null,
    sel: layer._sel,
    shownT: layer._shownT,
    gridMode: layer._gridMode,
    linksState: layer._linksState,
  };
}

// The drawn link polylines by kind: { kind: [[ [lat, lon], [lat, lon] ], ...] }.
function inPageLinks() {
  const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
  const out = {};
  m.eachLayer((l) => {
    if (!l._mcLinks) return;
    out[l._mcLinks] = (out[l._mcLinks] || []).concat(l.getLatLngs().map(p => p.map(ll => [ll.lat, ll.lng])));
    out[l._mcLinks + 'Style'] = { interactive: l.options.interactive, dash: l.options.dashArray || null, pane: l._renderer && l._renderer.options.pane };
  });
  return out;
}

const fieldSum = async (page) => (await page.evaluate(inPageFieldProbe, { idx: 0, cells: [] })).sum;
// The timeline (a role=slider strip) and the time line above it.
const timelineState = (page) => page.evaluate(() => {
  const s = document.querySelector('.mc-duct-timeline');
  const l = document.querySelector('.mc-duct-time-label');
  return s ? {
    value: +s.getAttribute('aria-valuenow'), max: +s.getAttribute('aria-valuemax'), disabled: s.getAttribute('aria-disabled') === 'true',
    valuetext: s.getAttribute('aria-valuetext'), label: l && l.textContent, ariaLabel: s.getAttribute('aria-label'),
  } : null;
});
// A user scroll of the strip that ends on stored time i (one gesture: the
// browser's scroll event, then the overlay's frame), then the snap.
async function scrollToTime(page, i) {
  await settled(page);
  await page.evaluate((k) => {
    const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
    let layer = null;
    m.eachLayer((l) => { if (l._mcDuct) layer = l; });
    document.querySelector('.mc-duct-timeline').scrollLeft = layer._tlLayout.xs[k];
  }, i);
  await page.waitForFunction((k) => document.querySelector('.mc-duct-timeline').getAttribute('aria-valuenow') === String(k), i, { timeout: 5000 });
  return settled(page);
}
const hashParams = (page) => page.evaluate(() => {
  const q = location.hash.split('?')[1] || '';
  const o = {};
  q.split('&').filter(Boolean).forEach((kv) => { const i = kv.indexOf('='); o[decodeURIComponent(i < 0 ? kv : kv.slice(0, i))] = i < 0 ? '' : decodeURIComponent(kv.slice(i + 1)); });
  return o;
});
async function waitHash(page, want, timeout) {
  await page.waitForFunction((w) => {
    const q = location.hash.split('?')[1] || '';
    const o = {};
    q.split('&').filter(Boolean).forEach((kv) => { const i = kv.indexOf('='); o[decodeURIComponent(i < 0 ? kv : kv.slice(0, i))] = i < 0 ? '' : decodeURIComponent(kv.slice(i + 1)); });
    return Object.keys(w).every(k => (w[k] === null ? !(k in o) : o[k] === w[k]));
  }, want, { timeout: timeout || 5000 });
}

// Requests to /api/ducting* seen by a page, in order.
function trackDucting(page) {
  const seen = [];
  page.on('request', (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith('/api/ducting')) seen.push({ path: u.pathname, q: Object.fromEntries(u.searchParams) });
  });
  return seen;
}
const gridReqs = (seen) => seen.filter(r => r.path === '/api/ducting/grid');
// Waits until a window for time t is drawn (the layer holds it).
async function waitWindow(page, t) {
  await page.waitForFunction((tt) => {
    const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
    let ok = false;
    if (m) m.eachLayer((l) => { if (l._mcDuct && l._data && l._data.times && l._data.times[0] === tt && l._field) ok = true; });
    return ok;
  }, t, { timeout: 20000, polling: 100 });
}

async function run() {
  console.log(`\nDucting overlay E2E, stored global grid — ${BASE}\n`);
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const pages = [];
  const track = async (opts) => { const p = await newPage(browser, opts); pages.push(p); return p; };

  try {
    let cfg, meta, links, freqs = [868, 915];
    await test('/api/config/client: ducting source gfs-grid with history days, never a baseUrl', async () => {
      cfg = await getJson('/api/config/client');
      assert(cfg.ducting && cfg.ducting.source === 'gfs-grid', 'ducting: ' + JSON.stringify(cfg.ducting) + ' (is ducting.source gfs-grid on the server?)');
      assert(cfg.ducting.model === 'gfs' && cfg.ducting.stepDeg === 0.5 && cfg.ducting.attribution && cfg.ducting.historyDays >= 1, 'incomplete: ' + JSON.stringify(cfg.ducting));
      assert(!/baseUrl/i.test(JSON.stringify(cfg)), 'client config has a baseUrl');
    });
    if (!cfg || !cfg.ducting || cfg.ducting.source !== 'gfs-grid') throw new Error('server is not in gfs-grid mode; see the header for how to start it');
    freqs = loraFreqsOf(cfg);

    await test('/api/ducting/meta: stored times every 3 h, past and forecast', async () => {
      meta = await getJson('/api/ducting/meta');
      assert(meta.status === 'ok', `status ${meta.status} (did the fixture generator run? see the header)`);
      assert(meta.source === 'gfs-grid' && meta.model === 'gfs' && meta.runTime > 0 && meta.generatedAt > 0 && meta.historyDays === cfg.ducting.historyDays, 'meta: ' + JSON.stringify(Object.assign({}, meta, { times: meta.times.length })));
      assert(meta.times.length >= 2 && meta.times.every((t, i) => i === 0 || t - meta.times[i - 1] === 3 * 3600), 'times not 3-hourly ascending');
      const now = Date.now() / 1000;
      assert(meta.times[0] < now && meta.times[meta.times.length - 1] > now, `times ${meta.times[0]}..${meta.times[meta.times.length - 1]} do not span now ${Math.round(now)}`);
    });
    if (!meta || meta.status !== 'ok') throw new Error('no stored grid; run the fixture generator (see the header)');
    const times = meta.times;
    const defIdx = defaultIndex(times);

    let evil = null, plain = null, relay = null, tEvil = 0, tPlain = 0, tRelay = 0, method = '';
    await test('/api/ducting/links: the seeded links (observer and relay evidence), High band and normal paths', async () => {
      links = await getJson(`/api/ducting/links?days=${cfg.ducting.historyDays}`);
      assert(links.status === 'ok' && Array.isArray(links.links) && links.days === cfg.ducting.historyDays && links.freqMHz > 0, 'links: ' + JSON.stringify(links).slice(0, 300));
      evil = links.links.find(l => l.a.name === EVIL_NAME);
      plain = links.links.find(l => l.a.name === PLAIN_NAME);
      relay = links.links.find(l => l.a.name === RELAY_NAME);
      assert(evil && plain && relay, `seeded links missing (names: ${links.links.map(l => l.a.name).join(' | ')})`);
      method = links.summary.losMethod;
      assert(method === 'smooth-earth', `losMethod ${method}: the E2E server must not use LoS terrain (deterministic, no downloads)`);
      const re = evil.receptions.find(r => r.via === 'observer' && r.highFrac >= LINK_ENHANCED_FRAC && r.meanGradNkm <= -62 && r.ductFrac >= 0.5 && r.snr != null);
      const rp = plain.receptions.find(r => r.via === 'observer' && r.meanGradNkm != null && r.highFrac >= 0 && r.highFrac < LINK_ENHANCED_FRAC);
      const rr = relay.receptions.find(r => r.via === 'relay' && r.highFrac >= LINK_ENHANCED_FRAC && r.snr == null && r.rssi == null);
      assert(re && rp && rr, 'receptions: ' + JSON.stringify([evil.receptions, plain.receptions, relay.receptions]));
      assert(relay.b.pubkey === plain.a.pubkey, 'the relay link is not received by the seeded relay: ' + JSON.stringify(relay.b));
      tEvil = re.t; tPlain = rp.t; tRelay = rr.t;
      assert([tEvil, tPlain, tRelay].every(t => times.includes(t)), 'reception times are not stored steps');
      assert([evil, plain, relay].every(l => l.distKm > l.horizonKm), 'a seeded link inside the radio horizon');
      assert(evil.count === 2 && evil.b.name, 'evil link: ' + JSON.stringify(evil).slice(0, 300));
      const s = links.summary;
      assert(s.links >= 3 && s.receptions >= 4 && s.withGrid <= s.receptions && s.duringDuct <= s.withGrid && s.unverifiedPairs === 0, 'summary: ' + JSON.stringify(s));
      assert(s.recvMeanGradNkm != null && s.baselineMeanGradNkm != null && s.recvEnhancedFrac >= 0 && s.recvEnhancedFrac <= 1 && s.baselineEnhancedFrac >= 0 && s.baselineEnhancedFrac <= 1, 'summary gradients: ' + JSON.stringify(s));
    });

    // ── Map page ──
    const mp = await track({ theme: 'light' });
    const page = mp.page;
    const seen = trackDucting(page);
    let sum0 = 0, data0 = null;
    await test('map page: overlay entry present, off by default, nothing fetched', async () => {
      await gotoRoute(page, '#/map');
      await openPicker(page);
      const lab = page.locator('.leaflet-control-layers-overlays label[data-tile-overlay-id="ducting"]').first();
      await lab.waitFor({ state: 'attached', timeout: 20000 });
      assert(!(await lab.locator('input').isChecked()), 'ducting is on by default');
      await sleep(300);
      const st = await pageState(page);
      assert(st.legends === 0 && st.times === 0 && !st.cells && !st.fields.length, 'overlay drawn while off');
      assert(!seen.length, 'requests with the overlay off: ' + JSON.stringify(seen));
    });

    await test('enabling it fetches meta, links and the window of the default time; draws the field and one panel: timeline and legend', async () => {
      await setOverlayUi(page, true);
      await waitWindow(page, times[defIdx]);
      await page.mouse.move(5, 5);
      await page.waitForSelector('.mc-duct-legend', { state: 'visible' });
      assert(seen.some(r => r.path === '/api/ducting/meta'), 'no meta request');
      assert(seen.some(r => r.path === '/api/ducting/links' && r.q.days === String(cfg.ducting.historyDays)), 'links request: ' + JSON.stringify(seen.filter(r => r.path === '/api/ducting/links')));
      assert(!seen.some(r => r.path === '/api/ducting'), 'the Open-Meteo endpoint was fetched in grid mode');
      const g = gridReqs(seen);
      assert(g.length >= 1 && g.every(r => r.q.t === String(times[defIdx])) && g.every(r => r.q.bbox.split(',').length === 4), 'grid requests: ' + JSON.stringify(g));
      const L0 = await page.evaluate(inPageLayer);
      data0 = L0.data;
      assert(L0.gridMode && L0.sel === defIdx && data0.times.length === 1 && data0.cells.length > 0, `layer sel=${L0.sel} want ${defIdx}, cells ${data0 && data0.cells.length}`);
      const st = await pageState(page);
      assert(st.cells === data0.cells.length && st.fields.length === 1 && st.fields[0].inDom && st.fields[0].pane === 'mcDuctFieldPane', `cells=${st.cells} fields=${JSON.stringify(st.fields)}`);
      assert(!st.interactive.length, 'interactive overlay layers: ' + st.interactive.join(','));
      assert(!st.edges.length, 'a grid window has an edge line');
      assert(st.panels === 1 && st.legends === 1 && st.times === 1 && st.legendVisible && st.timeVisible, `panels=${st.panels} legends=${st.legends} times=${st.times}`);
      assert(/Past times show the model.s analysis/.test(st.legendText) && /Observed long links/.test(st.legendText) && !/Dotted line/.test(st.legendText), 'legend: ' + st.legendText.slice(0, 300));
      assert(st.legendText.includes(meta.attribution), 'legend lacks the attribution');
      sum0 = await fieldSum(page);
    });

    await test('the ducting panel (timeline and legend) covers no other map control', async () => {
      const ov = await page.evaluate(inPageOverlaps);
      assert(ov.mine === 1 && ov.inside[0] === 2, 'panel, legend and timeline not all visible: ' + JSON.stringify(ov));
      assert(!ov.hits.length, ov.hits.join('; '));
    });

    await test('the timeline spans every stored time placed by time, defaults to the latest <= now, labelled analysis; the now line is on it', async () => {
      const s = await timelineState(page);
      assert(s.ariaLabel === 'Ducting map time', 'aria-label ' + s.ariaLabel);
      assert(s.max === times.length - 1 && !s.disabled, `timeline max ${s.max} want ${times.length - 1}`);
      assert(s.value === defIdx, `timeline at ${s.value} want ${defIdx}`);
      assert(/^\S.+ · analysis, (under 1 h|\d+ h) ago$/.test(s.label) && !/^Time/.test(s.label) && s.valuetext && s.label === s.valuetext, 'label: ' + s.label + ' / ' + s.valuetext);
      const v = await settled(page);
      assert(v.xs.length === times.length && Math.abs(v.scrollLeft - v.xs[defIdx]) <= 1, `strip not on the default time: ${v.scrollLeft} vs ${v.xs[defIdx]}`);
      const step = v.xs[1] - v.xs[0];
      assert(v.xs.every((x, i) => i === 0 || Math.abs(x - v.xs[i - 1] - step) < 0.01), 'stored steps not evenly placed');
      const nowLine = await page.evaluate(() => {
        const l = document.querySelector('.mc-duct-now-line');
        return l && !l.hidden && getComputedStyle(l).display !== 'none' ? parseFloat(l.style.left) : null;
      });
      assert(nowLine != null && nowLine > v.xs[defIdx] - 0.5 && nowLine <= v.xs[defIdx] + step, `now line at ${nowLine}, want between ${v.xs[defIdx]} and ${v.xs[defIdx] + step}`);
      const cut = await page.evaluate(() => {
        const lab = document.querySelector('.mc-duct-time-label'), val = document.querySelector('.mc-duct-time-value');
        const lr = lab.getBoundingClientRect(), vr = val.getBoundingClientRect();
        return { oneLine: lr.height < 24, timeShown: vr.right <= lr.right + 0.5 && vr.left >= lr.left - 0.5 };
      });
      assert(cut.oneLine && cut.timeShown, 'time line cut or wrapped: ' + JSON.stringify(cut));
      const h = await hashParams(page);
      assert(!('ductT' in h), 'default time written to the URL: ' + JSON.stringify(h));
    });

    await test('the field matches the window at the default time; LoRa cells, and only they, outlined', async () => {
      const f = await fieldProblems(page, data0, 0);
      assert(!f.probs.length, f.probs.slice(0, 5).join(' | '));
      assert(f.ducts > 0 && f.calm > 0, `ducts=${f.ducts} calm=${f.calm}`);
      const o = outlineProblems(await pageState(page), data0, 0, (await cssVars(page)).lora, freqs);
      assert(!o.probs.length, o.probs.slice(0, 5).join(' | '));
      assert(o.n > 0, 'no LoRa duct in the window');
    });

    await test('End on the timeline goes to the last forecast time: fetches its window, redraws, labels it forecast and links it', async () => {
      const last = times.length - 1, before = gridReqs(seen).length;
      await page.locator('.mc-duct-timeline').first().focus();
      await page.keyboard.press('End');
      await waitWindow(page, times[last]);
      const g = gridReqs(seen).slice(before);
      assert(g.length === 1 && g[0].q.t === String(times[last]), 'grid requests after End: ' + JSON.stringify(g));
      const s = await timelineState(page);
      assert(s.value === last && /· forecast$/.test(s.label), 'label: ' + s.label);
      const data = (await page.evaluate(inPageLayer)).data;
      const f = await fieldProblems(page, data, 0);
      assert(!f.probs.length, f.probs.slice(0, 5).join(' | '));
      assert((await fieldSum(page)) !== sum0, 'field image did not change');
      assert((await pageState(page)).fields.length === 1, 'the pick added a field image');
      await waitHash(page, { duct: '1', ductT: String(times[last]) });
    });

    await test('the oldest stored time (Home) is labelled analysis, days ago, and linked', async () => {
      await page.locator('.mc-duct-timeline').first().focus();
      await page.keyboard.press('Home');
      await waitWindow(page, times[0]);
      const s = await timelineState(page);
      assert(/· analysis, \d+ d ago$/.test(s.label), 'label: ' + s.label);
      await waitHash(page, { duct: '1', ductT: String(times[0]) });
    });

    await test('back on the default time: ductT leaves the URL and the window comes from memory', async () => {
      const before = gridReqs(seen).length;
      await scrollToTime(page, defIdx);
      await waitWindow(page, times[defIdx]);
      await waitHash(page, { duct: '1', ductT: null });
      assert(gridReqs(seen).length === before, 'refetched a window it held: ' + JSON.stringify(gridReqs(seen).slice(before)));
      assert((await fieldSum(page)) === sum0, 'default-time field differs from the first drawing');
    });

    await test('panning: a burst of moves out of the window fetches once (debounced); a move inside fetches nothing', async () => {
      const c0 = await page.evaluate(`(() => { const m = ${firstMap}; const c = m.getCenter(); return { lat: c.lat, lon: c.lng, z: m.getZoom() }; })()`);
      let before = gridReqs(seen).length;
      await page.evaluate((c) => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        [3, 6, 9].forEach(d => m.setView([c.lat, c.lon + d], c.z, { animate: false }));
      }, c0);
      await sleep(900);
      let g = gridReqs(seen).slice(before);
      assert(g.length === 1, `${g.length} window requests for one burst of 3 moves: ` + JSON.stringify(g));
      const [s, w, n, e] = g[0].q.bbox.split(',').map(Number);
      assert(s <= c0.lat && n >= c0.lat && w <= c0.lon + 9 && e >= c0.lon + 9, `bbox ${g[0].q.bbox} does not hold the final view ${c0.lat},${c0.lon + 9}`);
      before = gridReqs(seen).length;
      await page.evaluate((c) => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        m.setView([c.lat + 0.02, c.lon + 9.02], c.z, { animate: false });
      }, c0);
      await sleep(700);
      assert(gridReqs(seen).length === before, 'a small move inside the window fetched: ' + JSON.stringify(gridReqs(seen).slice(before)));
      await page.evaluate((c) => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        m.setView([c.lat, c.lon], c.z, { animate: false });
      }, c0);
      await waitWindow(page, times[defIdx]);
      await sleep(400);
      assert(gridReqs(seen).length === before, 'moving back refetched a held window');
      assert((await pageState(page)).cells === data0.cells.length, 'moving back did not redraw the first window');
    });

    // ── Long links ──
    async function showLink(pg, l) {
      await pg.evaluate((p) => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        m.closePopup();
        m.fitBounds([[p.a.lat, p.a.lon], [p.b.lat, p.b.lon]], { padding: [120, 120], animate: false });
      }, { a: l.a, b: l.b });
      await sleep(400);
    }
    async function checkDrawnLinks(pg, t) {
      const want = expectedLinks(links.links, t);
      const got = await pg.evaluate(inPageLinks);
      for (const kind of ['duct', 'plain', 'nodata']) {
        const w = want.filter(x => x.kind === kind).map(x => linkKey(x.link)).sort();
        const g = (got[kind] || []).map(segKey).sort();
        assert(JSON.stringify(w) === JSON.stringify(g), `${kind} links drawn ${g.length} want ${w.length} at ${t}`);
        if (g.length) {
          const st = got[kind + 'Style'];
          assert(st.interactive === false && st.pane === 'mcDuctLinePane' && (kind === 'nodata') === !!st.dash, `${kind} style ${JSON.stringify(st)}`);
        }
      }
      const casing = want.filter(x => x.kind !== 'nodata').map(x => linkKey(x.link)).sort();
      assert(JSON.stringify((got.casing || []).map(segKey).sort()) === JSON.stringify(casing), 'casing lines do not match the solid links');
      const legend = await pg.locator('.mc-duct-legend').first().textContent();
      const n = want.length;
      const line = n ? `On the map: ${n} link${n === 1 ? '' : 's'} heard within 90 min of the selected time.` : 'None heard within 90 min of the selected time.';
      assert(legend.includes(line), `legend count: want "${line}" in ${legend.slice(0, 400)}`);
      return want;
    }

    await test('at the time of the High-band reception its link is drawn emphasized; every drawn link matches /api/ducting/links', async () => {
      await scrollToTime(page, times.indexOf(tEvil));
      await waitWindow(page, tEvil);
      await showLink(page, evil);
      const want = await checkDrawnLinks(page, tEvil);
      assert(want.some(x => x.link === evil && x.kind === 'duct'), 'the seeded High-band link is not expected as emphasized');
    });

    // Clicks along link l until its popup opens (another link or a marker
    // may sit on a point); returns the popup text.
    async function openLinkPopup(pg, l) {
      for (const f of [0.5, 0.42, 0.58, 0.35, 0.65, 0.28, 0.72]) {
        const p = await pg.evaluate(DE.inPagePoint, { lat: l.a.lat + (l.b.lat - l.a.lat) * f, lon: l.a.lon + (l.b.lon - l.a.lon) * f });
        if (p.blocked) continue;
        await pg.mouse.click(p.x, p.y);
        try {
          await pg.waitForSelector('.leaflet-popup .mc-duct-link-popup', { timeout: 2000 });
        } catch (_) { continue; }
        const t = await pg.locator('.leaflet-popup .mc-duct-link-popup').first().innerText();
        if (t.includes('Sent by: ' + l.a.name)) return t;
        await pg.evaluate(`${firstMap}.closePopup()`);
        await pg.waitForSelector('.leaflet-popup .mc-duct-link-popup', { state: 'detached', timeout: 2000 }).catch(() => {});
      }
      throw new Error('could not open the popup of ' + l.a.name);
    }

    await test('clicking the link opens its popup; the node-chosen name is text, never markup; the click still reaches the map', async () => {
      const pre = await page.evaluate(() => typeof window.__dx);
      assert(pre === 'undefined', 'the sender name already ran as markup somewhere on the map page (window.__dx is set)');
      await page.evaluate(`(() => { const m = ${firstMap}; window.__ductClicks = 0; window.__ductOn = () => { window.__ductClicks++; }; m.on('click', window.__ductOn); })()`);
      const text = await openLinkPopup(page, evil);
      const res = await page.evaluate(`(() => { const m = ${firstMap}; m.off('click', window.__ductOn); const box = document.querySelector('.leaflet-popup .mc-duct-link-popup');
        return { clicks: window.__ductClicks, imgs: box.querySelectorAll('img').length, dx: typeof window.__dx, cellPopup: !!document.querySelector('.leaflet-popup .mc-duct-popup:not(.mc-duct-link-popup)') }; })()`);
      assert(res.imgs === 0 && res.dx === 'undefined', 'the name was rendered as markup: ' + JSON.stringify(res));
      assert(res.clicks >= 1, 'the page\'s map click listener did not get the click: ' + JSON.stringify(res));
      assert(!res.cellPopup, 'a cell popup opened over the link popup');
      assert(/^Long link, heard directly/.test(text) && text.includes('Heard by: ' + evil.b.name), 'popup: ' + text);
      assert(text.includes(`${Math.round(evil.distKm)} km apart; radio horizon ${Math.round(evil.horizonKm)} km`), 'popup distances (smooth-earth): ' + text);
      const r = evil.receptions.find(x => x.t === tEvil);
      const line = text.split('\n').find(x => x.includes('heard directly by the observer')) || '';
      assert(line.includes('SNR ') && line.includes('path gradient ' + grad(r.meanGradNkm)) && line.includes('High band or stronger along ' + pct(r.highFrac) + ' of the path') &&
        line.includes('LoRa duct along ' + pct(r.ductFrac)), 'popup reception line: ' + line + ' / ' + text);
      assert(text.includes('Heard 2 times in the period'), 'popup count: ' + text);
      assert(text.includes('High band: ' + grad(-62) + ' or stronger') && text.includes(`may carry ${Math.round(links.freqMHz * 1000) / 1000} MHz`), 'popup note: ' + text);
    });

    await test('the relay-evidence link is drawn emphasized at its time; its popup says relayed, without SNR', async () => {
      await scrollToTime(page, times.indexOf(tRelay));
      await waitWindow(page, tRelay);
      await showLink(page, relay);
      const want = await checkDrawnLinks(page, tRelay);
      assert(want.some(x => x.link === relay && x.kind === 'duct'), 'the seeded relay link is not expected as emphasized');
      const text = await openLinkPopup(page, relay);
      const line = text.split('\n').find(x => x.includes('relayed: the second repeater heard the first directly')) || '';
      assert(line && !/SNR|RSSI/.test(line) && line.includes('High band or stronger along 100% of the path'), 'relay reception line: ' + line + ' / ' + text);
      assert(text.includes('Heard by: ' + PLAIN_NAME), 'relay popup: ' + text);
      await scrollToTime(page, times.indexOf(tEvil));
      await waitWindow(page, tEvil);
      await showLink(page, evil);
      await openLinkPopup(page, evil);
    });

    await test('moving the timeline away closes the link popup and drops the link; the no-duct link is drawn plain at its time', async () => {
      const far = times.findIndex(t => !links.links.some(l => l.receptions.some(r => Math.abs(r.t - t) <= LINK_WINDOW_S)));
      assert(far >= 0, 'every stored time has a link near it');
      assert(await page.locator('.leaflet-popup .mc-duct-link-popup').count(), 'no link popup open before the move');
      await scrollToTime(page, far);
      await waitWindow(page, times[far]);
      // Leaflet fades a closed popup out and removes it 200 ms later.
      await page.waitForSelector('.leaflet-popup .mc-duct-link-popup', { state: 'detached', timeout: 2000 })
        .catch(() => { throw new Error('link popup still open at a time without the link'); });
      await checkDrawnLinks(page, times[far]);
      await scrollToTime(page, times.indexOf(tPlain));
      await waitWindow(page, tPlain);
      const want = await checkDrawnLinks(page, tPlain);
      assert(want.some(x => x.link === plain && x.kind === 'plain'), 'the seeded no-duct link is not expected as plain');
    });

    await test('legend: the period summary leads with the gradient comparison, beyond the radio horizon', async () => {
      const s = links.summary, days = links.days;
      const txt = await page.locator('.mc-duct-links-summary').first().textContent();
      const n = (v, w) => `${v.toLocaleString('en-US')} ${w}${v === 1 ? '' : 's'}`;
      let want = `Last ${days} days: ${n(s.receptions, 'reception')} on ${n(s.links, 'node pair')} beyond the radio horizon.` +
        ` Along their paths the modelled refractivity gradient averaged ${grad(s.recvMeanGradNkm)} when heard, against ${grad(s.baselineMeanGradNkm)} for the same paths over the whole period;` +
        ` half or more of the path was in the High band or stronger for ${pct(s.recvEnhancedFrac)} of receptions vs ${pct(s.baselineEnhancedFrac)} overall.`;
      if (s.duringDuct) want += ` Half or more of the path was in a modelled duct that may carry LoRa for ${n(s.duringDuct, 'reception')}.`;
      assert(txt === want, `summary:\n  got  ${txt}\n  want ${want}`);
      assert(txt === await page.evaluate((a) => window.MC_ductLinksSummary(a.s, a.d, a.m), { s, d: days, m: method }), 'legend differs from MC_ductLinksSummary');
    });

    await test('legend: "Observed long links" folds on a short screen (800 px) and opens on click, still clear of the other controls', async () => {
      const tog = page.locator('.mc-duct-links-toggle').first();
      assert((await tog.getAttribute('aria-expanded')) === 'false', 'long-link section not folded at 800 px tall');
      const keyRow = 'Half or more of the path in the High band or stronger (model)';
      let legend = await page.locator('.mc-duct-legend').first().textContent();
      assert(!legend.includes(keyRow) && legend.includes('Last ') && /On the map: |None heard within/.test(legend), 'folded section: ' + legend.slice(0, 500));
      await tog.click();
      await page.waitForFunction(() => { const t = document.querySelector('.mc-duct-links-toggle'); return t && t.getAttribute('aria-expanded') === 'true'; }, null, { timeout: 3000 });
      legend = await page.locator('.mc-duct-legend').first().textContent();
      assert(legend.includes(keyRow) && legend.includes('Less of the path in the High band') && legend.includes('Dashed: no model data for that time'), 'open section: ' + legend.slice(0, 600));
      assert(await page.evaluate(() => document.activeElement && document.activeElement.classList.contains('mc-duct-links-toggle')), 'the toggle lost the focus');
      const ov = await page.evaluate(inPageOverlaps);
      assert(!ov.hits.length, 'open: ' + ov.hits.join('; '));
      const sc = await page.evaluate(() => { const b = document.querySelector('.mc-duct-legend'); return { sh: b.scrollHeight, ch: b.clientHeight, oy: getComputedStyle(b).overflowY }; });
      assert(sc.oy === 'auto' && sc.ch > 0, 'legend scroll ' + JSON.stringify(sc));
      await page.locator('.mc-duct-links-toggle').first().click();
      await page.waitForFunction(() => { const t = document.querySelector('.mc-duct-links-toggle'); return t && t.getAttribute('aria-expanded') === 'false'; }, null, { timeout: 3000 });
    });

    await test('the chevron collapses the whole legend body, long links included; the timeline stays', async () => {
      const btn = page.locator('.mc-duct-collapse').first();
      const h0 = (await panelState(page)).h;
      await btn.click();
      const p = await panelState(page);
      assert(!p.legend && p.time && p.expanded === 'false', 'collapsed: ' + JSON.stringify(p));
      assert(!(await page.locator('.mc-duct-links-toggle').first().isVisible()) && !(await page.locator('.mc-duct-links-summary').first().isVisible()), 'long links still visible');
      assert(p.h < h0 - 100, `panel did not shrink (${h0} -> ${p.h})`);
      await btn.click();
      assert((await panelState(page)).legend, 'not expanded again');
    });

    await test('moved to the top of the map with the links table open, the panel stays inside the map; the legend body scrolls', async () => {
      await page.locator('.mc-duct-links-toggle').first().click();
      await page.waitForFunction(() => { const t = document.querySelector('.mc-duct-links-toggle'); return t && t.getAttribute('aria-expanded') === 'true'; }, null, { timeout: 3000 });
      const hb = await page.locator('.mc-duct-drag').first().boundingBox();
      await page.mouse.move(hb.x + hb.width / 2, hb.y + hb.height / 2);
      await page.mouse.down();
      await page.mouse.move(hb.x + 300, hb.y - 200, { steps: 6 });
      await page.mouse.move(hb.x + 300, 0, { steps: 6 });
      await page.mouse.up();
      await sleep(300);
      const box = await page.evaluate(() => {
        const p = document.querySelector('.mc-duct-panel'), b = document.querySelector('.mc-duct-legend');
        const r = p.getBoundingClientRect(), c = p.parentNode.getBoundingClientRect();
        return { floating: p.classList.contains('mc-duct-panel-floating'), top: r.top - c.top, bottom: c.bottom - r.bottom, sh: b.scrollHeight, ch: b.clientHeight, grad: !!p.querySelector('.mc-duct-grad-table') };
      });
      assert(box.floating && box.grad, 'not floating with the gradient table: ' + JSON.stringify(box));
      assert(box.top >= 7 && box.bottom >= 7, 'panel leaves the map: ' + JSON.stringify(box));
      assert(box.ch > 0 && box.sh >= box.ch, 'legend body: ' + JSON.stringify(box));
      // The map grows shorter: still inside.
      await page.setViewportSize({ width: 1280, height: 640 });
      await sleep(500);
      const b2 = await page.evaluate(() => { const p = document.querySelector('.mc-duct-panel'); const r = p.getBoundingClientRect(), c = p.parentNode.getBoundingClientRect(); return { top: r.top - c.top, bottom: c.bottom - r.bottom }; });
      assert(b2.top >= 7 && b2.bottom >= 7, 'after a resize the panel leaves the map: ' + JSON.stringify(b2));
      await page.setViewportSize({ width: 1280, height: 800 });
      await page.locator('.mc-duct-reset').first().click();
      assert((await panelState(page)).inCorner, 'Reset did not dock it');
      await page.locator('.mc-duct-links-toggle').first().click();
      const ov = await page.evaluate(inPageOverlaps);
      assert(!ov.hits.length, 'after reset: ' + ov.hits.join('; '));
    });

    await test('disabling the overlay removes everything it drew and its URL parameters', async () => {
      await setOverlayUi(page, false);
      await page.waitForFunction(() => !document.querySelector('.mc-duct-panel') && !document.querySelector('.mc-duct-legend') && !document.querySelector('.mc-duct-time'), null, { timeout: 5000 });
      const st = await pageState(page);
      assert(!st.cells && !st.lora.length && !st.fields.length && st.panels === 0 && st.legends === 0 && st.times === 0, 'left over: ' + JSON.stringify({ cells: st.cells, lora: st.lora.length, fields: st.fields.length }));
      assert(!Object.keys(await page.evaluate(inPageLinks)).length, 'link lines left on the map');
      await waitHash(page, { duct: null, ductT: null });
    });

    await test('map page has no page errors', async () => { assert(!page.__errors.length, page.__errors.slice(0, 3).join(' | ')); });

    // ── Deep link ──
    await test('deep link #/map?duct=1&ductT=<stored time> turns the overlay on at that time', async () => {
      const k = Math.max(0, defIdx - 2);
      const dp = await track({ theme: 'light' });
      const ds = trackDucting(dp.page);
      await gotoRoute(dp.page, `#/map?duct=1&ductT=${times[k]}`);
      await waitWindow(dp.page, times[k]);
      const s = await timelineState(dp.page);
      assert(s.value === k, `timeline at ${s.value} want ${k}`);
      const v = await settled(dp.page);
      assert(Math.abs(v.scrollLeft - v.xs[k]) <= 1, `strip not on the linked time: ${v.scrollLeft} vs ${v.xs[k]}`);
      assert(gridReqs(ds).length >= 1 && gridReqs(ds).every(r => r.q.t === String(times[k])), 'windows fetched: ' + JSON.stringify(gridReqs(ds)));
      assert(await dp.page.locator('label[data-tile-overlay-id="ducting"] input').first().isChecked(), 'overlay not ticked');
      const h = await hashParams(dp.page);
      assert(h.duct === '1' && h.ductT === String(times[k]), 'hash: ' + JSON.stringify(h));
      assert(!dp.page.__errors.length, dp.page.__errors.slice(0, 3).join(' | '));
    });

    await test('a deep link to a time that is not stored shows the default time', async () => {
      const dp = await track({ theme: 'light' });
      await gotoRoute(dp.page, `#/map?duct=1&ductT=${times[defIdx] + 60}`);
      await waitWindow(dp.page, times[defIdx]);
      const s = await timelineState(dp.page);
      assert(s.value === defIdx, `timeline at ${s.value} want ${defIdx}`);
      assert(!dp.page.__errors.length, dp.page.__errors.slice(0, 3).join(' | '));
    });

    // ── Timeline by pointer, the link, a reload, Now ──
    {
      const k = Math.max(0, defIdx - 3);
      const tp = await track({ theme: 'light' });
      const tpage = tp.page;
      const ts = trackDucting(tpage);
      await test('scrolling the strip into the past (mouse wheel) picks an analysis time; ductT is written once, after the debounce', async () => {
        await gotoRoute(tpage, '#/map?duct=1');
        await waitWindow(tpage, times[defIdx]);
        await tpage.mouse.move(5, 5);
        await settled(tpage);
        // Count the link writes from here on.
        await tpage.evaluate(() => {
          window.__ductWrites = [];
          const real = history.replaceState.bind(history);
          history.replaceState = (st, t, h) => { window.__ductWrites.push(String(h)); return real(st, t, h); };
        });
        const v0 = await tpage.evaluate(DE.inPageView);
        const step = v0.xs[1] - v0.xs[0];
        const box = await tpage.locator('.mc-duct-timeline').first().boundingBox();
        await tpage.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
        for (let n = 0; n < 3; n++) { await tpage.mouse.wheel(0, -step); await sleep(60); }
        await tpage.waitForFunction((i) => document.querySelector('.mc-duct-timeline').getAttribute('aria-valuenow') === String(i), k, { timeout: 5000 });
        const v = await settled(tpage);
        assert(v.idx === k && Math.abs(v.scrollLeft - v.xs[k]) <= 1, `wheel landed on ${v.idx} (scrollLeft ${v.scrollLeft}), want ${k} (${v.xs[k]})`);
        assert(DE.sameView(v0, v), 'the wheel on the strip moved the map');
        const s = await timelineState(tpage);
        assert(/^\S.+ · analysis, \d+ h ago$/.test(s.label) && !/^Time/.test(s.label), 'label: ' + s.label);
        await waitWindow(tpage, times[k]);
        await waitHash(tpage, { duct: '1', ductT: String(times[k]) });
        await sleep(500);
        const w = await tpage.evaluate(() => window.__ductWrites.filter(h => /ductT=/.test(h)));
        assert(w.length === 1 && w[0].includes('ductT=' + times[k]), 'link writes for one scroll: ' + JSON.stringify(w));
        assert(gridReqs(ts).some(r => r.q.t === String(times[k])), 'no window fetched for the scrolled-to time');
      });

      await test('reloading with that link restores the time, the strip position and the analysis wording', async () => {
        const before = gridReqs(ts).length;
        await tpage.reload({ waitUntil: 'domcontentloaded' });
        await waitWindow(tpage, times[k]);
        const v = await settled(tpage);
        assert(v.idx === k && Math.abs(v.scrollLeft - v.xs[k]) <= 1, `after reload: idx ${v.idx} scrollLeft ${v.scrollLeft}, want ${k} at ${v.xs[k]}`);
        const s = await timelineState(tpage);
        assert(/^\S.+ · analysis, \d+ h ago$/.test(s.label) && !/^Time/.test(s.label), 'label: ' + s.label);
        const g = gridReqs(ts).slice(before);
        assert(g.length >= 1 && g.every(r => r.q.t === String(times[k])), 'windows after reload: ' + JSON.stringify(g));
        assert(await tpage.locator('label[data-tile-overlay-id="ducting"] input').first().isChecked(), 'overlay not ticked');
        const h = await hashParams(tpage);
        assert(h.duct === '1' && h.ductT === String(times[k]), 'hash: ' + JSON.stringify(h));
      });

      await test('Now goes to the latest time <= now and removes ductT (the overlay stays linked)', async () => {
        await tpage.locator('.mc-duct-now').first().click();
        await waitWindow(tpage, times[defIdx]);
        const v = await settled(tpage);
        assert(v.idx === defIdx && Math.abs(v.scrollLeft - v.xs[defIdx]) <= 1, `Now picked ${v.idx}, want ${defIdx}`);
        await waitHash(tpage, { duct: '1', ductT: null });
        assert(/^\S.+ · analysis, (under 1 h|\d+ h) ago$/.test((await timelineState(tpage)).label), 'label after Now');
        assert(!tpage.__errors.length, tpage.__errors.slice(0, 3).join(' | '));
      });
    }

    // ── Planner: map clicks still place sites with the overlay on ──
    if (cfg.planner) {
      await test('planner page: with the overlay on, a click on the field places a site (and opens no ducting popup)', async () => {
        const pp = await track({ theme: 'light', overlays: ['ducting'] });
        await gotoRoute(pp.page, '#/tools/planner?mode=coverage');
        await pp.page.waitForSelector('.mc-duct-legend', { state: 'visible', timeout: 25000 });
        await pp.page.waitForFunction(() => {
          const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
          let ok = false;
          if (m) m.eachLayer((l) => { if (l._mcDuct && l._field && l._data) ok = true; });
          return ok;
        }, null, { timeout: 20000 });
        await pp.page.mouse.move(5, 5);
        assert(!(await pp.page.locator('path.mc-pl-cov-site').count()), 'a site before any click');
        const data = (await pp.page.evaluate(inPageLayer)).data;
        const cell = data.cells.find(c => c.c[0] >= 3) || data.cells.find(c => c.c[0] >= 1);
        const pts = await pointsIn(pp.page, cell, data.stepDeg, 4);
        assert(pts.length, 'no free point in the cell');
        await pp.page.mouse.click(pts[0].x, pts[0].y);
        await pp.page.waitForSelector('path.mc-pl-cov-site', { timeout: 5000 }).catch(() => {});
        await sleep(300);
        const res = await pp.page.evaluate(() => ({ site: document.querySelectorAll('path.mc-pl-cov-site').length, duct: !!document.querySelector('.leaflet-popup .mc-duct-popup') }));
        assert(res.site === 1 && !res.duct, `click at ${cell.lat},${cell.lon}: ` + JSON.stringify(res));
        assert(!(await pageState(pp.page)).interactive.length, 'interactive overlay layers');
        assert(!pp.page.__errors.length, pp.page.__errors.slice(0, 3).join(' | '));
      });
    } else {
      R.skip('planner page: a click on the field places a site', 'planner disabled on this server');
    }

    // ── Endpoint states ──
    for (const [name, status, body, want] of [
      ['pending meta', 200, { status: 'pending', source: 'gfs-grid', times: [] }, /being prepared/],
      ['HTTP 500 meta', 500, { error: 'x' }, /unavailable/],
    ]) {
      await test(`the status line explains a ${name} answer, also with the legend collapsed; nothing drawn`, async () => {
        const sp = await track({ theme: 'light', overlays: ['ducting'] });
        await sp.page.route('**/api/ducting/meta', (route) => route.fulfill({ status, json: body }));
        await gotoRoute(sp.page, '#/map');
        await sp.page.waitForSelector('.mc-duct-status', { state: 'visible', timeout: 25000 });
        await sp.page.waitForFunction((src) => new RegExp(src).test((document.querySelector('.mc-duct-status') || {}).textContent || ''), want.source, { timeout: 10000 });
        let ss = await sp.page.evaluate(inPageStatus);
        assert(ss.count === 1 && !ss.inLegend && want.test(ss.text), 'status: ' + JSON.stringify(ss));
        await sp.page.locator('.mc-duct-collapse').first().click();
        await sleep(200);
        ss = await sp.page.evaluate(inPageStatus);
        assert(!ss.legend && want.test(ss.text), 'collapsed: ' + JSON.stringify(ss));
        const st = await pageState(sp.page);
        assert(!st.cells && !st.fields.length, 'field drawn');
        const errs = sp.page.__errors.filter(e => !/Failed to load resource/.test(e));
        assert(!errs.length, errs.slice(0, 3).join(' | '));
      });
    }
    await test('a failing /api/ducting/links leaves the field and says the links are unavailable', async () => {
      const sp = await track({ theme: 'light', overlays: ['ducting'] });
      await sp.page.route('**/api/ducting/links*', (route) => route.fulfill({ status: 500, json: { error: 'x' } }));
      await gotoRoute(sp.page, '#/map');
      await waitWindow(sp.page, times[defIdx]);
      await sp.page.waitForFunction(() => /Long links are not available right now/.test((document.querySelector('.mc-duct-legend') || {}).textContent || ''), null, { timeout: 10000 });
      assert((await pageState(sp.page)).fields.length === 1, 'no field');
      assert(!Object.keys(await sp.page.evaluate(inPageLinks)).length, 'links drawn without an answer');
      const errs = sp.page.__errors.filter(e => !/Failed to load resource/.test(e));
      assert(!errs.length, errs.slice(0, 3).join(' | '));
    });
  } finally {
    for (const p of pages) await p.context.close().catch(() => {});
    await browser.close().catch(() => {});
  }
  console.log(`\n${R.summary()}`);
  process.exit(R.failed ? 1 : 0);
}

run().catch((e) => { console.error(e); process.exit(1); });
