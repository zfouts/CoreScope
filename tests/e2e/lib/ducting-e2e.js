/**
 * Shared helpers of the tropospheric-ducting overlay E2E suites
 * (test-map-ducting-e2e.js, Open-Meteo source; test-map-ducting-grid-e2e.js,
 * stored global grid): the pass/fail runner, page setup (collectors, tile
 * stub, storage), and in-page probes of what the overlay draws. Pages and
 * probes work on whatever the overlay holds, so both sources share them.
 */
'use strict';
const MH = require('./map-harness');

// The LoRa frequencies the overlay judges (map-ducting.js loraFreqs): the
// site's los.frequencyMhz from /api/config/client when it is 100..6000 MHz,
// else 868 and 915 MHz.
const loraFreqsOf = (cfg) => { const f = Number(cfg && cfg.los && cfg.los.frequencyMhz); return (f >= 100 && f <= 6000) ? [f] : [868, 915]; };
// f_min as the popup shows it (2 significant figures); verdicts use this value.
const sig2 = (v) => Number(Number(v).toPrecision(2));
const shownFmin = (c, h) => (c.c[h] >= 3 && c.f[h] > 0) ? sig2(c.f[h]) : 0;
const fmtFreq = (f) => String(Math.round(f * 1000) / 1000);
// The field's colour bands start at these values of n, the mean dN/dh over
// the lowest 1.5 km in N/km (map-ducting.js STOP_N).
const STOP_N = [-44, -48, -54, -58, -62, -70, -80, -90];
// The field value of a cell at hour h: n, or null for no data (0 or class 0).
const fieldN = (c, h) => (c.c[h] >= 1 && Array.isArray(c.n) && c.n[h]) ? c.n[h] : null;
// A stop between a and b (within 0.5 N/km): the two may legitimately sit in
// different bands, so their colours are not compared.
const stopBetween = (a, b) => STOP_N.some(s => s >= Math.min(a, b) - 0.5 && s <= Math.max(a, b) + 0.5);

function assert(cond, msg) { if (!cond) throw new Error(msg || 'Assertion failed'); }
const sleep = (ms) => new Promise(r => setTimeout(r, ms));

// Pass/fail counting. In CI (GitHub Actions sets CI=true) the servers are set
// up with everything the checks need, so a skip there means a broken setup:
// it counts as a failure instead.
function createRunner() {
  const r = { passed: 0, failed: 0, skipped: 0 };
  r.test = async (name, fn) => {
    try { await fn(); r.passed++; console.log(`  \u2705 ${name}`); }
    catch (e) { r.failed++; console.log(`  \u274c ${name}: ${e.message}`); }
  };
  r.skip = (name, why) => {
    if (process.env.CI === 'true') { r.failed++; console.log(`  \u274c ${name}: skipped in CI (${why})`); return; }
    r.skipped++; console.log(`  \u23ed  ${name}: ${why}`);
  };
  r.summary = () => `${r.passed} passed, ${r.failed} failed, ${r.skipped} skipped`;
  return r;
}

// ── Page plumbing ───────────────────────────────────────────────────────────
const IGNORED_CONSOLE = [/net::ERR_BLOCKED_BY_CLIENT/];

async function newPage(browser, base, opts) {
  opts = opts || {};
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
  const page = await context.newPage();
  page.setDefaultTimeout(20000);
  page.__errors = [];
  page.on('pageerror', (e) => page.__errors.push('pageerror: ' + (e && (e.stack || e.message) || e)));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    const t = m.text();
    if (IGNORED_CONSOLE.some(re => re.test(t))) return;
    const loc = m.location && m.location();
    // Status noise from unrelated /api/ probes is not the overlay's business,
    // but a failing /api/ducting is.
    if (/^Failed to load resource: the server responded/.test(t) && loc && /\/api\//.test(loc.url || '') && !/\/api\/ducting/.test(loc.url)) return;
    page.__errors.push('console.error: ' + t);
  });
  await MH.installCollectors(page);
  await MH.installTileStub(page, { mode: 'stub', baseUrl: base });
  await MH.presetStorage(page, { theme: opts.theme || 'light', provider: 'auto', overlays: opts.overlays || [] });
  if (opts.stripDucting || opts.noPlanner === false) {
    await page.route('**/api/config/client', async (route) => {
      const res = await route.fetch();
      const json = await res.json();
      if (opts.stripDucting) delete json.ducting;
      await route.fulfill({ response: res, json });
    });
  }
  return { context, page };
}

async function gotoRoute(page, base, hash, waitControl) {
  await page.goto(`${base}/${hash}`, { waitUntil: 'domcontentloaded' });
  if (waitControl !== false) await page.waitForSelector('.leaflet-control-layers', { state: 'attached', timeout: 25000 });
}
async function openPicker(page) { await page.locator('.leaflet-control-layers').first().hover(); }
// Ticks the picker's own checkbox (its click handler, as Leaflet wires it). A
// real mouse click is not used: the expanded picker can sit under the legend,
// which has its own test ('legend does not cover the expanded picker').
async function setOverlayUi(page, on) {
  const inp = page.locator('label[data-tile-overlay-id="ducting"] input').first();
  await inp.waitFor({ state: 'attached' });
  if ((await inp.isChecked()) !== on) await inp.evaluate((el) => el.click());
}

// What the page shows right now, for every live map: the cells the ducting
// layers hold, the LoRa outline layers (cells, style), edge lines, field
// images, any interactive layer of the overlay (there must be none), and
// the panel with its legend/time sections.
function inPageState() {
  const maps = ((window.__mcHarness && window.__mcHarness.maps) || [])
    .filter(m => m && m._container && m._container.isConnected && m._loaded);
  let cells = 0;
  const lora = [], fields = [], edges = [], interactive = [];
  maps.forEach((m, mi) => {
    m.eachLayer((l) => {
      if (l._mcDuct) cells += (l._cells || []).length;
      if (l._mcEdge) {
        const parts = l.getLatLngs().map(p => p.map(ll => [ll.lat, ll.lng]));
        edges.push({ map: mi, parts, color: l.options.color, dash: l.options.dashArray, weight: l.options.weight, interactive: l.options.interactive });
      }
      if (l._mcLora) lora.push({ map: mi, cells: l._mcCells.map(c => [c.lat, c.lon]), color: l.options.color, dash: l.options.dashArray, fill: l.options.fill, interactive: l.options.interactive, pane: l._renderer && l._renderer.options.pane });
      if ((l._mcEdge || l._mcLora || l._mcCell) && l.options.interactive !== false) interactive.push(l._mcEdge ? 'edge' : l._mcLora ? 'lora' : 'cell');
      if (l._mcField) fields.push({ map: mi, inDom: !!(l._image && l._image.isConnected), pane: l.options.pane });
    });
  });
  const vis = (el) => { const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && cs.display !== 'none'; };
  const q = (sel) => Array.prototype.slice.call(document.querySelectorAll(sel));
  return {
    maps: maps.length,
    cells,
    lora,
    interactive,
    fields,
    edges,
    panels: q('.mc-duct-panel').length,
    legends: q('.mc-duct-legend').length,
    times: q('.mc-duct-time').length,
    legendVisible: q('.mc-duct-legend').some(vis),
    timeVisible: q('.mc-duct-time').some(vis),
    legendText: (q('.mc-duct-legend')[0] || {}).textContent || '',
  };
}
const pageState = (page) => page.evaluate(inPageState);
// Waits until the ducting layers of the live maps hold at least min cells.
async function waitCells(page, min) {
  await page.waitForFunction((n) => {
    const maps = ((window.__mcHarness && window.__mcHarness.maps) || []).filter(m => m && m._container && m._container.isConnected && m._loaded);
    let c = 0; maps.forEach(m => m.eachLayer(l => { if (l._mcDuct) c += (l._cells || []).length; }));
    return c >= n;
  }, min || 1, { timeout: 25000, polling: 100 });
}

// Bounding boxes of the controls that must not overlap the ducting panel.
function inPageOverlaps() {
  const boxes = (el) => { const r = el.getBoundingClientRect(); return { l: r.left, t: r.top, r: r.right, b: r.bottom, w: r.width, h: r.height }; };
  const visible = (el) => { const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && cs.display !== 'none'; };
  const mine = Array.prototype.slice.call(document.querySelectorAll('.mc-duct-panel')).filter(visible);
  const others = Array.prototype.slice.call(document.querySelectorAll('.leaflet-control, #mapControls, #mapControlsToggle'))
    .filter(el => visible(el) && !el.matches('.mc-duct-panel') && !mine.some(m => m.contains(el) || el.contains(m)));
  const hits = [];
  mine.forEach((m, i) => {
    const a = boxes(m);
    const name = (e) => e.id ? '#' + e.id : '.' + String(e.className).split(/\s+/).slice(0, 3).join('.');
    others.forEach((o) => {
      const b = boxes(o);
      const ix = Math.min(a.r, b.r) - Math.max(a.l, b.l), iy = Math.min(a.b, b.b) - Math.max(a.t, b.t);
      if (ix > 1 && iy > 1) hits.push(`${name(m)} overlaps ${name(o)} by ${Math.round(ix)}x${Math.round(iy)}px`);
    });
    mine.forEach((m2, j) => {
      if (j <= i) return;
      const b = boxes(m2);
      const ix = Math.min(a.r, b.r) - Math.max(a.l, b.l), iy = Math.min(a.b, b.b) - Math.max(a.t, b.t);
      if (ix > 1 && iy > 1) hits.push(`${name(m)} overlaps ${name(m2)} by ${Math.round(ix)}x${Math.round(iy)}px`);
    });
    const vw = document.documentElement.clientWidth, vh = document.documentElement.clientHeight;
    if (a.l < -1 || a.t < -1 || a.r > vw + 1 || a.b > vh + 1) hits.push(`${name(m)} outside the viewport (${Math.round(a.l)},${Math.round(a.t)},${Math.round(a.r)},${Math.round(a.b)} in ${vw}x${vh})`);
  });
  // The panel's own sections: the legend body and the timeline inside it.
  const inside = mine.map(m => ['.mc-duct-legend', '.mc-duct-time'].filter(sel => { const e = m.querySelector(sel); return e && visible(e); }).length);
  return { hits, mine: mine.length, inside };
}

function cssVars(page) {
  return page.evaluate(() => {
    const cs = getComputedStyle(document.documentElement);
    const o = { lora: cs.getPropertyValue('--mc-duct-lora').trim(), alpha: parseFloat(cs.getPropertyValue('--mc-duct-alpha')) };
    for (let k = 1; k <= 8; k++) o['s' + k] = cs.getPropertyValue('--mc-duct-s' + k).trim();
    return o;
  });
}

/**
 * Probes the field image of the first live map at each cell centre: the
 * canvas pixel under the centre, found with Leaflet's own projection (pixel
 * points of the image corners and of the centre), so a field drawn with the
 * wrong projection does not line up. For each probe also returns what the
 * field should be at that pixel's centre (MC_ductSample at Leaflet's latlng
 * for it) and the colour of the cell's own n. Runs in the page.
 */
function inPageFieldProbe(arg) {
  const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
  let field = null, layer = null;
  m.eachLayer((l) => { if (l._mcField) field = l; if (l._drawField) layer = l; });
  if (!field || !layer) return { err: 'no field image or ducting layer' };
  const cv = field._image;
  const ctx = cv.getContext('2d');
  const b = field.getBounds();
  const z = m.getZoom();   // project/unproject: unrounded, unlike latLngToLayerPoint
  const nw = m.project(b.getNorthWest(), z), se = m.project(b.getSouthEast(), z);
  const stops = window.MC_ductFieldStops();
  const alpha = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--mc-duct-alpha'));
  const grid = layer._grid, vals = window.MC_ductGridValues(grid, layer._data.cells, arg.idx);
  return {
    w: cv.width, h: cv.height, alpha,
    sum: (() => { const d = ctx.getImageData(0, 0, cv.width, cv.height).data; let h = 0; for (let i = 0; i < d.length; i += 4) h = (h * 31 + d[i] * 7 + d[i + 1] * 3 + d[i + 2] + d[i + 3] * 11) % 1000000007; return h; })(),
    probes: arg.cells.map((c) => {
      const p = m.project([c.lat, c.lon], z);
      const x = Math.floor((p.x - nw.x) / (se.x - nw.x) * cv.width), y = Math.floor((p.y - nw.y) / (se.y - nw.y) * cv.height);
      const px = Array.from(ctx.getImageData(x, y, 1, 1).data);
      const ll = m.unproject([nw.x + (x + 0.5) / cv.width * (se.x - nw.x), nw.y + (y + 0.5) / cv.height * (se.y - nw.y)], z);
      const smp = window.MC_ductSample(grid, vals, ll.lat, ll.lng);
      const exp = window.MC_ductFieldColor(smp.g, stops);
      return { lat: c.lat, lon: c.lon, n: c.n, px, smp, exp: [exp[0], exp[1], exp[2], Math.round(exp[3] * alpha * smp.a * 255)], own: window.MC_ductFieldColor(c.n, stops) };
    }),
  };
}

// Lattice neighbours (N, S, E, W) of a cell, from the data.
function neighbours(data, c) {
  const st = data.stepDeg, key = (la, lo) => `${la.toFixed(4)},${lo.toFixed(4)}`;
  const byKey = new Map(data.cells.map(x => [key(x.lat, x.lon), x]));
  return [[st, 0], [-st, 0], [0, st], [0, -st]].map(([a, b]) => byKey.get(key(c.lat + a, c.lon + b))).filter(Boolean);
}

// 1 for a cell whose 8 neighbours are all in the forecast area (data at some
// hour), 0.5 on the area's outer ring: the field's coverage at a cell centre.
// 0 (not checked) when a neighbour in the area has no n at hour idx.
function ringFactor(data, c, idx) {
  const st = data.stepDeg, key = (la, lo) => `${la.toFixed(4)},${lo.toFixed(4)}`;
  const byKey = new Map(data.cells.filter(x => x.c.some(v => v >= 1)).map(x => [key(x.lat, x.lon), x]));
  let f = 1;
  for (const a of [-st, 0, st]) for (const b of [-st, 0, st]) {
    const x = byKey.get(key(c.lat + a, c.lon + b));
    if (!x) f = Math.min(f, 0.5);
    else if (fieldN(x, idx) == null) return 0;
  }
  return f;
}

/**
 * Checks the field at hour idx: coloured cells (n <= -44) coloured like their
 * n, a normal cell (n > -44, with only normal or missing neighbours) transparent, and every probed
 * pixel equal to the field sampled at Leaflet's position of that pixel. The
 * colours are flat bands, so a probe within 0.5 N/km of a band boundary (its
 * pixel's float rounding may land on either side) has its colour checks
 * skipped; the alpha check always applies.
 */
async function fieldProblems(page, data, idx) {
  // Coloured cells, clear of the first stop (a cell at exactly -44 samples
  // either side of it a fraction of a pixel away); spread over the bands.
  const ducts = data.cells.filter(c => fieldN(c, idx) != null && fieldN(c, idx) <= STOP_N[0] - 1)
    .sort((a, b) => a.n[idx] - b.n[idx]).filter((c, i, arr) => i % Math.max(1, Math.floor(arr.length / 12)) === 0).slice(0, 12);
  // Normal: n above -44 with a margin, and no lattice neighbour so much
  // stronger that the slope carries the value past -44 within the pixel or
  // two between the probe and the cell centre.
  const calm = data.cells.filter((c) => {
    const v = fieldN(c, idx);
    return v != null && v > STOP_N[0] + 1 && neighbours(data, c).every(nb => { const w = fieldN(nb, idx); return w == null || w > v - 20; });
  }).slice(0, 6);
  const cells = ducts.concat(calm).map(c => ({ lat: c.lat, lon: c.lon, n: c.n[idx] }));
  const r = await page.evaluate(inPageFieldProbe, { idx, cells });
  if (r.err) return { probs: [r.err], r };
  const probs = [];
  if (!ducts.length) probs.push(`no cell with n <= ${STOP_N[0]} at hour ${idx}`);
  r.probes.forEach((p, i) => {
    const where = `${p.lat},${p.lon} n=${p.n}`;
    // Colour only where opaque enough: the canvas stores premultiplied alpha.
    const edge = stopBetween(p.smp.g, p.smp.g);
    const bad = Math.abs(p.px[3] - p.exp[3]) > 3 || (!edge && p.exp[3] >= 64 && p.px.slice(0, 3).some((v, k) => Math.abs(v - p.exp[k]) > 4));
    if (bad) probs.push(`${where}: pixel ${p.px} but the field there is ${p.exp}`);
    if (i < ducts.length) {
      // Full opacity inside the forecast area, half on its outer ring (the wider edge fade);
      // a neighbour without n at this hour fades it too, so only data-rich cells are probed for alpha.
      if (p.px[3] < 0.8 * r.alpha * 255 * ringFactor(data, ducts[i], idx)) probs.push(`${where}: coloured centre alpha ${p.px[3]}`);
      // The pixel centre is a fraction of a cell from the cell centre, so it
      // has the cell's own band colour unless a band boundary lies between.
      if (!stopBetween(p.n, p.smp.g) && p.px.slice(0, 3).some((v, k) => Math.abs(v - p.own[k]) > 4)) probs.push(`${where}: pixel ${p.px} not its own band colour ${p.own} (sampled n ${p.smp.g})`);
    } else if (p.px[3] !== 0) probs.push(`${where}: normal cell centre not transparent (${p.px})`);
  });
  return { probs, r, ducts: ducts.length, calm: calm.length };
}

// Outline problems: one dashed, unfilled, non-interactive --mc-duct-lora
// outline layer exactly on the cells where the model shows a duct that may
// carry LoRa at hour idx (0 < shown f_min <= the highest judged frequency,
// freqs ascending as loraFreqsOf gives them).
function outlineProblems(st, data, idx, lora, freqs) {
  const probs = [];
  const want = new Set(data.cells.filter(c => { const v = shownFmin(c, idx); return v > 0 && v <= freqs[freqs.length - 1]; }).map(c => `${c.lat},${c.lon}`));
  if (st.lora.length > 1) probs.push(`${st.lora.length} LoRa outline layers`);
  const l = st.lora[0];
  if (!want.size) { if (l) probs.push(`outline drawn on ${l.cells.length} cells, none wanted`); return { probs, n: 0 }; }
  if (!l) { probs.push(`no outline layer, ${want.size} LoRa cells`); return { probs, n: want.size }; }
  if (!(l.dash === '4 3' && l.fill === false && l.interactive === false && String(l.color).toLowerCase() === String(lora).toLowerCase())) probs.push('outline style ' + JSON.stringify([l.color, l.dash, l.fill, l.interactive]));
  if (l.pane !== 'mcDuctLinePane') probs.push('outline renderer pane ' + l.pane);
  const got = new Set(l.cells.map(([a, b]) => `${a},${b}`));
  want.forEach(k => { if (!got.has(k)) probs.push(`${k}: LoRa duct without the dashed outline`); });
  got.forEach((k) => {
    if (want.has(k)) return;
    const c = data.cells.find(x => `${x.lat},${x.lon}` === k);
    probs.push(`${k}: class ${c && c.c[idx]} f=${c && c.f[idx]} has an outline`);
  });
  return { probs, n: want.size };
}

// The first live map's container point of (lat, lon) in page coordinates,
// and whether a marker, interactive path, control or popup covers it.
function inPagePoint(a) {
  const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
  const p = m.latLngToContainerPoint([a.lat, a.lon]);
  const r = m.getContainer().getBoundingClientRect();
  const x = r.left + p.x, y = r.top + p.y;
  const el = document.elementFromPoint(x, y);
  const blocked = !el || !!el.closest('.leaflet-marker-icon, .leaflet-interactive, .leaflet-control, .leaflet-popup, #mapControls, .mc-duct-panel, .leaflet-marker-pane, .leaflet-tooltip')
    || x < r.left + 40 || y < r.top + 40 || x > r.right - 40 || y > r.bottom - 40;
  return { x, y, blocked };
}

// Centres the first live map on cell c and returns up to `max` clickable
// points inside the cell (a node marker or control may sit on the centre).
async function pointsIn(page, c, step, max) {
  await page.evaluate((a) => {
    const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
    m.closePopup();
    m.setView([a.lat, a.lon], 9, { animate: false });
  }, c);
  await sleep(400);
  const d = step * 0.3, out = [];
  for (const [dy, dx] of [[0, 0], [d, d], [-d, d], [d, -d], [-d, -d], [0, d], [d, 0], [-d, 0], [0, -d]]) {
    const pt = await page.evaluate(inPagePoint, { lat: c.lat + dy, lon: c.lon + dx });
    if (!pt.blocked) out.push(pt);
    if (out.length >= max) break;
  }
  return out;
}

// The timeline (a role=slider strip): the shown index, and moving it with
// the keyboard as a user does (ArrowLeft / ArrowRight from where it is;
// Home / End when that is shorter).
const timeIdx = (page) => page.evaluate(() => parseInt(document.querySelector('.mc-duct-timeline').getAttribute('aria-valuenow'), 10));
async function setTime(page, idx) {
  const tl = page.locator('.mc-duct-timeline').first();
  await tl.focus();
  let cur = await timeIdx(page);
  const max = await page.evaluate(() => parseInt(document.querySelector('.mc-duct-timeline').getAttribute('aria-valuemax'), 10));
  if (idx < Math.abs(idx - cur)) { await page.keyboard.press('Home'); cur = 0; }
  else if (max - idx < Math.abs(idx - cur)) { await page.keyboard.press('End'); cur = max; }
  for (let k = 0; k < Math.abs(idx - cur); k++) await page.keyboard.press(idx > cur ? 'ArrowRight' : 'ArrowLeft');
  await page.waitForFunction((i) => document.querySelector('.mc-duct-timeline').getAttribute('aria-valuenow') === String(i), idx, { timeout: 5000 });
}
// The first live map's view, the ducting layer's strip state (times and the
// shown index of either source: _tlTimes / _tlIndex).
function inPageView() {
  const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
  const c = m.getCenter();
  let layer = null;
  m.eachLayer((l) => { if (l._mcDuct) layer = l; });
  const tl = document.querySelector('.mc-duct-timeline');
  const o = m.getPixelOrigin(), pp = m._getMapPanePos();
  return {
    lat: c.lat, lng: c.lng, zoom: m.getZoom(), px: o.x - pp.x, py: o.y - pp.y,
    idx: tl ? parseInt(tl.getAttribute('aria-valuenow'), 10) : -1,
    scrollLeft: tl ? tl.scrollLeft : null,
    xs: layer && layer._tlLayout ? layer._tlLayout.xs : [],
    times: layer ? layer._tlTimes() : [],
  };
}
// Waits until the strip has settled: the shown time sits under the centre.
async function settled(page) {
  await page.waitForFunction(() => {
    const m = window.__mcHarness.maps.filter(x => x._container && x._container.isConnected && x._loaded)[0];
    let layer = null;
    m.eachLayer((l) => { if (l._mcDuct) layer = l; });
    const tl = document.querySelector('.mc-duct-timeline');
    if (!layer || !layer._tlLayout || !tl) return false;
    // Also wait for the overlay's own scroll to be over (_tlTarget cleared,
    // a frame after the strip arrives): a scrollLeft set by a test carries no
    // wheel/touch/pointer event, so inside that frame it is indistinguishable
    // from the end of the code's scroll and is ignored.
    return layer._tlTarget == null && Math.abs(tl.scrollLeft - layer._tlLayout.xs[layer._tlIndex()]) <= 1;
  }, null, { timeout: 4000, polling: 50 }).catch(() => {});
  return page.evaluate(inPageView);
}
// Same view: same zoom and the same pixel at the top left (getCenter() may
// round differently once the map has been touched, without any move).
const sameView = (a, b) => a.zoom === b.zoom && Math.abs(a.px - b.px) < 0.5 && Math.abs(a.py - b.py) < 0.5;
// The panel's box, where it lives, and its stored state.
function inPagePanel() {
  const p = document.querySelector('.mc-duct-panel');
  if (!p) return null;
  const r = p.getBoundingClientRect();
  const vis = (sel) => { const e = p.querySelector(sel); if (!e) return false; const b = e.getBoundingClientRect(); return b.width > 0 && b.height > 0 && getComputedStyle(e).display !== 'none'; };
  let stored = null;
  try { stored = JSON.parse(localStorage.getItem('mc-duct-panel')); } catch (_) {}
  return {
    l: r.left, t: r.top, w: r.width, h: r.height,
    floating: p.classList.contains('mc-duct-panel-floating'),
    inCorner: !!p.closest('.leaflet-bottom.leaflet-left'),
    inMap: p.parentNode && p.parentNode.classList.contains('leaflet-container'),
    legend: vis('.mc-duct-legend'), time: vis('.mc-duct-time'), reset: vis('.mc-duct-reset'),
    expanded: p.querySelector('.mc-duct-collapse').getAttribute('aria-expanded'),
    stored,
  };
}
const panelState = (page) => page.evaluate(inPagePanel);

module.exports = {
  loraFreqsOf, sig2, shownFmin, fmtFreq, STOP_N, fieldN, stopBetween,
  assert, sleep, createRunner,
  IGNORED_CONSOLE, newPage, gotoRoute, openPicker, setOverlayUi,
  inPageState, pageState, waitCells, inPageOverlaps, cssVars, inPageFieldProbe,
  neighbours, ringFactor, fieldProblems, outlineProblems, inPagePoint, pointsIn,
  timeIdx, setTime, inPageView, settled, sameView, inPagePanel, panelState,
};
