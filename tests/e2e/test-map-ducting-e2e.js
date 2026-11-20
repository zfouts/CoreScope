/**
 * E2E for the tropospheric-ducting map overlay, against a real CoreScope
 * server whose ducting source is a fake Open-Meteo (tests/e2e/lib/
 * fake-open-meteo.js, scenario 'mixed'). Nothing here reaches the real
 * Open-Meteo or a real tile host. The stored global grid (ducting.source
 * "gfs-grid", the default) has its own suite: test-map-ducting-grid-e2e.js.
 *
 * CI: runs in the GitHub Actions E2E job (.github/workflows/deploy.yml, e2e
 * shard 1): the fake Open-Meteo on 13597, then a ducting + planner server on
 * 13596, then `suite 1 test-map-ducting-e2e.js` with CHROMIUM_REQUIRE=1
 * BASE_URL=http://localhost:13596.
 *
 * Locally, start the pieces in this order (the server's first refresh runs at startup;
 * if the fake is not listening yet it fails and the server retries only
 * after 5 minutes):
 *
 *   mkdir -p /tmp/ducting-e2e && cp test-fixtures/e2e-fixture.db /tmp/ducting-e2e/
 *   cat > /tmp/ducting-e2e/config.json <<'JSON'
 *   { "port": 13596,
 *     "planner": { "enabled": true, "defaultRangeKm": 30, "newAntennaM": 10,
 *                  "existingAntennaM": 20, "maxAreaKm": 100, "nodeMarginKm": 20, "maxLegKm": 100 },
 *     "ducting": { "enabled": true, "source": "open-meteo", "baseUrl": "http://127.0.0.1:13597",
 *                  "gridStepDeg": 0.5, "forecastHours": 48 } }
 *   JSON
 *   node tests/e2e/lib/fake-open-meteo.js --port 13597 --scenario mixed &
 *   (cd cmd/server && go build -o /tmp/ducting-e2e/corescope-server .)   # or: scripts/map-perf/serve-dataset.sh build worktree <dir>
 *   /tmp/ducting-e2e/corescope-server -config-dir /tmp/ducting-e2e \
 *       -db /tmp/ducting-e2e/e2e-fixture.db -port 13596 -public public &
 *   BASE_URL=http://localhost:13596 node tests/e2e/test-map-ducting-e2e.js
 *
 * (the sqlite driver needs cgo: build with the Makefile or the Docker build in
 * scripts/map-perf/serve-dataset.sh, never CGO_ENABLED=0.)
 *
 * If something already listens on FAKE_PORT the test uses that fake through
 * its /__state endpoint; otherwise it starts its own, which only helps when
 * the server is started after it (the server then retries within 5 minutes
 * only if it failed; restart it).
 *
 * Env:
 *   BASE_URL       CoreScope server (default http://localhost:13596)
 *   FAKE_PORT      fake Open-Meteo port (default 13597)
 *   CHROMIUM_PATH  optional browser binary
 */
'use strict';
const { chromium } = require('playwright');
const MH = require('./lib/map-harness');
const FAKE = require('./lib/fake-open-meteo');
const DE = require('./lib/ducting-e2e');
const { loraFreqsOf, sig2, shownFmin, fmtFreq, fieldN, assert, sleep, openPicker, setOverlayUi, pageState, waitCells,
  inPageOverlaps, cssVars, inPageFieldProbe, fieldProblems, pointsIn, setSlider } = DE;

const BASE = (process.env.BASE_URL || 'http://localhost:13596').replace(/\/+$/, '');
const FAKE_PORT = parseInt(process.env.FAKE_PORT || '13597', 10);
const SCENARIO = 'mixed';
// The LoRa frequencies the overlay judges (DE.loraFreqsOf); set once the
// client config is read.
let LORA_FREQS = [868, 915];
const outlineProblems = (st, data, idx, lora) => DE.outlineProblems(st, data, idx, lora, LORA_FREQS);
const newPage = (browser, opts) => DE.newPage(browser, BASE, opts);
const gotoRoute = (page, hash, waitControl) => DE.gotoRoute(page, BASE, hash, waitControl);
const R = DE.createRunner();
const test = R.test, skip = R.skip;

// ── Fake Open-Meteo: own instance, or the one already listening ─────────────
async function getFake() {
  let own = null;
  try {
    own = await FAKE.start({ port: FAKE_PORT, scenario: SCENARIO });
    console.log(`started fake Open-Meteo on ${own.url} (${SCENARIO})`);
    return {
      url: own.url, own,
      state: async () => ({ scenario: own.scenario, requests: own.requests, violations: own.violations }),
      close: () => own.close(),
    };
  } catch (e) {
    if (e.code !== 'EADDRINUSE') throw e;
  }
  const url = `http://127.0.0.1:${FAKE_PORT}`;
  const get = async () => (await fetch(url + '/__state', { signal: AbortSignal.timeout(5000) })).json();
  const st = await get().catch(() => null);
  if (!st) throw new Error(`port ${FAKE_PORT} is in use by something that is not the fake Open-Meteo`);
  if (st.scenario !== SCENARIO) {
    await fetch(`${url}/__scenario?name=${SCENARIO}`);
    console.log(`fake on ${url} was '${st.scenario}', switched to '${SCENARIO}' (the server only sees it at its next refresh)`);
  }
  console.log(`using the fake Open-Meteo already listening on ${url}`);
  return { url, own: null, state: get, close: async () => {} };
}

async function getJson(path) {
  const r = await fetch(BASE + path, { signal: AbortSignal.timeout(20000) });
  if (!r.ok) throw new Error(`${path} -> HTTP ${r.status}`);
  return r.json();
}


async function run() {
  console.log(`\nDucting overlay E2E — ${BASE}\n`);
  const fake = await getFake();
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const pages = [];
  const track = async (opts) => { const p = await newPage(browser, opts); pages.push(p); return p; };

  try {
    let cfg;
    await test('/api/config/client has ducting and never the baseUrl', async () => {
      cfg = await getJson('/api/config/client');
      assert(cfg.ducting && typeof cfg.ducting === 'object', 'no ducting block (is ducting.enabled set on the server?)');
      assert(cfg.ducting.source === 'open-meteo', 'ducting.source ' + cfg.ducting.source + ': this suite needs "source": "open-meteo" in the server config');
      assert(cfg.ducting.model === 'gfs' && cfg.ducting.modelLabel && cfg.ducting.stepDeg > 0 && cfg.ducting.attribution, 'incomplete: ' + JSON.stringify(cfg.ducting));
      const txt = JSON.stringify(cfg);
      assert(!/baseUrl/i.test(txt) && !txt.includes('127.0.0.1:' + FAKE_PORT), 'client config leaks the ducting baseUrl');
    });
    LORA_FREQS = loraFreqsOf(cfg);
    if (!cfg || !cfg.ducting) throw new Error('server has no ducting configured; see the header for how to start it');

    let data;
    await test('/api/ducting reaches status ok', async () => {
      const t0 = Date.now();
      for (;;) {
        data = await getJson('/api/ducting');
        if (data.status === 'ok') break;
        if (Date.now() - t0 > 30000) {
          throw new Error(`status ${data.status} ${data.error || ''} after 30 s. If the server started before the fake Open-Meteo listened, restart it (it retries only after 5 min)`);
        }
        await sleep(500);
      }
      assert(data.model === 'gfs' && data.attribution && data.generatedAt > 0, 'metadata missing');
      assert(data.cells.length > 0 && data.times.length === 48, `cells=${data.cells.length} times=${data.times.length}`);
      assert(data.times.every((t, i) => i === 0 || t - data.times[i - 1] === 3600), 'times are not hourly ascending');
    });

    await test('cell classes and frequencies match the scenario', async () => {
      const probs = [];
      data.cells.forEach((c) => {
        ['c', 'f', 'b', 't', 'dm', 'g', 'n'].forEach((k) => { if (!Array.isArray(c[k]) || c[k].length !== data.times.length) probs.push(`${c.lat},${c.lon}: ${k} has ${c[k] && c[k].length} entries`); });
        data.times.forEach((_, h) => {
          const want = FAKE.intendedKind(SCENARIO, c.lat, c.lon, h).cls;
          if (c.c[h] !== want) probs.push(`${c.lat},${c.lon} h${h}: class ${c.c[h]} want ${want}`);
          const a = FAKE.analyzeCell(SCENARIO, c.lat, c.lon, h, data.times.length);
          if (a.cls !== c.c[h]) probs.push(`${c.lat},${c.lon} h${h}: fake port says ${a.cls}, server ${c.c[h]}`);
          if (c.c[h] === 4 && !(c.f[h] > 0 && c.f[h] <= 868)) probs.push(`${c.lat},${c.lon} h${h}: surface duct fmin ${c.f[h]} does not trap 868 MHz`);
          if (c.c[h] >= 3 && Math.abs(c.f[h] - Math.round(a.fmin)) > 1) probs.push(`${c.lat},${c.lon} h${h}: fmin ${c.f[h]} vs ${Math.round(a.fmin)}`);
          if (c.c[h] <= 2 && c.f[h] !== 0) probs.push(`${c.lat},${c.lon} h${h}: class ${c.c[h]} with fmin ${c.f[h]}`);
          if (Array.isArray(c.n) && !Number.isInteger(c.n[h])) probs.push(`${c.lat},${c.lon} h${h}: n ${c.n[h]} is not a whole number`);
        });
      });
      assert(!probs.length, probs.slice(0, 6).join(' | '));
      const seen = new Set();
      data.cells.forEach(c => c.c.forEach(v => seen.add(v)));
      [1, 2, 3, 4].forEach(k => assert(seen.has(k), `no class ${k} cell in the data (fixture has too few nodes? cells=${data.cells.length})`));
      assert(data.cells.some(c => new Set(c.c).size > 1), 'no cell changes over the forecast hours; the slider could not change a colour');
    });

    await test('the fake saw valid, batched requests only', async () => {
      const st = await fake.state();
      assert(!st.violations.length, 'fake rejected requests: ' + st.violations.slice(0, 3).join('; '));
      const ok = st.requests.filter(r => r.valid && r.path === '/v1/forecast');
      assert(ok.length > 0, 'the fake saw no forecast request');
      ok.forEach((r) => { assert(r.locations >= 1 && r.locations <= FAKE.BATCH_MAX, `request with ${r.locations} locations`); assert(r.hours === 48, `forecast_hours=${r.hours}`); });
      const totalLoc = ok.reduce((a, r) => a + r.locations, 0);
      assert(totalLoc >= data.cells.length, `requested ${totalLoc} locations for ${data.cells.length} cells`);
      const refreshes = Math.max(1, Math.round(totalLoc / data.cells.length));
      const perRefresh = Math.ceil(data.cells.length / FAKE.BATCH_MAX);
      assert(ok.length <= perRefresh * refreshes, `${ok.length} requests for ${refreshes} refresh(es) of ${data.cells.length} cells, want <= ${perRefresh} each`);
    });

    // ── Map page ──
    const mp = await track({ theme: 'light' });
    const page = mp.page;
    let idx0 = 0;
    await test('map page: overlay entry sits in the Overlays group and is off by default', async () => {
      await gotoRoute(page, '#/map');
      await openPicker(page);
      const lab = page.locator('.leaflet-control-layers-overlays label[data-tile-overlay-id="ducting"]').first();
      await lab.waitFor({ state: 'attached', timeout: 20000 });
      assert(/ducting/i.test(await lab.textContent()), 'label text: ' + (await lab.textContent()));
      assert(!(await lab.locator('input').isChecked()), 'ducting is on by default');
      const st = await pageState(page);
      assert(st.legends === 0 && st.times === 0 && !st.cells && !st.lora.length && !st.fields.length, 'legend/time/cells/field present with the overlay off');
    });

    await test('enabling it draws cells, with a visible, unobstructed legend and time slider', async () => {
      await setOverlayUi(page, true);
      await waitCells(page, 1);
      await page.mouse.move(5, 5);
      await page.waitForSelector('.mc-duct-legend', { state: 'visible' });
      const st = await pageState(page);
      assert(st.maps === 1 && st.cells === data.cells.length, `maps=${st.maps} cells drawn=${st.cells} cells=${data.cells.length}`);
      assert(!st.interactive.length, 'interactive overlay layers: ' + st.interactive.join(','));
      assert(st.fields.length === 1 && st.fields[0].inDom && st.fields[0].pane === 'mcDuctFieldPane', 'field images: ' + JSON.stringify(st.fields));
      assert(st.legends === 1 && st.times === 1 && st.legendVisible && st.timeVisible, `legends=${st.legends} times=${st.times}`);
      assert(st.legendText.includes('may carry LoRa at ' + LORA_FREQS.map(fmtFreq).join(' / ') + ' MHz') && /Above normal/.test(st.legendText) && /Very high/.test(st.legendText) && !/Enhanced|Strong/.test(st.legendText) && /lowest 1\.5 km \(N\/km\)/.test(st.legendText) && /Updated/.test(st.legendText), 'legend text: ' + st.legendText.slice(0, 200));
      const ov = await page.evaluate(inPageOverlaps);
      assert(ov.mine === 2, 'legend and time box not both visible');
      assert(!ov.hits.length, ov.hits.join('; '));
    });

    await test('the forecast area has a dotted outline on its outer boundary, and the legend explains it', async () => {
      const st = await pageState(page);
      assert(st.edges.length === 1, 'edge layers: ' + st.edges.length);
      const e = st.edges[0];
      assert(e.dash === '1 5' && e.weight === 1 && e.interactive === false && e.color, 'edge style: ' + JSON.stringify([e.dash, e.weight, e.interactive, e.color]));
      assert(/Dotted line: edge of the forecast area/.test(st.legendText), 'legend text: ' + st.legendText.slice(0, 200));
      // Every unit of every run separates a cell with data from one without.
      const step = Number(data.stepDeg) || 0.5;
      const has = new Set();
      data.cells.forEach(c => { if (c.c.some(v => v >= 1)) has.add(Math.round(c.lat / step - 0.5) + ',' + Math.round(c.lon / step - 0.5)); });
      const cell = (i, j) => has.has(i + ',' + j);
      let units = 0;
      e.parts.forEach((p) => {
        assert(p.length === 2, 'a run has 2 points');
        const [a, b] = p;
        if (Math.abs(a[0] - b[0]) < 1e-9) {            // horizontal, between rows
          const r = Math.round(a[0] / step), j0 = Math.round(Math.min(a[1], b[1]) / step), j1 = Math.round(Math.max(a[1], b[1]) / step);
          for (let j = j0; j < j1; j++, units++) assert(cell(r - 1, j) !== cell(r, j), `run at lat ${a[0]} lon ${j * step}: not a boundary`);
        } else {                                       // vertical, between columns
          assert(Math.abs(a[1] - b[1]) < 1e-9, 'run is neither horizontal nor vertical');
          const c = Math.round(a[1] / step), i0 = Math.round(Math.min(a[0], b[0]) / step), i1 = Math.round(Math.max(a[0], b[0]) / step);
          for (let i = i0; i < i1; i++, units++) assert(cell(i, c - 1) !== cell(i, c), `run at lon ${a[1]} lat ${i * step}: not a boundary`);
        }
      });
      // ... and every boundary unit is covered (count them independently).
      let want = 0;
      has.forEach((k) => {
        const [i, j] = k.split(',').map(Number);
        [[i - 1, j], [i + 1, j], [i, j - 1], [i, j + 1]].forEach(([a, b]) => { if (!cell(a, b)) want++; });
      });
      assert(units === want, `boundary units drawn=${units} want ${want}`);
    });

    // On a short viewport the expanded picker list is taller than the space
    // beside the legend, so the boxes may intersect; what must hold is that the
    // picker is on top: every sampled point of it hits the picker itself.
    await test('the expanded layers picker is on top of the legend and time slider', async () => {
      await openPicker(page);
      await sleep(300);
      const res = await page.evaluate(() => {
        const pk = document.querySelector('.leaflet-control-layers-expanded');
        if (!pk) return { err: 'picker did not expand' };
        const r = pk.getBoundingClientRect(), bad = [];
        for (let i = 1; i <= 5; i++) {
          for (let j = 1; j <= 9; j++) {
            const x = r.left + r.width * i / 6, y = r.top + r.height * j / 10;
            if (y >= window.innerHeight) continue;
            const el = document.elementFromPoint(x, y);
            if (!el || !pk.contains(el)) bad.push(`${Math.round(x)},${Math.round(y)} -> ${el ? (el.className || el.tagName) : 'nothing'}`);
          }
        }
        return { bad };
      });
      await page.mouse.move(5, 5);
      assert(!res.err, res.err);
      assert(!res.bad.length, 'picker covered at ' + res.bad.slice(0, 5).join('; '));
    });

    await test('the field image matches the data at the current forecast hour, aligned with the map', async () => {
      idx0 = await page.evaluate(() => { const s = document.querySelector('.mc-duct-time input[type=range]'); return parseInt(s.value, 10); });
      assert(idx0 >= 0 && idx0 < data.times.length, 'slider index ' + idx0);
      const f = await fieldProblems(page, data, idx0);
      assert(!f.probs.length, f.probs.slice(0, 5).join(' | '));
      assert(f.calm > 0, 'no calm cell to check for transparency');
      const panes = await page.evaluate(() => ['mcDuctField', 'mcDuctLine'].map((n) => { const p = document.querySelector('.leaflet-' + n + '-pane'); const cs = p && getComputedStyle(p); return cs ? { z: +cs.zIndex, pe: cs.pointerEvents, canvases: p.querySelectorAll('canvas').length } : null; }));
      assert(panes[0] && panes[0].z > 200 && panes[0].z < 400 && panes[0].pe === 'none', 'field pane ' + JSON.stringify(panes[0]));
      assert(panes[1] && panes[1].z > panes[0].z && panes[1].z < 400 && panes[1].pe === 'none' && panes[1].canvases === 1, 'line pane ' + JSON.stringify(panes[1]));
      assert(!(await page.evaluate(() => document.querySelectorAll('.leaflet-overlay-pane canvas').length)), 'the overlay put a canvas in the overlay pane');
    });

    await test('LoRa cells, and only they, have the dashed outline', async () => {
      const o = outlineProblems(await pageState(page), data, idx0, (await cssVars(page)).lora);
      assert(!o.probs.length, o.probs.slice(0, 5).join(' | '));
      assert(o.n > 0, 'no LoRa duct at hour ' + idx0);
    });

    await test('moving the time slider changes the field, the outlines and the label', async () => {
      const before = (await page.evaluate(inPageFieldProbe, { idx: idx0, cells: [] })).sum;
      const label0 = await page.locator('.mc-duct-time-label').first().textContent();
      const max = await page.evaluate(() => parseInt(document.querySelector('.mc-duct-time input[type=range]').max, 10));
      assert(max === data.times.length - 1, `slider max ${max}`);
      await setSlider(page, max);
      const after = await fieldProblems(page, data, max);
      assert(after.r.sum !== before, 'field image did not change');
      assert(!after.probs.length, after.probs.slice(0, 5).join(' | '));
      const o = outlineProblems(await pageState(page), data, max, (await cssVars(page)).lora);
      assert(!o.probs.length, o.probs.slice(0, 5).join(' | '));
      assert((await page.locator('.mc-duct-time-label').first().textContent()) !== label0, 'label did not change');
      assert((await pageState(page)).fields.length === 1, 'slider added a field image');
      await setSlider(page, idx0);
      const back = await fieldProblems(page, data, idx0);
      assert(!back.probs.length && back.r.sum === before, 'after moving back: ' + back.probs.slice(0, 3).join(' | '));
    });

    // A real mouse click at a free point of a cell of class wantClass at the
    // shown hour; the popup comes from the overlay's map click handler.
    async function popupFor(wantClass) {
      const cell = data.cells.find(c => c.c[idx0] === wantClass);
      assert(cell, `no class ${wantClass} cell at hour ${idx0}`);
      for (const pt of await pointsIn(page, cell, data.stepDeg, 6)) {
        await page.mouse.click(pt.x, pt.y);
        try {
          await page.waitForSelector('.leaflet-popup .mc-duct-popup', { timeout: 2500 });
          return { text: await page.locator('.leaflet-popup .mc-duct-popup').first().innerText(), cell };
        } catch (_) { /* try the next point */ }
      }
      throw new Error(`could not open the popup of cell ${cell.lat},${cell.lon} (class ${wantClass})`);
    }
    // The popup's LoRa verdict lines for cell c at hour h: one per judged frequency.
    const verdicts = (c, h) => LORA_FREQS.map(fr => `LoRa ${fmtFreq(fr)} MHz: ` + (shownFmin(c, h) > 0 && shownFmin(c, h) <= fr ? 'possibly trapped (model estimate)' : 'not trapped by this layer'));

    await test('clicking a surface-duct cell opens its popup: trapping layer, frequency estimate, LoRa possibly trapped', async () => {
      const r = await popupFor(4);
      const f = r.cell.f[idx0];
      assert(/Surface-based duct/.test(r.text), 'popup: ' + r.text);
      assert(r.text.includes('Trapping layer in the model (its levels are ~200 m apart, so thinner layers are not resolved)'), 'popup: ' + r.text);
      assert(r.text.includes(`Lowest trapped frequency: \u2248 ${sig2(f)} MHz (model estimate)`), `popup (f=${f}): ` + r.text);
      const lines = r.text.split('\n').filter(l => /^LoRa /.test(l));
      assert(JSON.stringify(lines) === JSON.stringify(verdicts(r.cell, idx0)), `popup LoRa lines (f=${f}) ${JSON.stringify(lines)} want ${JSON.stringify(verdicts(r.cell, idx0))}`);
      assert(lines.some(l => /possibly trapped/.test(l)), 'surface duct of the fake traps LoRa: ' + r.text);
      assert(!/Can carry|: yes|: no\b/.test(r.text), 'old yes/no wording: ' + r.text);
      assert(/Base: 0 m/.test(r.text), 'popup: ' + r.text);
      const n = fieldN(r.cell, idx0);
      if (n != null) assert(r.text.includes('Gradient, lowest 1.5 km: ' + (n < 0 ? '\u2212' + -n : n) + ' N/km'), 'popup gradient (n=' + n + '): ' + r.text);
    });
    await test('clicking a super-refraction cell: no trapping layer, LoRa not trapped', async () => {
      const r = await popupFor(2);
      const lines = r.text.split('\n').filter(l => /^LoRa /.test(l));
      assert(/Super-refraction/.test(r.text) && /Lowest trapped frequency: none/.test(r.text), 'popup: ' + r.text);
      assert(lines.length === LORA_FREQS.length && lines.every(l => / MHz: not trapped by this layer$/.test(l)), 'popup LoRa lines: ' + JSON.stringify(lines));
      assert(!/Trapping layer/.test(r.text), 'popup: ' + r.text);
    });
    await test('a map click in the forecast area reaches the page\'s own map click listeners; the popup follows the slider', async () => {
      const cell = data.cells.find(c => c.c[idx0] === 4);
      const pts = await pointsIn(page, cell, data.stepDeg, 1);
      assert(pts.length, 'no free point in the cell');
      await page.evaluate(() => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        window.__ductTestClicks = 0;
        window.__ductTestOn = () => { window.__ductTestClicks++; };
        m.on('click', window.__ductTestOn);
      });
      await page.mouse.click(pts[0].x, pts[0].y);
      await page.waitForSelector('.leaflet-popup .mc-duct-popup', { timeout: 2500 });
      const got = await page.evaluate(() => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        m.off('click', window.__ductTestOn);
        return window.__ductTestClicks;
      });
      assert(got === 1, 'map click listeners saw ' + got + ' clicks');
      // Another hour: the open popup shows that hour's analysis of the same cell.
      const other = cell.c.findIndex((v, h) => h !== idx0 && v >= 1 && v !== 4);
      if (other >= 0) {
        await setSlider(page, other);
        const t = await page.locator('.leaflet-popup .mc-duct-popup').first().innerText().catch(() => '');
        assert(t && !/Surface-based duct/.test(t), `popup at hour ${other} (class ${cell.c[other]}): ${t}`);
        await setSlider(page, idx0);
      }
      await page.evaluate(() => window.__mcHarness.maps.forEach(m => m.closePopup && m.closePopup()));
    });

    await test('theme toggle redraws the field and outlines from the CSS variables', async () => {
      await page.evaluate(() => window.__mcHarness.maps.forEach(m => m.closePopup && m.closePopup()));
      const light = await cssVars(page);
      const before = (await page.evaluate(inPageFieldProbe, { idx: idx0, cells: [] })).sum;
      const cells0 = (await pageState(page)).cells;
      await MH.setTheme(page, 'dark');
      await page.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'dark');
      const dark = await cssVars(page);
      assert(dark.s4 && dark.s4 !== light.s4 && dark.lora !== light.lora, `--mc-duct-* did not change with the theme (${light.s4}/${dark.s4}, ${light.lora}/${dark.lora})`);
      let f = null;
      for (let i = 0; i < 30; i++) {
        f = await fieldProblems(page, data, idx0);
        if (!f.probs.length && f.r.sum !== before) break;
        await sleep(200);
      }
      assert(f.r.sum !== before, 'field image unchanged after the theme change');
      assert(!f.probs.length, 'after theme change: ' + f.probs.slice(0, 4).join(' | '));
      const st = await pageState(page);
      const o = outlineProblems(st, data, idx0, dark.lora);
      assert(!o.probs.length, 'dark outlines: ' + o.probs.slice(0, 3).join(' | '));
      assert(st.cells === cells0 && st.fields.length === 1, `cells ${cells0}->${st.cells}, fields ${st.fields.length}`);
      const ov = await page.evaluate(inPageOverlaps);
      assert(!ov.hits.length, 'dark: ' + ov.hits.join('; '));
      await MH.setTheme(page, 'light');
    });

    // Leaflet 1.9.4: a map move redraws the canvas at once and orphans the
    // queued frame; switching off before that frame used to throw from the
    // orphan ("reading 'clearRect'", stress gate seed 303, scenario g).
    await test('switching off right after a canvas move redraw throws nothing', async () => {
      const before = page.__errors.length;
      const queued = await page.evaluate(() => {
        let line = null;
        window.__mcHarness.maps.forEach(m => { if (m._loaded) m.eachLayer(l => { if (!line && l._mcEdge && l._renderer) line = l; }); });
        if (!line) return 'no edge line';
        const r = line._renderer;
        line.setStyle({ opacity: 0.5 });       // queues a frame redraw
        if (!r._redrawRequest) return 'no redraw queued';
        r._updatePaths();                      // what every moveend does
        line.setStyle({ opacity: 0.4 });       // marks an area to redraw
        window.MC_setTileOverlay('ducting', false);
        return 'ok';
      });
      assert(queued === 'ok', queued);
      await sleep(300);
      const errs = page.__errors.slice(before);
      assert(!errs.length, errs.join(' | '));
      await page.evaluate(() => window.MC_setTileOverlay('ducting', true));
      await waitCells(page, 1);
    });

    await test('disabling the overlay removes legend, time slider and cells', async () => {
      await setOverlayUi(page, false);
      await page.waitForFunction(() => !document.querySelector('.mc-duct-legend') && !document.querySelector('.mc-duct-time'), null, { timeout: 5000 });
      const st = await pageState(page);
      assert(!st.cells && !st.lora.length && !st.edges.length, `${st.cells} cells, ${st.lora.length} outlines, ${st.edges.length} edges still drawn`);
      assert(!(await page.evaluate(() => document.querySelectorAll('.leaflet-mcDuctLine-pane canvas').length)), 'line canvas left in the DOM');
      assert(!st.fields.length, 'field image left on the map');
      assert(!(await page.evaluate(() => document.querySelectorAll('.mc-duct-field').length)), 'field canvas left in the DOM');
      assert(st.legends === 0 && st.times === 0, 'legend/time boxes left');
      assert(await page.evaluate(() => JSON.parse(localStorage.getItem('mc-tile-overlays') || '[]').indexOf('ducting') < 0), 'preference still has ducting');
    });

    await test('re-enabling after a disable draws one legend and one slider, no duplicates', async () => {
      await setOverlayUi(page, true);
      await waitCells(page, 1);
      await setOverlayUi(page, false);
      await setOverlayUi(page, true);
      await waitCells(page, data.cells.length);
      await sleep(300);
      const st = await pageState(page);
      assert(st.legends === 1 && st.times === 1 && st.cells === data.cells.length && st.edges.length === 1 && st.lora.length <= 1, `legends=${st.legends} times=${st.times} cells=${st.cells} edges=${st.edges.length} outlines=${st.lora.length}`);
      assert(st.fields.length === 1 && await page.evaluate(() => document.querySelectorAll('.mc-duct-field').length) === 1, 'field images: ' + st.fields.length);
      await setOverlayUi(page, false);
    });

    await test('map page has no page errors', async () => { assert(!page.__errors.length, page.__errors.slice(0, 3).join(' | ')); });

    // ── Live ──
    let livePage = null;
    await test('live page: entry present, enabling draws cells and an unobstructed legend', async () => {
      const lp = await track({ theme: 'light', overlays: ['ducting'] });
      await gotoRoute(lp.page, '#/live');
      await lp.page.waitForSelector('label[data-tile-overlay-id="ducting"]', { state: 'attached', timeout: 25000 });
      await waitCells(lp.page, data.cells.length);
      await lp.page.waitForSelector('.mc-duct-legend', { state: 'visible' });
      await lp.page.mouse.move(5, 5);
      const st = await pageState(lp.page);
      assert(st.legends === 1 && st.times === 1, `legends=${st.legends} times=${st.times}`);
      assert(st.fields.length === 1 && st.fields[0].inDom, 'live field images: ' + st.fields.length);
      assert(!st.interactive.length, 'interactive overlay layers: ' + st.interactive.join(','));
      const ov = await lp.page.evaluate(inPageOverlaps);
      assert(!ov.hits.length, ov.hits.join('; '));
      assert(!lp.page.__errors.length, lp.page.__errors.slice(0, 3).join(' | '));
      livePage = lp.page;
    });

    // Live draws its clickable packet paths on its own canvas (preferCanvas)
    // in the overlay pane. The overlay must not put a canvas above it, and a
    // click inside the forecast area must reach Live's map and its paths.
    await test('live page: a click inside the forecast area reaches Live\'s map and canvas paths; no ducting popup over them', async () => {
      assert(livePage, 'live page did not load');
      const lpg = livePage;
      const cell = data.cells.find(c => c.c.some(v => v >= 1));
      const pts = await pointsIn(lpg, cell, data.stepDeg, 3);
      assert(pts.length, 'no free point in a forecast cell on Live');
      const pt = pts[0];
      // Nothing of the overlay is hit-tested at the click point.
      const top = await lpg.evaluate((p) => {
        const el = document.elementFromPoint(p.x, p.y);
        return { inDuct: !!(el && el.closest('.leaflet-mcDuctLine-pane, .leaflet-mcDuctField-pane')), tag: el && el.tagName };
      }, pt);
      assert(!top.inDuct, 'a ducting pane is hit-tested at the click point: ' + JSON.stringify(top));
      // A clickable path like Live's (interactive, transparent, on the map's
      // canvas renderer, a popup on click) through the click point, plus a map listener.
      const added = await lpg.evaluate((p) => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        const r = m.getContainer().getBoundingClientRect();
        const a = m.containerPointToLatLng([p.x - r.left - 60, p.y - r.top]), b = m.containerPointToLatLng([p.x - r.left + 60, p.y - r.top]);
        window.__ductLive = { path: 0, map: 0 };
        const poly = L.polyline([a, b], { weight: 12, opacity: 0, interactive: true }).addTo(m);
        poly.on('click', (e) => { window.__ductLive.path++; L.popup({ className: 'path-info-popup' }).setLatLng(e.latlng).setContent('<div class="duct-test-path">path</div>').openOn(m); });
        window.__ductLive.poly = poly;
        window.__ductLive.onMap = () => { window.__ductLive.map++; };
        m.on('click', window.__ductLive.onMap);
        return { canvas: !!(poly._renderer && poly._renderer._container && poly._renderer._container.tagName === 'CANVAS'), pane: poly._renderer && poly._renderer.options.pane };
      }, pt);
      assert(added.canvas && added.pane === 'overlayPane', 'test path not on Live\'s overlay-pane canvas: ' + JSON.stringify(added));
      await lpg.mouse.move(pt.x, pt.y - 2);
      await lpg.mouse.move(pt.x, pt.y);
      await sleep(100);
      await lpg.mouse.click(pt.x, pt.y);
      await sleep(400);
      const res = await lpg.evaluate(() => {
        const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
        m.off('click', window.__ductLive.onMap);
        const out = { path: window.__ductLive.path, map: window.__ductLive.map, pathPopup: !!document.querySelector('.leaflet-popup .duct-test-path'), ductPopup: !!document.querySelector('.leaflet-popup .mc-duct-popup') };
        window.__ductLive.poly.remove();
        m.closePopup();
        return out;
      });
      assert(res.path === 1, 'Live\'s canvas path did not get the click: ' + JSON.stringify(res));
      assert(res.map === 1, 'the map click did not propagate past the overlay: ' + JSON.stringify(res));
      assert(res.pathPopup && !res.ductPopup, 'the path\'s popup must stay, no ducting popup over it: ' + JSON.stringify(res));
      // A click on free map (no path) opens the ducting popup there.
      await lpg.mouse.move(5, 5);
      await lpg.mouse.click(pt.x, pt.y);
      await lpg.waitForSelector('.leaflet-popup .mc-duct-popup', { timeout: 2500 });
      await lpg.evaluate(() => window.__mcHarness.maps.forEach(m => m.closePopup && m.closePopup()));
      assert(!lpg.__errors.length, lpg.__errors.slice(0, 3).join(' | '));
    });

    // ── Planner (opt-in) ──
    if (cfg.planner) {
      await test('planner page: entry present', async () => {
        const pp = await track({ theme: 'light' });
        await gotoRoute(pp.page, '#/tools/planner');
        await pp.page.waitForSelector('label[data-tile-overlay-id="ducting"]', { state: 'attached', timeout: 25000 });
        assert(!pp.page.__errors.length, pp.page.__errors.slice(0, 3).join(' | '));
      });
      // Planner places a site on a map click (map.on('click') in planner.js).
      // Interactive cells with popups used to stop that click everywhere in
      // the forecast area.
      await test('planner page: with the overlay on, a click inside the forecast area places a site (and opens no ducting popup)', async () => {
        const pp = await track({ theme: 'light', overlays: ['ducting'] });
        await gotoRoute(pp.page, '#/tools/planner?mode=coverage');
        await waitCells(pp.page, data.cells.length);
        await pp.page.waitForSelector('.mc-duct-legend', { state: 'visible' });
        await pp.page.mouse.move(5, 5);
        assert(!(await pp.page.locator('path.mc-pl-cov-site').count()), 'a site before any click');
        const idx = await pp.page.evaluate(() => parseInt(document.querySelector('.mc-duct-time input[type=range]').value, 10));
        // A cell with data at the shown hour, so its popup would open if the click went to the overlay.
        const cell = data.cells.find(c => c.c[idx] >= 3) || data.cells.find(c => c.c[idx] >= 1);
        const pts = await pointsIn(pp.page, cell, data.stepDeg, 4);
        assert(pts.length, 'no free point in the cell');
        await pp.page.mouse.click(pts[0].x, pts[0].y);
        await pp.page.waitForSelector('path.mc-pl-cov-site', { timeout: 5000 }).catch(() => {});
        await sleep(300);
        const res = await pp.page.evaluate(() => ({ site: document.querySelectorAll('path.mc-pl-cov-site').length, duct: !!document.querySelector('.leaflet-popup .mc-duct-popup') }));
        assert(res.site === 1, `no site placed by a click at ${cell.lat},${cell.lon} (overlay on): ` + JSON.stringify(res));
        assert(!res.duct, 'the click placed a site and also opened the ducting popup over it');
        const st = await pageState(pp.page);
        assert(!st.interactive.length, 'interactive overlay layers: ' + st.interactive.join(','));
        assert(!pp.page.__errors.length, pp.page.__errors.slice(0, 3).join(' | '));
      });
    } else {
      skip('planner page: entry present', 'planner disabled on this server');
      skip('planner page: a click inside the forecast area places a site', 'planner disabled on this server');
    }

    // ── Config / API states ──
    await test('without ducting in the client config there is no overlay entry', async () => {
      const np = await track({ theme: 'light', overlays: ['ducting'], stripDucting: true });
      await gotoRoute(np.page, '#/map');
      await openPicker(np.page);
      await np.page.waitForSelector('.leaflet-control-layers-overlays', { state: 'attached' });
      assert((await np.page.locator('label[data-tile-overlay-id="ducting"]').count()) === 0, 'entry present');
      await sleep(400);
      const st = await pageState(np.page);
      assert(!st.cells && !st.fields.length && !st.legends && !st.times, 'overlay drawn although the server has none');
      assert(!np.page.__errors.length, np.page.__errors.slice(0, 3).join(' | '));
    });

    for (const [name, status, body, want] of [
      ['pending', 200, { status: 'pending', times: [], cells: [] }, /being prepared/],
      ['error', 200, { status: 'error', error: 'weather data unavailable', times: [], cells: [] }, /unavailable/],
      ['HTTP 500', 500, { error: 'x' }, /unavailable/],
    ]) {
      await test(`legend explains a ${name} /api/ducting answer, with no cells`, async () => {
        const sp = await track({ theme: 'light', overlays: ['ducting'] });
        await sp.page.route('**/api/ducting', (route) => route.fulfill({ status, json: body }));
        await gotoRoute(sp.page, '#/map');
        await sp.page.waitForSelector('.mc-duct-legend', { state: 'visible', timeout: 25000 });
        await sp.page.waitForFunction((src) => new RegExp(src).test((document.querySelector('.mc-duct-legend') || {}).textContent || ''), want.source, { timeout: 10000 });
        const st = await pageState(sp.page);
        assert(!st.cells && !st.lora.length && !st.fields.length, 'cells or field drawn');
        const errs = sp.page.__errors.filter(e => !/Failed to load resource/.test(e));
        assert(!errs.length, errs.slice(0, 3).join(' | '));
      });
    }

    // ── The fake itself ──
    await test('fake: error500, ratelimit, nulls, pending and validation scenarios', async () => {
      const f = await FAKE.start({ port: 0, scenario: 'error500' });
      try {
        const q = (n) => `${f.url}/v1/forecast?latitude=${Array(n).fill('51.25').join(',')}&longitude=${Array.from({ length: n }, (_, i) => (0.25 + i * 0.5).toFixed(2)).join(',')}&hourly=${FAKE.hourlyVariables().join(',')}&models=gfs_global&forecast_hours=3&timeformat=unixtime&timezone=GMT`;
        assert((await fetch(q(1))).status === 500, 'error500');
        f.setScenario('ratelimit');
        assert((await fetch(q(1))).status === 429, 'ratelimit');
        f.setScenario('nulls');
        const j = await (await fetch(q(8))).json();
        assert(Array.isArray(j) && j.length === 8 && j[0].hourly.time.length === 3, 'nulls shape');
        assert(j.some(l => l.hourly.temperature_900hPa.every(v => v === null)), 'nulls: no location without levels');
        assert(j.some(l => l.hourly.temperature_1000hPa.every(v => v !== null) && l.hourly.temperature_950hPa.every(v => v === null)), 'nulls: no location with single missing levels');
        f.setScenario('mixed');
        const one = await (await fetch(q(1))).json();
        assert(!Array.isArray(one) && one.elevation > 0 && one.hourly.surface_pressure.length === 3, 'one location is an object');
        assert((await fetch(q(51))).status === 400, '51 locations accepted');
        assert((await fetch(q(1).replace('gfs_global', 'icon_global'))).status === 400, 'wrong model accepted');
        assert((await fetch(q(1).replace('timeformat=unixtime', 'timeformat=iso8601'))).status === 400, 'wrong timeformat accepted');
        assert((await fetch(q(1).replace('temperature_850hPa', 'x'))).status === 400, 'missing variable accepted');
        assert(f.violations.length === 4, 'violations ' + f.violations.length);
        f.setScenario('pending');
        const ctl = new AbortController();
        let answered = false;
        const pend = fetch(q(1), { signal: ctl.signal }).then(() => { answered = true; }, () => {});
        await sleep(300);
        assert(!answered, 'pending answered');
        ctl.abort();
        await pend;
      } finally { await f.close(); }
    });
  } finally {
    for (const p of pages) await p.context.close().catch(() => {});
    await browser.close().catch(() => {});
    await fake.close().catch(() => {});
  }
  console.log(`\n${R.summary()}`);
  process.exit(R.failed ? 1 : 0);
}

run().catch((e) => { console.error(e); process.exit(1); });
