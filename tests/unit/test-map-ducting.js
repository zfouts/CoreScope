/* test-map-ducting.js — tropospheric ducting map overlay (map-ducting.js).
 *
 * Covers, against the real public/map-ducting.js in a vm sandbox with a fake
 * Leaflet, fake canvas, fake CSApi and fake timers, using
 * tests/fixtures/ducting-sample.json:
 *   - MC_ductTimeIndex boundaries
 *   - the colour bands (MC_ductFieldColor / MC_ductFieldStops): flat colour
 *     at and between stops, transparent for normal values, constant alpha,
 *     missing variables; the legend bar's hard stops
 *   - the field value is the cells' n (mean dN/dh over the lowest 1.5 km);
 *     n 0 or missing (an older server) is no data
 *   - the lattice, Mercator row -> latitude, Catmull-Rom bicubic sampling
 *     (exact at cell centres, continuous, bilinear next to missing data, the
 *     separable renderer equal to a naive 4x4 evaluation) and the fade to
 *     transparent at cells without data
 *   - the field image layer: pane, bounds, pixels, redrawn (not rebuilt) on
 *     time and theme change, resized/rebounded on refresh, removed on teardown
 *   - nothing interactive on the map: the cells with data are a list, the
 *     dashed LoRa outline (shown fmin <= the highest judged LoRa frequency:
 *     915 MHz by default, else the site's los.frequencyMhz) one
 *     non-interactive multi-polygon on a canvas in a pane without pointer
 *     events
 *   - map clicks: MC_ductCellAt lattice lookup (world copies), one map click
 *     handler that never stops the event and opens the cell popup only when
 *     the click did nothing else; the popup follows the slider and refreshes
 *   - the forecast area's dotted outline (merged boundary runs, antimeridian
 *     seam, one layer per build, colour from --mc-duct-edge) and the wider
 *     fade at the area's outer edge
 *   - popup text per class, the 1.5 km gradient line, the LoRa line (one
 *     threshold with the outline), the frequency to 2 significant figures
 *   - pending / error / failed-refresh legend states, legend content; the
 *     out-of-date notice on error, failure or a run-out forecast, not on
 *     pending; no rebuild when a refresh brings the same generatedAt
 *   - remove during an in-flight fetch, timer cleanup, refresh cadence
 *   - overlay registry + MC_buildOverlayLayer (factory absent -> null)
 *
 * Runs via: node tests/unit/test-map-ducting.js
 */
'use strict';
const REPO_ROOT = require('path').resolve(__dirname, '..', '..');
const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const SRC = fs.readFileSync(path.join(REPO_ROOT, 'public', 'map-ducting.js'), 'utf8');
const PROV = fs.readFileSync(path.join(REPO_ROOT, 'public', 'map-tile-providers.js'), 'utf8');
const SAMPLE = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, 'tests', 'fixtures', 'ducting-sample.json'), 'utf8'));
const T0 = SAMPLE.times[0], T1 = SAMPLE.times[1], T2 = SAMPLE.times[2];
// One colour per stop (-44, -48, -54, -58, -62, -70, -80, -90 N/km).
const STOP_RGB = [[128, 0, 128], [0, 0, 255], [0, 200, 200], [0, 160, 0], [240, 240, 0], [250, 150, 0], [230, 0, 0], [250, 0, 200]];
const STOP_N = [-44, -48, -54, -58, -62, -70, -80, -90];
const STOP_VARS = {};
STOP_RGB.forEach((c, k) => { STOP_VARS['--mc-duct-s' + (k + 1)] = '#' + c.map(v => v.toString(16).padStart(2, '0')).join(''); });
STOP_VARS['--mc-duct-s3'] = ' rgb(0, 200, 200) ';   // rgb() form too
const STOPS = STOP_N.map((g, k) => ({ g, rgb: STOP_RGB[k] }));

const queue = [];
const FOCUSED = [];   // elements focus() was called on, in order
function test(name, fn) { queue.push([name, fn]); }

// ── fakes ───────────────────────────────────────────────────────────────────

function makeEl(tag) {
  const listeners = {};
  let text = '';
  const el = {
    tagName: tag, className: '', children: [], attrs: {}, parentNode: null, value: '', listeners, style: {},
    appendChild(c) { el.children.push(c); c.parentNode = el; return c; },
    setAttribute(k, v) { el.attrs[k] = String(v); },
    addEventListener(t, f) { (listeners[t] = listeners[t] || []).push(f); },
    removeEventListener(t, f) { listeners[t] = (listeners[t] || []).filter(x => x !== f); },
    fire(t) { (listeners[t] || []).slice().forEach(f => f({ type: t })); },
    focus() { FOCUSED.push(el); },
  };
  Object.defineProperty(el, 'textContent', {
    get() { return text + el.children.map(c => c.textContent).join('\n'); },
    set(v) { text = String(v); el.children = []; },
  });
  if (tag === 'canvas') {
    el.width = 300; el.height = 150; el.puts = 0; el.img = null;
    const ctx2d = {
      createImageData: (w, h) => ({ width: w, height: h, data: new Uint8ClampedArray(w * h * 4) }),
      putImageData: (img) => { el.img = img; el.puts++; },
    };
    el.getContext = (k) => (k === '2d' ? ctx2d : null);
  }
  return el;
}

// RGBA of the field canvas at pixel (x, y).
function px(canvas, x, y) {
  const o = (y * canvas.img.width + x) * 4, d = canvas.img.data;
  return [d[o], d[o + 1], d[o + 2], d[o + 3]];
}

function makeLeaflet(stats) {
  const L = {};
  L.setOptions = (o, opts) => { o.options = Object.assign({}, o.options, opts); return o.options; };
  L.Layer = {
    extend(proto) {
      function C(opts) { this.initialize(opts); }
      Object.assign(C.prototype, proto);
      return C;
    },
  };
  L.Control = function (opts) { this.options = opts || {}; };
  L.Control.prototype.addTo = function (map) {
    this._map = map; this._container = this.onAdd(map); this._container.parentNode = map.corner; map._controls.push(this); return this;
  };
  L.Control.prototype.remove = function () {
    if (this._map._gone) throw new Error('map gone');
    this._map._controls = this._map._controls.filter(c => c !== this); this._removed = true; return this;
  };
  L.extend = Object.assign;
  L.latLngBounds = (b) => ({ b });
  L.canvas = (o) => { const r = { kind: 'canvas', opts: o }; stats.renderers.push(r); return r; };
  L.ImageOverlay = {
    extend(proto) {
      function F(url, bounds, opts) { this._url = url; this._bounds = bounds; this.options = opts; this.setBoundsCalls = 0; stats.fields.push(this); }
      Object.assign(F.prototype, proto);
      F.prototype.setBounds = function (b) { this._bounds = b; this.setBoundsCalls++; return this; };
      return F;
    },
  };
  L.layerGroup = () => {
    const g = { kind: 'group', layers: [] };
    g.addLayer = (l) => { g.layers.push(l); return g; };
    g.removeLayer = (l) => { g.layers = g.layers.filter(x => x !== l); return g; };
    g.clearLayers = () => { g.layers = []; return g; };
    return g;
  };
  L.polyline = (latlngs, opts) => {
    const l = { kind: 'polyline', latlngs, opts, style: {}, setStyleCalls: 0 };
    l.setStyle = (st) => { Object.assign(l.style, st); l.setStyleCalls++; return l; };
    stats.lines.push(l);
    return l;
  };
  L.polygon = (latlngs, opts) => {
    const l = { kind: 'polygon', latlngs, opts };
    stats.polys.push(l);
    return l;
  };
  // Nothing in the overlay may be an interactive vector any more.
  L.rectangle = () => { throw new Error('L.rectangle used'); };
  L.popup = (opts) => {
    const p = { opts, open: false, latlng: null, content: null, openCalls: 0 };
    p.setLatLng = (ll) => { p.latlng = ll; return p; };
    p.getLatLng = () => p.latlng;
    p.setContent = (c) => { p.content = c; return p; };
    p.openOn = (map) => { map._popup = p; p.open = true; p.openCalls++; return p; };
    p.isOpen = () => p.open;
    stats.popups.push(p);
    return p;
  };
  return L;
}

function makeMap() {
  const m = { _controls: [], _layers: [], _gone: false, _on: {}, _panes: {} };
  m.getPane = (n) => m._panes[n];
  m.createPane = (n) => { m._panes[n] = { style: {} }; return m._panes[n]; };
  m.on = (ev, f) => { ev.split(' ').forEach(e => { (m._on[e] = m._on[e] || []).push(f); }); return m; };
  m.off = (ev, f) => { ev.split(' ').forEach(e => { m._on[e] = (m._on[e] || []).filter(x => x !== f); }); return m; };
  m.fire = (e, arg) => (m._on[e] || []).slice().forEach(f => f(arg));
  m.closePopup = (p) => { p = p || m._popup; if (p) p.open = false; return m; };
  m.corner = { style: { marginBottom: '' }, getBoundingClientRect: () => ({ left: 0, right: 270 }) };
  m.attr = { offsetHeight: 36, rect: { left: 0 }, getBoundingClientRect() { return this.rect; } };
  m.attributionControl = { getContainer: () => m.attr };
  m.addLayer = (l) => { m._layers.push(l); return m; };
  m.removeLayer = (l) => { if (m._gone) throw new Error('map gone'); m._layers = m._layers.filter(x => x !== l); return m; };
  return m;
}

// Fake CSApi.fetch: each call is held until the test settles it.
function makeApi() {
  const api = { calls: [] };
  api.fetch = (url, opts) => new Promise((resolve, reject) => {
    api.calls.push({ url, opts, ok(body) { resolve({ ok: true, json: () => Promise.resolve(body) }); }, fail() { reject(new Error('net')); } });
  });
  return api;
}

function makeSandbox(opts) {
  opts = opts || {};
  const stats = { renderers: [], fields: [], lines: [], polys: [], popups: [] };
  const timers = [];
  const cleared = [];
  let nextId = 1;
  const winL = {};
  const vars = Object.assign({ '--mc-duct-lora': ' #0a0b0c ', '--mc-duct-edge': ' rgba(1, 2, 3, 0.8) ', '--mc-duct-alpha': '0.5' }, STOP_VARS);
  const ctx = {
    console, JSON, Date, Math, Object, Array, String, Number, isFinite, parseInt, Promise,
    document: { createElement: makeEl, documentElement: {} },
    addEventListener(t, f) { (winL[t] = winL[t] || []).push(f); },
    removeEventListener(t, f) { winL[t] = (winL[t] || []).filter(x => x !== f); },
    dispatch(t) { (winL[t] || []).slice().forEach(f => f({ type: t })); },
    getComputedStyle: () => ({ getPropertyValue: (n) => vars[n] || '' }),
    setInterval(fn, ms) { const id = nextId++; timers.push({ id, fn, ms }); return id; },
    clearInterval(id) { cleared.push(id); const i = timers.findIndex(t => t.id === id); if (i >= 0) timers.splice(i, 1); },
  };
  ctx.window = ctx;
  ctx.CSApi = makeApi();
  if (!opts.noLeaflet) ctx.L = makeLeaflet(stats);
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx, { filename: 'map-ducting.js' });
  return { ctx, stats, timers, cleared, api: ctx.CSApi, vars, winL };
}

const tick = () => new Promise(r => setImmediate(r));
const clone = (o) => JSON.parse(JSON.stringify(o));
// The cells outlined as LoRa now ("lat,lon" sorted), and the outline layer.
const loraLayer = (s) => s.layer._lora;
const loraSet = (s) => (s.layer._lora ? Array.from(s.layer._lora._mcCells, c => c.lat + ',' + c.lon).sort() : []);
// A map click as Leaflet delivers it (preclick, then click), on target.
function click(s, lat, lng, target) {
  const ev = { latlng: { lat, lng }, originalEvent: { target: target || { classList: { contains: () => false } } } };
  s.map.fire('preclick', ev);
  s.map.fire('click', ev);
  return ev;
}
const popupText = (s) => { const p = s.stats.popups[0]; return p && p.open ? p.content.textContent : null; };
function find(node, pred, out) {
  out = out || [];
  if (pred(node)) out.push(node);
  (node.children || []).forEach(c => find(c, pred, out));
  return out;
}

// Adds a layer to a fresh map with time frozen at `now`, resolves the fetch.
// los: window.MC_LOS_CFG (the site's line-of-sight config), unset by default.
async function mount(body, now, los) {
  const s = makeSandbox();
  if (los) s.ctx.MC_LOS_CFG = los;
  const realNow = Date.now;
  Date.now = () => (now != null ? now : T1 + 60) * 1000;
  try {
    const map = makeMap();
    const layer = s.ctx.MC_createDuctingLayer({});
    layer.onAdd(map);
    s.api.calls[0].ok(body);
    await tick(); await tick();
    return Object.assign(s, { map, layer });
  } finally { Date.now = realNow; }
}
const legendText = (s) => s.map._controls.map(c => c._container.textContent).join('\n');

// ── pure helpers ────────────────────────────────────────────────────────────

test('MC_ductTimeIndex: before first, between, after last, empty', () => {
  const { ctx } = makeSandbox();
  const ti = ctx.MC_ductTimeIndex;
  assert.strictEqual(ti(SAMPLE.times, T0 - 5), 0);
  assert.strictEqual(ti(SAMPLE.times, T0), 0);
  assert.strictEqual(ti(SAMPLE.times, T1 + 10), 1);
  assert.strictEqual(ti(SAMPLE.times, T2), 2);
  assert.strictEqual(ti(SAMPLE.times, T2 + 99999), 2);
  assert.strictEqual(ti([], 5), -1);
  assert.strictEqual(ti(null, 5), -1);
});

// ── colour bands ────────────────────────────────────────────────────────────

test('MC_ductFieldColor: each stop starts its band; flat colour down to the next stop, alpha 1', () => {
  const { ctx } = makeSandbox();
  const f = (g) => Array.from(ctx.MC_ductFieldColor(g, STOPS));
  STOPS.forEach((st, k) => {
    assert.deepStrictEqual(f(st.g), st.rgb.concat(1), 'at the stop g=' + st.g);
    const next = k + 1 < STOPS.length ? STOPS[k + 1].g : st.g - 1000;
    // Everywhere inside the band: the same colour, no blending toward the next.
    for (const t of [0.01, 0.25, 0.5, 0.75, 0.99]) assert.deepStrictEqual(f(st.g + (next - st.g) * t), st.rgb.concat(1), 'inside band ' + k + ' at ' + t);
    if (k + 1 < STOPS.length) {
      assert.deepStrictEqual(f(next + 1e-9), st.rgb.concat(1), 'just above the next stop');
      assert.deepStrictEqual(f(next), STOPS[k + 1].rgb.concat(1), 'the next stop belongs to the next band');
    }
  });
  assert.deepStrictEqual(f(-66.5), STOP_RGB[4].concat(1), 'between -62 and -70: the -62 colour, not a mix');
  assert.deepStrictEqual(f(-51), STOP_RGB[1].concat(1), 'between -48 and -54: the -48 colour');
  assert.deepStrictEqual(f(-7945), STOP_RGB[7].concat(1), 'beyond the last stop');
});

test('MC_ductFieldColor: transparent above -44 (normal), NaN or without stops; alpha never anything but 0 or 1', () => {
  const { ctx } = makeSandbox();
  const f = (g) => Array.from(ctx.MC_ductFieldColor(g, STOPS));
  for (const g of [100, 0, -30, -40, -43, -43.999]) assert.strictEqual(f(g)[3], 0, 'n=' + g);
  assert.deepStrictEqual(f(-44), STOP_RGB[0].concat(1), 'the colour starts at -44, at full alpha');
  for (let g = -44; g > -800; g -= 1.7) assert.strictEqual(f(g)[3], 1, 'constant alpha at ' + g);
  assert.strictEqual(f(NaN)[3], 0);
  assert.strictEqual(Array.from(ctx.MC_ductFieldColor(-300, []))[3], 0, 'no stops -> transparent');
  assert.strictEqual(Array.from(ctx.MC_ductFieldColor(-300, null))[3], 0);
});

test('MC_ductFieldStops: reads hex and rgb() variables, drops missing or unreadable ones', () => {
  const s = makeSandbox();
  const st = JSON.parse(JSON.stringify(s.ctx.MC_ductFieldStops()));
  assert.deepStrictEqual(st, STOPS);
  delete s.vars['--mc-duct-s2'];
  s.vars['--mc-duct-s5'] = 'tomato';
  s.vars['--mc-duct-s6'] = '#f90';
  const st2 = JSON.parse(JSON.stringify(s.ctx.MC_ductFieldStops()));
  assert.deepStrictEqual(st2.map(x => x.g), [-44, -54, -58, -70, -80, -90]);
  assert.deepStrictEqual(st2[3].rgb, [255, 153, 0], '#rgb expanded');
  // Without -48 the -44 band runs on to -54; without -62 the -58 one to -70.
  assert.deepStrictEqual(Array.from(s.ctx.MC_ductFieldColor(-50.5, st2)), STOP_RGB[0].concat(1));
  assert.deepStrictEqual(Array.from(s.ctx.MC_ductFieldColor(-65, st2)), STOP_RGB[3].concat(1));
  // Without -44 nothing is coloured above the first stop that is there.
  const st3 = st2.slice(1);
  assert.strictEqual(Array.from(s.ctx.MC_ductFieldColor(-50, st3))[3], 0);
  assert.deepStrictEqual(Array.from(s.ctx.MC_ductFieldColor(-54, st3)), [0, 200, 200, 1]);
});

// ── lattice, projection, sampling ───────────────────────────────────────────

test('MC_ductRowLat: rows follow Web Mercator, not linear latitude', () => {
  const { ctx } = makeSandbox();
  const lat = ctx.MC_ductRowLat;
  assert.ok(Math.abs(lat(0, 400, 60, 0) - 60) < 1e-9);
  assert.ok(Math.abs(lat(400, 400, 60, 0)) < 1e-9);
  const merc = (d) => Math.log(Math.tan(Math.PI / 4 + d * Math.PI / 360));
  const mid = lat(200, 400, 60, 0);
  assert.ok(Math.abs(merc(mid) - merc(60) / 2) < 1e-12, 'mid row is the Mercator midpoint');
  assert.ok(mid > 34 && mid < 36, 'and not 30: ' + mid);
  assert.ok(Math.abs(lat(100, 400, 35.5, 25) - lat(100, 400, 35.5, 25)) < 1e-12);
});

test('MC_ductGrid: lattice of the cells with data, outer edges as bounds, image size', () => {
  const { ctx } = makeSandbox();
  const g = ctx.MC_ductGrid(SAMPLE.cells, 0.5);
  assert.strictEqual(g.rows, 2);
  assert.strictEqual(g.cols, 2);
  assert.deepStrictEqual([g.south, g.west, g.north, g.east], [51, -0.5, 52, 0.5]);
  assert.deepStrictEqual(Array.from(g.slot), [0, 1, 2], 'row-major from the south-west');
  assert.strictEqual(g.w, 96, '48 px per cell');
  assert.ok(g.h > 96 && g.h < 180, 'Mercator stretches the height at 51N: ' + g.h);
  const none = clone(SAMPLE.cells);
  none.forEach(c => { c.c = [0, 0, 0]; });
  assert.strictEqual(ctx.MC_ductGrid(none, 0.5), null, 'no data at any hour');
  assert.strictEqual(ctx.MC_ductGrid(SAMPLE.cells, 0), null);
  const only = clone(SAMPLE.cells);
  only[2].c = [0, 0, 0];
  assert.strictEqual(ctx.MC_ductGrid(only, 0.5).slot[2], -1, 'a cell never with data is off the lattice');
});

test('MC_ductGrid: several far-apart regions share one image, capped at 2048 px', () => {
  const { ctx } = makeSandbox();
  const cell = (lat, lon) => ({ lat, lon, c: [1], f: [0], b: [0], t: [0], dm: [0], n: [-50] });
  const cells = [cell(-33.75, 151.25), cell(51.25, -0.25), cell(30.25, -95.25), cell(64.25, -147.75)];
  const g = ctx.MC_ductGrid(cells, 0.5);
  assert.ok(g.w <= 2048 && g.h <= 2048 && Math.max(g.w, g.h) === 2048, g.w + 'x' + g.h);
  assert.strictEqual(g.west, -148);
  assert.strictEqual(g.east, 151.5);
  // Lattice indices tolerate float noise in the centres.
  const n = ctx.MC_ductGrid([cell(30.25 + 1e-9, -95.25 - 1e-9), cell(30.75, -94.75)], 0.5);
  assert.strictEqual(n.rows, 2);
  assert.strictEqual(n.cols, 2);
});

test('MC_ductSample: value at centres, bilinear between, fades out at cells without data', () => {
  const { ctx } = makeSandbox();
  const cells = clone(SAMPLE.cells);
  const g = ctx.MC_ductGrid(cells, 0.5);
  const v = ctx.MC_ductGridValues(g, cells, 2);           // n: a=-66, b=-55, c=-40, (51.75,0.25) none
  assert.ok(Number.isNaN(v[3]), 'missing slot is NaN');
  const at = (lat, lon) => ctx.MC_ductSample(g, v, lat, lon);
  assert.strictEqual(at(51.25, -0.25).g, -66);
  assert.strictEqual(at(51.25, -0.25).a, 0.5, 'centre of an outer-edge cell: half faded');
  assert.strictEqual(at(51.25, 0).g, -60.5, 'halfway between a and b');
  assert.strictEqual(at(51.25, 0).a, 0.5);
  assert.strictEqual(at(51.25, 0.5).a, 0, 'b east edge: no neighbour, faded out');
  assert.strictEqual(at(51, -0.25).a, 0, 'south edge');
  const q = at(51.25, 0.375).a;
  assert.ok(q > 0 && q < 1, 'fading within half a cell: ' + q);
  assert.strictEqual(at(51.75, 0.25).a, 0, 'centre of the missing cell');
  assert.ok(Math.abs(at(51.5, 0).g - (-66 - 55 - 40) / 3) < 1e-4, 'mean over the cells that have data');
  const v0 = ctx.MC_ductGridValues(g, cells, 0);           // cell c is class 0 at t0
  assert.ok(Number.isNaN(v0[2]), 'class 0 at this hour is no data');
});

test('MC_ductGridValues: the field is n, not g; n 0, missing or not a number is no data', () => {
  const { ctx } = makeSandbox();
  const cells = clone(SAMPLE.cells);
  const g = ctx.MC_ductGrid(cells, 0.5);
  assert.deepStrictEqual(Array.from(ctx.MC_ductGridValues(g, cells, 1)).slice(0, 3), [-46, -57, -36], 'n at t1, not g (-96, -170, -38)');
  cells[0].n[1] = 0;                                       // 0 = no data at this hour
  cells[1].n = null;
  delete cells[2].n;
  const v = ctx.MC_ductGridValues(g, cells, 1);
  assert.ok([0, 1, 2, 3].every(k => Number.isNaN(v[k])), 'all no data: ' + Array.from(v));
  cells[1].n = [-50, 'x', -52];
  const w = ctx.MC_ductGridValues(g, cells, 1);
  assert.ok(Number.isNaN(w[1]), 'not a number');
  assert.strictEqual(ctx.MC_ductGridValues(g, cells, 2)[1], -52);
  assert.ok(Number.isNaN(ctx.MC_ductGridValues(g, cells, 7)[1]), 'past the end of n');
  assert.ok(Number.isNaN(ctx.MC_ductSample(g, v, 51.25, -0.25).g), 'no field anywhere');
});

// Catmull-Rom, written out the plain way: 16 lookups per point.
function crW(t) {
  const t2 = t * t, t3 = t2 * t;
  return [0.5 * (-t3 + 2 * t2 - t), 0.5 * (3 * t3 - 5 * t2 + 2), 0.5 * (-3 * t3 + 4 * t2 + t), 0.5 * (t3 - t2)];
}
function naiveCubic(vals, rows, cols, v, u) {
  const i = Math.floor(v), j = Math.floor(u), wv = crW(v - i), wu = crW(u - j);
  const cl = (x, n) => Math.max(0, Math.min(n - 1, x));
  let s = 0;
  for (let a = 0; a < 4; a++) for (let b = 0; b < 4; b++) s += wv[a] * wu[b] * vals[cl(i - 1 + a, rows) * cols + cl(j - 1 + b, cols)];
  return s;
}
// A filled rows x cols block with a smooth but not polynomial field g(lat, lon).
function wavy(rows, cols, step, lat0, lon0) {
  const cells = [];
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) {
      const g = -150 + 90 * Math.sin(r * 0.9) * Math.cos(c * 0.7) - 4 * r * c;
      cells.push({ lat: lat0 + r * step, lon: lon0 + c * step, c: [3], f: [500], b: [0], t: [0], dm: [0], n: [g] });
    }
  }
  return cells;
}

test('MC_ductSample: bicubic equals the data at every cell centre and matches a naive 4x4 Catmull-Rom between', () => {
  const { ctx } = makeSandbox();
  const cells = wavy(6, 7, 1, 20.5, -90.5);
  const g = ctx.MC_ductGrid(cells, 1);
  const v = ctx.MC_ductGridValues(g, cells, 0);
  cells.forEach((c) => assert.strictEqual(ctx.MC_ductSample(g, v, c.lat, c.lon).g, v[g.slot[cells.indexOf(c)]], 'centre ' + c.lat + ',' + c.lon));
  for (let lat = 20.01; lat < 26; lat += 0.173) {
    for (let lon = -90.99; lon < -84; lon += 0.191) {
      const want = naiveCubic(v, g.rows, g.cols, lat - 0.5 - g.i0, lon - 0.5 - g.j0);
      const got = ctx.MC_ductSample(g, v, lat, lon).g;
      assert.ok(Math.abs(got - want) < 1e-9, `${lat},${lon}: ${got} vs naive ${want}`);
    }
  }
  // Not bilinear: halfway between two centres the cubic bends with the neighbours.
  const bil = (v[2 * 7 + 2] + v[2 * 7 + 3]) / 2;
  assert.ok(Math.abs(ctx.MC_ductSample(g, v, 23, -87).g - bil) > 0.5, 'differs from bilinear');
});

test('MC_ductSample: bicubic is continuous across cell boundaries and centres', () => {
  const { ctx } = makeSandbox();
  const cells = wavy(6, 7, 1, 20.5, -90.5);
  const g = ctx.MC_ductGrid(cells, 1);
  const v = ctx.MC_ductGridValues(g, cells, 0);
  const at = (lat, lon) => ctx.MC_ductSample(g, v, lat, lon).g;
  const eps = 1e-7;
  for (let k = 0; k < 7; k++) {
    const lon = -90.5 + k;                                 // a lattice column: interval boundary
    for (const lat of [21.3, 23.5, 24.9]) {
      assert.ok(Math.abs(at(lat, lon - eps) - at(lat, lon + eps)) < 1e-4, `jump at lon ${lon}`);
    }
  }
  for (let r = 0; r < 6; r++) {
    const lat = 20.5 + r;
    assert.ok(Math.abs(at(lat - eps, -87.3) - at(lat + eps, -87.3)) < 1e-4, `jump at lat ${lat}`);
  }
  // A fine walk never jumps by more than the slope allows.
  let prev = at(23.2, -90.9);
  for (let lon = -90.9; lon < -84.1; lon += 0.001) {
    const cur = at(23.2, lon);
    assert.ok(Math.abs(cur - prev) < 1, `step at ${lon}: ${prev} -> ${cur}`);
    prev = cur;
  }
});

test('MC_ductSample: next to a cell without data the value falls back to bilinear', () => {
  const { ctx } = makeSandbox();
  const cells = wavy(6, 7, 1, 20.5, -90.5);
  const g = ctx.MC_ductGrid(cells, 1);
  const v = ctx.MC_ductGridValues(g, cells, 0);
  v[3 * 7 + 3] = NaN;                                      // no data at (23.5, -87.5) this hour
  const bilinear = (lat, lon) => {                        // over the slots that have data
    const vv = lat - 0.5 - g.i0, uu = lon - 0.5 - g.j0, i = Math.floor(vv), j = Math.floor(uu), fv = vv - i, fu = uu - j;
    let p = 0, s = 0;
    for (let a = 0; a < 2; a++) for (let b = 0; b < 2; b++) {
      const x = v[(i + a) * 7 + j + b];
      if (Number.isNaN(x)) continue;
      const w = (a ? fv : 1 - fv) * (b ? fu : 1 - fu);
      p += w; s += w * x;
    }
    return s / p;
  };
  // (24.2, -86.3): its 4x4 reaches the missing slot; (21.2, -85.2) does not.
  const near = ctx.MC_ductSample(g, v, 24.2, -86.3).g;
  assert.ok(Math.abs(near - bilinear(24.2, -86.3)) < 1e-9, near + ' vs bilinear ' + bilinear(24.2, -86.3));
  const far = ctx.MC_ductSample(g, v, 21.2, -85.2).g;
  assert.ok(Math.abs(far - naiveCubic(v, g.rows, g.cols, 21.2 - 0.5 - g.i0, -85.2 - 0.5 - g.j0)) < 1e-9, 'bicubic away from it');
  assert.ok(Math.abs(far - bilinear(21.2, -85.2)) > 1e-3, 'and that is not bilinear');
  assert.ok(Number.isFinite(near) && Number.isFinite(ctx.MC_ductSample(g, v, 23.5, -87.2).g), 'no NaN leaks while a neighbour has data');
});

test('MC_ductFieldValues: the separable renderer equals sample() and the naive 4x4 at every pixel', () => {
  const { ctx } = makeSandbox();
  const cells = wavy(5, 6, 1, 20.5, -90.5).filter(c => !(c.lat === 22.5 && c.lon === -88.5));   // with a hole
  const g = ctx.MC_ductGrid(cells, 1);
  const v = ctx.MC_ductGridValues(g, cells, 0);
  const fv = ctx.MC_ductFieldValues(g, v);
  assert.strictEqual(fv.g.length, g.w * g.h);
  let cubic = 0, fallback = 0, covered = 0;
  for (let y = 0; y < g.h; y += 3) {
    const lat = ctx.MC_ductRowLat(y + 0.5, g.h, g.north, g.south);
    for (let x = 0; x < g.w; x += 3) {
      const lon = g.west + (x + 0.5) / g.w * (g.east - g.west);
      const s = ctx.MC_ductSample(g, v, lat, lon), k = y * g.w + x;
      assert.strictEqual(fv.g[k], Math.fround(s.g), `g at ${x},${y}`);
      assert.strictEqual(fv.a[k], Math.fround(s.a), `a at ${x},${y}`);
      if (s.a > 0) covered++;
      const naive = naiveCubic(v, g.rows, g.cols, lat - 0.5 - g.i0, lon - 0.5 - g.j0);
      if (Number.isNaN(naive)) fallback++;
      else { cubic++; assert.ok(Math.abs(s.g - naive) < 1e-9, `${x},${y}: ${s.g} vs naive ${naive}`); }
    }
  }
  assert.ok(cubic > 100 && fallback > 100 && covered > 100, `both paths exercised: ${cubic} cubic, ${fallback} fallback`);
});

// A filled block of cells (rows x cols, lower-left centre lat0/lon0).
function block(rows, cols, lat0, lon0, step) {
  step = step || 0.5;
  const cells = [];
  for (let i = 0; i < rows; i++) for (let j = 0; j < cols; j++) {
    cells.push({ lat: lat0 + i * step, lon: lon0 + j * step, c: [1], f: [0], b: [0], t: [0], dm: [0], n: [-50] });
  }
  return cells;
}
const segs = (ctx, cells) => JSON.parse(JSON.stringify(ctx.MC_ductEdges(ctx.MC_ductGrid(cells, 0.5)))).map(sg => sg.map(p => p.join(',')).sort().join(' | ')).sort();

test('MC_ductEdges: a filled 3x4 block is 4 merged runs on its outer boundary', () => {
  const { ctx } = makeSandbox();
  // rows 3 (lat 10.25..11.25), cols 4 (lon 20.25..21.75): edges 10..11.5, 20..22
  assert.deepStrictEqual(segs(ctx, block(3, 4, 10.25, 20.25)), [
    '10,20 | 10,22', '11.5,20 | 11.5,22', '10,20 | 11.5,20', '10,22 | 11.5,22',
  ].map(x => x.split(' | ').sort().join(' | ')).sort());
});

test('MC_ductEdges: an L-shape has 6 runs; two separate blocks have two outlines', () => {
  const { ctx } = makeSandbox();
  // L: 2x2 block plus 2 extra cells to the east on the bottom row.
  const l = block(2, 2, 10.25, 20.25).concat(block(1, 2, 10.25, 21.25));
  assert.strictEqual(ctx.MC_ductEdges(ctx.MC_ductGrid(l, 0.5)).length, 6);
  const two = block(2, 2, 10.25, 20.25).concat(block(2, 2, 30.25, 40.25));
  const e = ctx.MC_ductEdges(ctx.MC_ductGrid(two, 0.5));
  assert.strictEqual(e.length, 8, '4 + 4');
  assert.ok(e.every(sg => Math.abs(sg[0][0] - sg[1][0]) <= 1 && Math.abs(sg[0][1] - sg[1][1]) <= 1), 'no run bridges the blocks');
  assert.strictEqual(ctx.MC_ductEdges(null).length, 0);
});

test('MC_ductEdges: a hole inside the area is outlined too', () => {
  const { ctx } = makeSandbox();
  const cells = block(3, 3, 10.25, 20.25).filter(c => !(c.lat === 10.75 && c.lon === 20.75));
  assert.strictEqual(ctx.MC_ductEdges(ctx.MC_ductGrid(cells, 0.5)).length, 8, '4 outer + 4 around the hole');
});

test('MC_ductEdges: a block across the antimeridian draws no line across the world and no seam line', () => {
  const { ctx } = makeSandbox();
  const cells = block(2, 1, 10.25, 179.75).concat(block(2, 1, 10.25, -179.75));
  const e = ctx.MC_ductEdges(ctx.MC_ductGrid(cells, 0.5));
  assert.ok(e.every(sg => Math.abs(sg[0][1] - sg[1][1]) <= 0.5 + 1e-9), 'no run spans more than a cell in lon');
  assert.ok(!e.some(sg => Math.abs(Math.abs(sg[0][1]) - 180) < 1e-9 && sg[0][1] === sg[1][1]), 'no line at lon +-180 where the cells meet');
  assert.strictEqual(e.length, 6, 'top and bottom of each half, west edge of the east half, east edge of the west half');
  // Without the other half the seam IS an edge.
  assert.strictEqual(ctx.MC_ductEdges(ctx.MC_ductGrid(block(2, 1, 10.25, 179.75), 0.5)).length, 4);
});

test('fade at the area\'s outer edge spans a cell; interior cells next to filled neighbours are unaffected', () => {
  const { ctx } = makeSandbox();
  const cells = block(5, 5, 10.25, 20.25);        // lon 20..22.5, lat 10..12.5
  const g = ctx.MC_ductGrid(cells, 0.5);
  const v = ctx.MC_ductGridValues(g, cells, 0);
  const a = (lat, lon) => ctx.MC_ductSample(g, v, lat, lon).a;
  assert.strictEqual(a(11.25, 20), 0, 'at the edge');
  assert.ok(Math.abs(a(11.25, 20.25) - 0.5) < 1e-9, 'half a cell in: ' + a(11.25, 20.25));
  assert.ok(a(11.25, 20.125) > 0 && a(11.25, 20.125) < a(11.25, 20.25), 'rising within the outer half cell');
  assert.ok(a(11.25, 20.5) > 0.5 && a(11.25, 20.5) < 1, 'still fading between the first and second cell');
  assert.strictEqual(a(11.25, 20.75), 1, 'second cell in: full');
  assert.strictEqual(a(11.25, 21.25), 1, 'interior');
  assert.strictEqual(a(11.25, 21.75), 1, 'interior, next to the outer ring');
  // A hole in an otherwise filled area: the cells next to it are still the interior's... a hole is an edge too.
  const holed = cells.filter(c => !(c.lat === 11.25 && c.lon === 21.25));
  const gh = ctx.MC_ductGrid(holed, 0.5);
  assert.strictEqual(gh.soft[(1) * 5 + 0], 1);
});

// ── drawing ─────────────────────────────────────────────────────────────────

// Pixel nearest (lat, lon) in the field canvas.
function pixelAt(s, lat, lon) {
  const g = s.layer._grid, cv = s.layer._canvas;
  const merc = (d) => Math.log(Math.tan(Math.PI / 4 + d * Math.PI / 360));
  const x = Math.floor((lon - g.west) / (g.east - g.west) * cv.width);
  const y = Math.floor((merc(g.north) - merc(lat)) / (merc(g.north) - merc(g.south)) * cv.height);
  return { x, y, rgba: px(cv, x, y) };
}

test('nothing interactive: the cells with data are a list; outlines on one canvas in a pane without pointer events', async () => {
  const s = await mount(SAMPLE, T0);
  assert.strictEqual(s.layer._cells.length, 3, 'third cell is class 0/1/1: has data');
  assert.strictEqual(s.layer._mcDuct, true);
  assert.strictEqual(s.stats.renderers.length, 1);
  const r = s.stats.renderers[0];
  assert.strictEqual(r.opts.pane, 'mcDuctLinePane');
  const pane = s.map._panes.mcDuctLinePane;
  assert.strictEqual(pane.style.pointerEvents, 'none', 'the line canvas never takes the pointer');
  assert.ok(+pane.style.zIndex > +s.map._panes.mcDuctFieldPane.style.zIndex && +pane.style.zIndex < 390, 'above the field, below vectors and the planner node pane: ' + pane.style.zIndex);
  // The group holds the edge and the LoRa outline only, both non-interactive.
  const kinds = s.layer._group.layers.map(l => (l._mcEdge ? 'edge' : l._mcLora ? 'lora' : 'other'));
  assert.deepStrictEqual(kinds.sort(), ['edge', 'lora']);
  s.layer._group.layers.forEach((l) => { assert.strictEqual(l.opts.interactive, false); assert.strictEqual(l.opts.renderer, r); });
  assert.strictEqual(s.layer._popup, undefined, 'no popup until a click');
});

test('MC_ductCellAt: the cell whose square holds the point; world copies wrap; -1 outside', () => {
  const { ctx } = makeSandbox();
  const g = ctx.MC_ductGrid(SAMPLE.cells, 0.5);
  const at = (lat, lon) => ctx.MC_ductCellAt(g, lat, lon);
  assert.strictEqual(at(51.25, -0.25), 0, 'centre');
  assert.strictEqual(at(51.0, -0.5), 0, 'south-west corner belongs to the cell');
  assert.strictEqual(at(51.4999, -0.0001), 0);
  assert.strictEqual(at(51.3, 0.0), 1, 'east of the shared edge');
  assert.strictEqual(at(51.9, -0.4), 2);
  assert.strictEqual(at(51.75, 0.25), -1, 'slot on the lattice without a cell');
  assert.strictEqual(at(50.99, -0.25), -1, 'south of the lattice');
  assert.strictEqual(at(51.25, 0.5), -1, 'east edge is outside');
  assert.strictEqual(at(51.25, 359.75), 0, 'world copy east');
  assert.strictEqual(at(51.25, -360.25), 0, 'world copy west');
  assert.strictEqual(at(NaN, 0), -1);
  assert.strictEqual(ctx.MC_ductCellAt(null, 51.25, -0.25), -1);
  // A cell never with data is not on the lattice: nothing to find there.
  const only = clone(SAMPLE.cells); only[2].c = [0, 0, 0];
  assert.strictEqual(ctx.MC_ductCellAt(ctx.MC_ductGrid(only, 0.5), 51.75, -0.25), -1);
  // Across the antimeridian.
  const am = [{ lat: 10.25, lon: 179.75, c: [1] }, { lat: 10.25, lon: -179.75, c: [1] }];
  const ga = ctx.MC_ductGrid(am, 0.5);
  assert.strictEqual(ctx.MC_ductCellAt(ga, 10.3, 179.9), 0);
  assert.strictEqual(ctx.MC_ductCellAt(ga, 10.3, 180.1), 1, '180.1 is -179.9');
  assert.strictEqual(ctx.MC_ductCellAt(ga, 10.3, -179.9), 1);
});

test('edge outline: one non-interactive dotted polyline per build, in the group, on the shared renderer', async () => {
  const s = await mount(SAMPLE, T0);
  assert.strictEqual(s.stats.lines.length, 1);
  const l = s.stats.lines[0];
  assert.strictEqual(l._mcEdge, true);
  assert.ok(s.layer._group.layers.includes(l), 'removed with the group');
  assert.strictEqual(l.opts.renderer, s.stats.renderers[0]);
  assert.strictEqual(l.opts.interactive, false);
  assert.strictEqual(l.opts.weight, 1);
  assert.strictEqual(l.opts.dashArray, '1 5');
  assert.notStrictEqual(l.opts.dashArray, '4 3', 'not the LoRa dash');
  assert.strictEqual(l.opts.color, 'rgba(1, 2, 3, 0.8)', 'from --mc-duct-edge, trimmed');
  assert.ok(l.opts.opacity > 0 && l.opts.opacity < 1);
  assert.ok(l.latlngs.length >= 4, 'merged runs');
  // Not rebuilt on slider moves or theme changes; restyled only when the colour changes.
  s.layer._setIndex(2);
  s.ctx.dispatch('theme-changed');
  assert.strictEqual(s.stats.lines.length, 1);
  assert.strictEqual(l.setStyleCalls, 0, 'same colour: no restyle');
  s.vars['--mc-duct-edge'] = '#abcdef';
  s.ctx.dispatch('theme-changed');
  assert.strictEqual(s.stats.lines.length, 1);
  assert.strictEqual(l.style.color, '#abcdef');
  assert.strictEqual(l.setStyleCalls, 1);
  // A rebuild (refresh) makes a fresh one.
  s.layer._build();
  assert.strictEqual(s.stats.lines.length, 2);
  assert.strictEqual(s.layer._group.layers.filter(x => x._mcEdge).length, 1);
  assert.strictEqual(s.stats.lines[1].opts.color, '#abcdef');
  const group = s.layer._group;
  s.layer.onRemove(s.map);
  assert.strictEqual(s.layer._edge, null);
  assert.ok(!s.map._layers.includes(group), 'group (and the edge) gone from the map');
});

test('edge outline: no colour variable -> stroke off, never invented', async () => {
  const s = makeSandbox();
  s.vars['--mc-duct-edge'] = '';
  const map = makeMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  s.api.calls[0].ok(clone(SAMPLE));
  await tick(); await tick();
  assert.strictEqual(s.stats.lines[0].opts.stroke, false);
});

test('legend: dotted-line row for the forecast area edge', async () => {
  const s = await mount(SAMPLE);
  assert.ok(legendText(s).includes('Dotted line: edge of the forecast area (no forecast beyond it)'));
  assert.strictEqual(find(s.map._controls[1]._container, n => /mc-duct-swatch-edge/.test(n.className || '')).length, 1);
});

test('field: one image layer in its own pane below markers, over the cells\' outer edges', async () => {
  const s = await mount(SAMPLE);
  assert.strictEqual(s.stats.fields.length, 1);
  const f = s.stats.fields[0];
  assert.ok(s.map._layers.includes(f), 'added to the map');
  assert.strictEqual(f._mcField, true);
  assert.strictEqual(f._url, s.layer._canvas, 'the image is the canvas itself');
  assert.strictEqual(f.options.pane, 'mcDuctFieldPane');
  assert.strictEqual(f.options.interactive, false);
  const pane = s.map._panes.mcDuctFieldPane;
  assert.strictEqual(pane.style.pointerEvents, 'none');
  assert.ok(+pane.style.zIndex > 200 && +pane.style.zIndex < 400, 'between tiles and vectors');
  assert.deepStrictEqual(JSON.parse(JSON.stringify(f._bounds)), [[51, -0.5], [52, 0.5]]);
  assert.strictEqual(s.layer._canvas.width, 96);
  assert.strictEqual(s.layer._canvas.height, s.layer._grid.h);
});

test('field pixels: cell coloured like MC_ductFieldColor of its n, normal cell transparent', async () => {
  const s = await mount(SAMPLE, T2);                       // a: surface duct, n=-66
  const p = pixelAt(s, 51.25, -0.25);
  // Exactly the band colour of the value sampled at that pixel's centre ...
  const g = s.layer._grid, cv = s.layer._canvas;
  const lat = s.ctx.MC_ductRowLat(p.y + 0.5, cv.height, g.north, g.south);
  const lon = g.west + (p.x + 0.5) / cv.width * (g.east - g.west);
  const smp = s.ctx.MC_ductSample(g, s.ctx.MC_ductGridValues(g, s.layer._data.cells, 2), lat, lon);
  const exact = s.ctx.MC_ductFieldColor(smp.g, STOPS);
  for (let k = 0; k < 3; k++) assert.ok(Math.abs(p.rgba[k] - exact[k]) <= 2, `channel ${k}: ${p.rgba} vs ${exact}`);
  assert.ok(Math.abs(p.rgba[3] - 0.5 * 255 * smp.a) <= 2, 'alpha = --mc-duct-alpha x coverage: ' + p.rgba[3]);
  // ... which, half a pixel from the centre, is close to the cell's own n.
  assert.ok(Math.abs(smp.g - -66) < 1, 'sampled ' + smp.g);
  const want = s.ctx.MC_ductFieldColor(-66, STOPS);
  assert.deepStrictEqual(p.rgba.slice(0, 3), Array.from(want).slice(0, 3), 'the -62..-70 band colour, flat');
  assert.deepStrictEqual(p.rgba.slice(0, 3), STOP_RGB[4]);
  const s0 = await mount(SAMPLE, T0);                      // a: n=-38 (above -44)
  assert.strictEqual(pixelAt(s0, 51.25, -0.25).rgba[3], 0, 'normal cell transparent');
  assert.ok(pixelAt(s0, 51.25, 0.25).rgba[3] > 40, 'n=-60 next to it is coloured (every cell here is on the outer edge: half faded)');
});

test('field pixels: soft edge where a neighbour has no data, nothing outside the cells', async () => {
  const s = await mount(SAMPLE, T2);
  const cv = s.layer._canvas;
  const W = cv.width, H = cv.height;
  // Cell b (51.25, 0.25) has no neighbour to the east: alpha falls toward the edge.
  const yb = pixelAt(s, 51.25, 0.25).y;
  const row = [];
  for (let x = W * 3 / 4; x < W; x++) row.push(px(cv, x, yb)[3]);
  for (let k = 1; k < row.length; k++) assert.ok(row[k] <= row[k - 1], 'monotonic fade: ' + row.join(','));
  assert.ok(row[0] > 40 && row[row.length - 1] < 10, row.join(','));
  // The missing cell's centre (51.75, 0.25) is transparent; so are the corners.
  assert.strictEqual(pixelAt(s, 51.75, 0.25).rgba[3], 0);
  assert.strictEqual(px(cv, W - 1, 0)[3], 0);
  assert.strictEqual(px(cv, 0, H - 1)[3], 0);
});

test('default time is the latest <= now, else the first', async () => {
  const s = await mount(SAMPLE, T1 + 60);
  assert.strictEqual(s.layer._idx, 1);
  const early = await mount(SAMPLE, T0 - 3600);
  assert.strictEqual(early.layer._idx, 0);
});

test('LoRa outline: dashed --mc-duct-lora only for ducts whose shown fmin <= 915 MHz (default bands)', async () => {
  const s = await mount(SAMPLE, T0);
  assert.deepStrictEqual(loraSet(s), ['51.25,0.25'], 'elevated duct f=78; class 1 and 0 cells not');
  const l = loraLayer(s);
  assert.strictEqual(l.opts.color, '#0a0b0c', 'value trimmed');
  assert.strictEqual(l.opts.dashArray, '4 3');
  assert.strictEqual(l.opts.fill, false, 'outline only, no fill');
  assert.strictEqual(l.opts.interactive, false);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(l.latlngs)), [[[[51, 0], [51, 0.5], [51.5, 0.5], [51.5, 0]]]], 'one ring per cell: centre +- step/2');
  s.layer._setIndex(1);
  assert.deepStrictEqual(loraSet(s), [], 'super-refraction, f=1500');
  assert.strictEqual(loraLayer(s), null, 'no empty layer left');
  s.layer._setIndex(2);
  assert.deepStrictEqual(loraSet(s), ['51.25,-0.25'], 'surface duct f=380; f=4200 not');
  const body = clone(SAMPLE);
  body.cells[0].c = [3, 4, 3]; body.cells[0].f = [868, 920, 0];
  body.cells[1].f = [914, 1000, 4200];
  const e = await mount(body, T0);
  assert.deepStrictEqual(loraSet(e), ['51.25,-0.25', '51.25,0.25'], 'f=868 (shown 870) and f=914 (shown 910) are <= 915');
  e.layer._setIndex(1);
  assert.deepStrictEqual(loraSet(e), [], 'f=920, f=1000 (shown 920, 1000) are above 915');
  e.layer._setIndex(2);
  assert.deepStrictEqual(loraSet(e), [], 'class 3 with f=0 is no duct');
  const g = clone(SAMPLE);
  g.cells[0].c = [3, 3, 3]; g.cells[0].f = [916, 925, 900];
  const ge = await mount(g, T0);
  assert.deepStrictEqual(loraSet(ge), ['51.25,0.25'], 'f=916 shows 920: above 915 even though f itself is close');
  ge.layer._setIndex(2);
  assert.ok(loraSet(ge).includes('51.25,-0.25'), 'f=900 outlined (trapped at 915)');
});

test('default bands: f=900 is outlined; popup says 868 not trapped, 915 possibly', async () => {
  const body = clone(SAMPLE);
  body.cells[0].c = [4, 4, 4]; body.cells[0].f = [900, 900, 900];
  const s = await mount(body, T0);
  assert.ok(loraSet(s).includes('51.25,-0.25'));
  const lines = s.ctx.MC_ductPopupContent(body.cells[0], 0).textContent.split('\n').filter(l => /^LoRa /.test(l));
  assert.deepStrictEqual(lines, ['LoRa 868 MHz: not trapped by this layer', 'LoRa 915 MHz: possibly trapped (model estimate)']);
});

test('site frequency 910.525 MHz: one verdict line, outline threshold 910.525 (905 yes, 950 no)', async () => {
  const body = clone(SAMPLE);
  body.cells[0].c = [4, 4, 4]; body.cells[0].f = [905, 950, 868];
  const s = await mount(body, T0, { frequencyMhz: 910.525 });
  const pop = (i) => s.ctx.MC_ductPopupContent(body.cells[0], i).textContent;
  const lora = (i) => pop(i).split('\n').filter(l => /^LoRa /.test(l));
  assert.ok(loraSet(s).includes('51.25,-0.25'), 'f=905 (shown 910) outlined');
  assert.deepStrictEqual(lora(0), ['LoRa 910.525 MHz: possibly trapped (model estimate)']);
  assert.ok(pop(0).includes('Lowest trapped frequency: \u2248 910 MHz (model estimate)'), pop(0));
  s.layer._setIndex(1);
  assert.ok(!loraSet(s).includes('51.25,-0.25'), 'f=950 not outlined');
  assert.deepStrictEqual(lora(1), ['LoRa 910.525 MHz: not trapped by this layer']);
  s.layer._setIndex(2);
  assert.ok(loraSet(s).includes('51.25,-0.25'), 'f=868 (shown 870) outlined at 910.525');
  assert.ok(legendText(s).includes('Dashed outline: the model shows a duct that may carry LoRa at 910.525 MHz'), legendText(s));
  // Out of range site frequencies fall back to the two bands.
  for (const bad of [50, 7000, 'x', null]) {
    const b = await mount(clone(SAMPLE), T0, { frequencyMhz: bad });
    assert.ok(legendText(b).includes('LoRa at 868 / 915 MHz'), 'frequencyMhz ' + bad);
  }
});

test('LoRa outline and popup agree for every class and frequency (default bands and a site frequency)', async () => {
  const fs_ = [0, 78, 380, 700, 864, 865, 868, 905, 909, 914, 915, 916, 950, 1000, 1500];
  const body = clone(SAMPLE);
  body.times = fs_.map((_, i) => T0 + i * 3600);
  const mk = (c) => ({ lat: 51.25, lon: -0.25, c: fs_.map(() => c), f: fs_.slice(), b: fs_.map(() => 0), t: fs_.map(() => 100), dm: fs_.map(() => 50), n: fs_.map(() => -60) });
  const sig2 = (v) => Number(Number(v).toPrecision(2));
  for (const [los, freqs] of [[null, [868, 915]], [{ frequencyMhz: 910.525 }, [910.525]]]) {
    for (const cls of [2, 3, 4]) {
      body.cells = [mk(cls)];
      const s = await mount(body, T0, los);
      fs_.forEach((f, i) => {
        s.layer._setIndex(i);
        const outlined = loraSet(s).length === 1;
        const txt = s.ctx.MC_ductPopupContent(body.cells[0], i).textContent;
        const lines = txt.split('\n').filter(l => /^LoRa /.test(l));
        const shown = cls >= 3 && f > 0 ? sig2(f) : 0;
        assert.strictEqual(lines.length, freqs.length, 'one line per judged frequency: ' + txt);
        freqs.forEach((fr, k) => {
          const want = shown > 0 && shown <= fr;
          assert.strictEqual(lines[k], `LoRa ${fr} MHz: ` + (want ? 'possibly trapped (model estimate)' : 'not trapped by this layer'), `class ${cls} f=${f} at ${fr}`);
        });
        const says = lines.some(l => /possibly trapped/.test(l));
        assert.strictEqual(outlined, says, `class ${cls} f=${f}: outline ${outlined}, popup ${txt}`);
        assert.strictEqual(outlined, shown > 0 && shown <= freqs[freqs.length - 1], `class ${cls} f=${f}`);
        // The frequency line shows the very value the verdicts used.
        assert.ok(txt.includes('Lowest trapped frequency: ' + (shown ? '\u2248 ' + shown + ' MHz (model estimate)' : 'none')), txt);
      });
    }
  }
});

test('changing time redraws the field in place: same cells, image layer, renderer; no fetch', async () => {
  const s = await mount(SAMPLE, T0);
  const cells = s.layer._cells.slice();
  const calls = s.api.calls.length;
  const cv = s.layer._canvas, puts = cv.puts;
  const before = pixelAt(s, 51.25, -0.25).rgba;
  s.layer._slider.value = '2';
  s.layer._slider.fire('input');
  assert.ok(s.layer._cells.every((c, i) => c === cells[i]) && s.layer._cells.length === cells.length);
  assert.strictEqual(s.stats.renderers.length, 1);
  assert.strictEqual(s.stats.fields.length, 1);
  assert.strictEqual(s.stats.fields[0].setBoundsCalls, 0, 'bounds unchanged');
  assert.strictEqual(s.layer._canvas, cv);
  assert.strictEqual(cv.puts, puts + 1, 'one redraw');
  assert.strictEqual(s.api.calls.length, calls);
  assert.strictEqual(s.layer._idx, 2);
  assert.notDeepStrictEqual(pixelAt(s, 51.25, -0.25).rgba, before, 'n -38 -> -66');
  const img = cv.img;
  s.layer._setIndex(0);
  assert.strictEqual(cv.img, img, 'pixel buffer reused for the same size');
  assert.deepStrictEqual(pixelAt(s, 51.25, -0.25).rgba, before, 'and cleared: duct -> normal is transparent again');
  const polys = s.stats.polys.length, l = loraLayer(s);
  s.layer._setIndex(0);
  s.ctx.dispatch('theme-changed');
  assert.strictEqual(s.stats.polys.length, polys, 'same outlined cells and colour: outline not rebuilt');
  assert.strictEqual(loraLayer(s), l);
  assert.ok(/Forecast time:/.test(s.layer._timeLabel.textContent));
  assert.strictEqual(s.layer._slider.max, '2');
});

// Correctness only: wall-clock bounds do not belong in the unit gate.
test('field build on a Gulf-sized lattice (860 cells): full width, pixels drawn', () => {
  const { ctx } = makeSandbox();
  const cells = [];
  for (let i = 0; i < 21; i++) {
    for (let j = 0; j < 41; j++) {
      if (cells.length >= 860) break;
      cells.push({ lat: 25.25 + i * 0.5, lon: -99.75 + j * 0.5, c: [((i + j) % 4) + 1], f: [500], b: [0], t: [0], dm: [0], n: [-36 - ((i * 7 + j * 13) % 60)] });
    }
  }
  const g = ctx.MC_ductGrid(cells, 0.5);
  const vals = ctx.MC_ductGridValues(g, cells, 0);
  let n = 0;
  for (let y = 0; y < g.h; y += 7) for (let x = 0; x < g.w; x += 7) if (ctx.MC_ductSample(g, vals, ctx.MC_ductRowLat(y + 0.5, g.h, g.north, g.south), g.west + (x + 0.5) / g.w * (g.east - g.west)).a > 0) n++;
  assert.ok(n > 0);
  // The real renderer through a layer:
  const s = makeSandbox();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(makeMap());
  layer._data = { status: 'ok', stepDeg: 0.5, times: [T0], cells };
  layer._build();
  layer._idx = 0;
  layer._drawField();
  assert.strictEqual(layer._canvas.width, 1968, '48 px per cell, under the 2048 cap');
  const d = layer._canvas.img.data;
  let drawn = 0;
  for (let i = 3; i < d.length; i += 4 * 97) if (d[i]) drawn++;
  assert.ok(drawn > 0, 'pixels drawn');
});

test('slider is disabled until data arrives', () => {
  const s = makeSandbox();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(makeMap());
  assert.strictEqual(layer._slider.disabled, true);
});

// ── popup ───────────────────────────────────────────────────────────────────

// The LoRa verdict lines at the default bands (868 and 915 MHz).
const loraLines = (both) => ['LoRa 868 MHz: ' + both[0], 'LoRa 915 MHz: ' + both[1]];
const YES = 'possibly trapped (model estimate)', NO = 'not trapped by this layer';
const POSSIBLY = loraLines([YES, YES]).join('\n');          // trapped at both
const NOT_TRAPPED = loraLines([NO, NO]).join('\n');         // at neither
const LAYER_LINE = 'Trapping layer in the model (its levels are ~200 m apart, so thinner layers are not resolved)';

test('popup text for each class: trapping layer, frequency estimate and the LoRa line', async () => {
  const s = await mount(SAMPLE, T0);
  const pop = (cell, i) => s.ctx.MC_ductPopupContent(cell, i).textContent;
  const [a, b] = SAMPLE.cells;
  const normal = pop(a, 0);
  assert.ok(/Normal/.test(normal) && !/868|LoRa/.test(normal));
  assert.ok(normal.includes('Gradient, lowest 1.5 km: \u221238 N/km'), normal);
  const sup = pop(a, 1);
  assert.ok(/Super-refraction/.test(sup) && sup.includes(NOT_TRAPPED) && /Lowest trapped frequency: none/.test(sup), sup);
  assert.ok(!sup.includes(LAYER_LINE), 'no trapping layer for super-refraction');
  assert.ok(sup.includes('Gradient, lowest 1.5 km: \u221246 N/km'), sup);
  assert.ok(!/Can carry|: yes|: no\b/.test(sup), 'no yes/no answers: ' + sup);
  const sfc = pop(a, 2);
  assert.deepStrictEqual(sfc.split('\n'), [
    'Surface-based duct',
    'Gradient, lowest 1.5 km: \u221266 N/km',
    LAYER_LINE,
    'Base: 0 m above ground',
    'Top: 100 m above ground',
    'Strength: 10.0 M-units',
    'Lowest trapped frequency: \u2248 380 MHz (model estimate)',
  ].concat(loraLines([YES, YES])));
  assert.ok(!/\u2265 ~200 m thick/.test(sfc), 'old thickness claim gone');
  const elev = pop(b, 1);
  assert.ok(/Elevated duct/.test(elev) && elev.includes(LAYER_LINE));
  assert.ok(/Base: 600 m/.test(elev) && /Top: 820 m/.test(elev) && /Strength: 6\.0/.test(elev), elev);
  assert.ok(elev.includes('Lowest trapped frequency: \u2248 1500 MHz (model estimate)'), elev);
  assert.ok(elev.includes(NOT_TRAPPED), elev);
  assert.ok(elev.includes('Gradient, lowest 1.5 km: \u221257 N/km'), elev);
  assert.ok(pop(b, 0).includes('\u2248 78 MHz') && pop(b, 0).includes(POSSIBLY), pop(b, 0));
});

test('popup: lowest trapped frequency rounded to 2 significant figures', async () => {
  const s = makeSandbox();
  const cell = { lat: 0, lon: 0, c: [3, 3, 3, 3, 3, 3, 3], f: [78.4, 868, 915, 1234, 4249, 99.6, 905], b: [0, 0, 0, 0, 0, 0, 0], t: [0, 0, 0, 0, 0, 0, 0], dm: [0, 0, 0, 0, 0, 0, 0] };
  const got = cell.f.map((_, i) => /Lowest trapped frequency: \u2248 (\d+) MHz \(model estimate\)/.exec(s.ctx.MC_ductPopupContent(cell, i).textContent)[1]);
  assert.deepStrictEqual(got, ['78', '870', '920', '1200', '4200', '100', '910']);
  // The verdicts follow the shown figure: f=868 shows 870, so not at 868 but at 915;
  // f=915 shows 920, at neither; f=905 shows 910, at 915.
  const t = (i) => s.ctx.MC_ductPopupContent(cell, i).textContent;
  assert.ok(t(1).includes(loraLines([NO, YES]).join('\n')), '868 itself: ' + t(1));
  assert.ok(t(2).includes(NOT_TRAPPED), '915 itself: ' + t(2));
  assert.ok(t(6).includes(loraLines([NO, YES]).join('\n')), '905: ' + t(6));
});

test('popup: gradient line rounded, only when n has data; class 0 shows no data', async () => {
  const body = clone(SAMPLE);
  body.cells[0].n = [-52.6, 0, 7];
  delete body.cells[1].n;
  const s = await mount(body, T0);
  const pop = (cell, i) => s.ctx.MC_ductPopupContent(cell, i).textContent;
  const [a, b, c] = body.cells;
  assert.ok(pop(a, 0).includes('Gradient, lowest 1.5 km: \u221253 N/km'), pop(a, 0));
  assert.ok(!/Gradient/.test(pop(a, 1)), 'n 0: no line: ' + pop(a, 1));
  assert.ok(pop(a, 2).includes('Gradient, lowest 1.5 km: 7 N/km'), pop(a, 2));
  const eb = pop(b, 0);
  assert.ok(!/Gradient/.test(eb) && /Elevated duct/.test(eb) && eb.includes(POSSIBLY), 'no n: the rest as before: ' + eb);
  assert.strictEqual(pop(c, 0), 'No data');
});

test('older server without n: no field, but cells, LoRa outlines, popups and edge as before; no throw', async () => {
  const body = clone(SAMPLE);
  body.cells.forEach(c => { delete c.n; });
  const s = await mount(body, T2);
  assert.strictEqual(s.layer._cells.length, 3);
  assert.strictEqual(s.stats.fields.length, 1, 'the (empty) image layer is still there');
  assert.ok(s.layer._canvas.img.data.every(v => v === 0), 'nothing coloured');
  assert.deepStrictEqual(loraSet(s), ['51.25,-0.25'], 'surface duct f=380 still outlined');
  assert.strictEqual(s.stats.lines.length, 1, 'forecast-area edge');
  click(s, 51.25, -0.25);
  await tick();
  assert.ok(/Surface-based duct/.test(popupText(s)), 'popup by click: ' + popupText(s));
  for (const i of [0, 1, 2]) s.layer._setIndex(i);
  s.ctx.dispatch('theme-changed');
  assert.ok(s.layer._canvas.img.data.every(v => v === 0));
});

test('popup: one LoRa line per default band, no yes/no; no innerHTML use', async () => {
  const body = clone(SAMPLE);
  body.cells[1].f = [900, 860, 915];
  const s = await mount(body, T0);
  const pop = (i) => s.ctx.MC_ductPopupContent(body.cells[1], i).textContent;
  assert.ok(pop(0).includes(loraLines([NO, YES]).join('\n')) && !/Can carry|: yes|: no\b/.test(pop(0)), pop(0));
  assert.ok(pop(1).includes(POSSIBLY), '860 shows 860: both bands: ' + pop(1));
  assert.ok(pop(2).includes(NOT_TRAPPED), '915 shows 920, above both: ' + pop(2));
  assert.ok(!/innerHTML/.test(SRC.replace(/\/\*[\s\S]*?\*\/|\/\/.*$/gm, '')), 'no innerHTML in map-ducting.js');
});

// ── map clicks ──────────────────────────────────────────────────────────────

test('click in a cell with data opens its popup after the click, without stopping it', async () => {
  const s = await mount(SAMPLE, T2);
  assert.strictEqual((s.map._on.click || []).length, 1, 'one map click handler');
  const ev = click(s, 51.3, -0.2);
  assert.strictEqual(popupText(s), null, 'not inside the click: the other handlers run first');
  await tick();
  assert.ok(/Surface-based duct/.test(popupText(s)) && popupText(s).includes(POSSIBLY), popupText(s));
  const p = s.stats.popups[0];
  assert.deepStrictEqual(p.latlng, { lat: 51.3, lng: -0.2 }, 'opens where clicked');
  assert.strictEqual(s.map._popup, p);
  assert.ok(!ev.originalEvent._stopped, 'event not stopped');
  // The other cell, a world copy of it, outside the area, a cell without data at this hour.
  click(s, 51.25, 0.25); await tick();
  assert.ok(/Elevated duct/.test(popupText(s)), popupText(s));
  assert.strictEqual(s.stats.popups.length, 1, 'one popup object, reused');
  s.map.closePopup();
  click(s, 51.25, 360.25); await tick();
  assert.ok(/Elevated duct/.test(popupText(s)), 'world copy: ' + popupText(s));
  s.map.closePopup();
  click(s, 52.5, 3); await tick();
  assert.strictEqual(popupText(s), null, 'outside the forecast area');
  const s0 = await mount(SAMPLE, T0);
  click(s0, 51.75, -0.25); await tick();
  assert.strictEqual(popupText(s0), null, 'class 0 at this hour: nothing to show');
  assert.strictEqual(s0.layer.openCellPopup({ lat: 51.25, lng: -0.25 }), true, 'openCellPopup');
  assert.ok(/Normal/.test(popupText(s0)));
});

test('click: no popup when it hit an interactive layer or another handler drew or opened something', async () => {
  const s = await mount(SAMPLE, T2);
  const interactive = { classList: { contains: (c) => c === 'leaflet-interactive' } };
  click(s, 51.25, -0.25, interactive); await tick();
  assert.strictEqual(popupText(s), null, 'on a path/marker (or a canvas hovering one)');
  // Another map click handler places something (Planner) ...
  const place = () => s.map.fire('layeradd', {});
  s.map.on('click', place);
  click(s, 51.25, -0.25); await tick();
  assert.strictEqual(popupText(s), null, 'the page used the click');
  s.map.off('click', place);
  // ... or a path opened its own popup before the map click (Live).
  const ev = { latlng: { lat: 51.25, lng: -0.25 }, originalEvent: { target: {} } };
  s.map.fire('preclick', ev);
  s.map.fire('popupopen', {});
  s.map.fire('click', ev);
  await tick();
  assert.strictEqual(popupText(s), null, 'a popup opened during the click');
  // Changes before the click do not count.
  s.map.fire('layeradd', {});
  click(s, 51.25, -0.25); await tick();
  assert.ok(/Surface-based duct/.test(popupText(s)), 'a plain click still opens it');
});

test('open popup follows the slider, closes where the hour has no data, and goes with the layer', async () => {
  const body = clone(SAMPLE);
  const s = await mount(body, T2);
  click(s, 51.8, -0.3); await tick();                      // cell c: class 1 at t2
  assert.ok(/^Normal/.test(popupText(s)), popupText(s));
  s.layer._setIndex(1);
  assert.ok(/Gradient, lowest 1.5 km: \u221236/.test(popupText(s)), 'text of hour 1: ' + popupText(s));
  s.layer._setIndex(0);
  assert.strictEqual(popupText(s), null, 'class 0 at hour 0: closed');
  s.layer._setIndex(2);
  click(s, 51.25, -0.25); await tick();
  assert.ok(/Surface-based/.test(popupText(s)));
  s.layer.onRemove(s.map);
  assert.strictEqual(popupText(s), null, 'closed with the layer');
  ['click', 'preclick', 'popupopen'].forEach(e => assert.strictEqual((s.map._on[e] || []).length, 0, e + ' listener left'));
  assert.ok(!(s.map._on.layeradd || []).length, 'layeradd listeners left');
  // A click queued just before the remove opens nothing afterwards.
  const s2 = await mount(SAMPLE, T2);
  click(s2, 51.25, -0.25);
  s2.layer.onRemove(s2.map);
  await tick();
  assert.strictEqual(s2.stats.popups.length, 0, 'no popup after remove');
});

// ── legend & states ─────────────────────────────────────────────────────────

test('legend: banded bar from the field variables, scale labels, LoRa row, attribution, note', async () => {
  const s = await mount(SAMPLE);
  const t = legendText(s);
  assert.ok(t.includes('Dashed outline: the model shows a duct that may carry LoRa at 868 / 915 MHz'), t);
  assert.ok(t.includes('Refractivity gradient over the lowest 1.5 km (N/km)'), t);
  assert.ok(!/-79|-157|Super-refraction|Ducting \(/.test(t), 'no g-based thresholds or labels: ' + t);
  assert.ok(t.includes('NOAA GFS') && t.includes('Open-Meteo.com (CC BY 4.0)'), t);
  assert.ok(/Updated /.test(t));
  assert.ok(t.includes('Indicator only: model levels are ~200 m apart, so thinner ducts are not visible.'));
  const box = s.map._controls[1]._container;
  const bar = find(box, n => n.className === 'mc-duct-scale-bar')[0];
  assert.ok(bar && /^linear-gradient\(to right, transparent 0%/.test(bar.style.background), bar && bar.style.background);
  for (let k = 1; k <= 8; k++) assert.ok(bar.style.background.includes(`var(--mc-duct-s${k})`), 's' + k);
  // Hard stops: each colour appears twice (start and end of its band), and
  // every band starts exactly where the previous one ends.
  const parts = bar.style.background.replace(/^linear-gradient\(to right, |\)$/g, '').split(/,\s*(?![^(]*\))/);
  const stops = parts.map((p) => { const m = /^(.*\S)\s+([\d.]+)%$/.exec(p.trim()); return { c: m[1], at: +m[2] }; });
  assert.strictEqual(stops.length, 18, 'transparent + 8 bands, two stops each: ' + parts.join(' | '));
  for (let k = 0; k < 18; k += 2) {
    assert.strictEqual(stops[k].c, stops[k + 1].c, 'band ' + k / 2 + ' has one colour');
    assert.ok(stops[k + 1].at > stops[k].at, 'band ' + k / 2 + ' has width');
    if (k) assert.strictEqual(stops[k].at, stops[k - 1].at, 'band ' + k / 2 + ' starts where the previous ends (no blend)');
  }
  assert.deepStrictEqual(stops.filter((_, i) => i % 2 === 0).map(x => x.c), ['transparent'].concat(STOP_N.map((_, k) => `var(--mc-duct-s${k + 1})`)));
  assert.strictEqual(stops[0].at, 0);
  assert.strictEqual(stops[17].at, 100);
  assert.ok(!/#[0-9a-f]{3,8}\b|rgb\(/i.test(bar.style.background), 'no literal colour in the bar');
  const labels = find(box, n => /mc-duct-scale-label\b/.test(n.className));
  assert.deepStrictEqual(labels.map(n => n.textContent), ['Normal', 'Above normal', 'High', 'Very high']);
  assert.ok(!/Enhanced|Strong/.test(t), 'old band words gone');
  const left = labels.map(n => parseFloat(n.style.left));
  assert.ok(left.every((v, i) => i === 0 || v > left[i - 1]), 'labels in order: ' + left);
  // "Above normal" marks where the colour starts (-44, band 1), "High" the -62 band (5).
  assert.strictEqual(left[0], 0);
  assert.strictEqual(left[1], stops[2].at, 'Above normal at the start of the first colour');
  assert.strictEqual(left[2], stops[10].at, 'High at the start of the -62 band');
  assert.strictEqual(left[3], 100);
  assert.ok(/mc-duct-scale-label-end/.test(labels[3].className), 'Very high right-aligned at the end');
  const sw = find(box, n => /mc-duct-swatch-/.test(n.className));
  assert.deepStrictEqual(sw.map(n => n.className), ['mc-duct-swatch mc-duct-swatch-lora', 'mc-duct-swatch mc-duct-swatch-edge']);
  assert.ok(s.map._controls.every(c => c.options.position === 'bottomleft'));
});

test('pending status shows the preparing message and draws nothing', async () => {
  const s = await mount({ status: 'pending' });
  const t = legendText(s);
  assert.ok(t.includes('Ducting forecast is being prepared'), t);
  assert.ok(t.includes('Indicator only'));
  assert.strictEqual(s.layer._cells.length, 0);
  assert.strictEqual(s.stats.polys.length, 0);
  assert.strictEqual(s.stats.fields.length, 0, 'no field image');
});

test('error status and fetch failure show unavailable, without error text', async () => {
  const s1 = await mount({ status: 'error', error: 'boom at db.go:12' });
  assert.ok(legendText(s1).includes('Ducting forecast unavailable'));
  assert.ok(!/boom|db\.go/.test(legendText(s1)));
  const s2 = makeSandbox();
  const layer = s2.ctx.MC_createDuctingLayer({});
  const map = makeMap();
  layer.onAdd(map);
  s2.api.calls[0].fail();
  await tick(); await tick();
  assert.ok(map._controls.map(c => c._container.textContent).join('').includes('Ducting forecast unavailable'));
});

test('malformed body counts as unavailable', async () => {
  const s = await mount({ status: 'ok', cells: 'nope' });
  assert.ok(legendText(s).includes('Ducting forecast unavailable'));
});

test('failed or pending refresh keeps the last good data drawn', async () => {
  const s = await mount(SAMPLE, T0);
  assert.strictEqual(s.timers.length, 1);
  assert.strictEqual(s.timers[0].ms, 30 * 60 * 1000);
  s.timers[0].fn();
  assert.strictEqual(s.api.calls.length, 2);
  s.api.calls[1].fail();
  await tick(); await tick();
  assert.strictEqual(s.layer._cells.length, 3);
  assert.deepStrictEqual(loraSet(s), ['51.25,0.25']);
  assert.ok(s.map._layers.includes(s.stats.fields[0]), 'field kept');
  assert.ok(!legendText(s).includes('unavailable'));
  s.timers[0].fn();
  s.api.calls[2].ok({ status: 'pending' });
  await tick(); await tick();
  assert.ok(!legendText(s).includes('being prepared'));
  assert.ok(legendText(s).includes('Updated '));
  assert.strictEqual(s.layer._group.layers.length, 2, 'the edge and LoRa outlines');
});

test('no data yet: retries every minute until data arrives, then every 30 minutes', async () => {
  const s = await mount({ status: 'pending' });
  assert.strictEqual(s.timers.length, 1);
  assert.strictEqual(s.timers[0].ms, 60 * 1000, 'pending -> retry in a minute');
  s.timers[0].fn();
  s.api.calls[1].fail();
  await tick(); await tick();
  assert.strictEqual(s.timers.length, 1, 'still one timer after a failure');
  assert.strictEqual(s.timers[0].ms, 60 * 1000);
  s.timers[0].fn();
  s.api.calls[2].ok(clone(SAMPLE));
  await tick(); await tick();
  assert.strictEqual(s.layer._cells.length, 3);
  assert.strictEqual(s.timers.length, 1);
  assert.strictEqual(s.timers[0].ms, 30 * 60 * 1000, 'back to the normal refresh');
  s.layer.onRemove(s.map);
  assert.strictEqual(s.timers.length, 0);
});

test('a refresh with a new forecast rebuilds and keeps a user-picked time', async () => {
  const s = await mount(SAMPLE, T1 + 60);
  s.layer._slider.value = '2';
  s.layer._slider.fire('input');
  s.timers[0].fn();
  const body = clone(SAMPLE);
  body.generatedAt = SAMPLE.generatedAt + 3600;
  s.api.calls[1].ok(body);
  await tick(); await tick();
  assert.strictEqual(s.layer._idx, 2);
  assert.strictEqual(s.layer._data.generatedAt, body.generatedAt);
  assert.strictEqual(s.stats.lines.length, 2, 'edge rebuilt');
  assert.strictEqual(s.layer._group.layers.length, 2, 'old edge and outline cleared, new ones added');
  assert.strictEqual(s.stats.fields.length, 1, 'same field layer');
  assert.strictEqual(s.stats.fields[0].setBoundsCalls, 0);
});

test('a refresh with the same generatedAt rebuilds nothing, keeps an open popup and clears the out-of-date notice', async () => {
  const realNow = Date.now;
  const s = await mount(SAMPLE, T1 + 60);
  try {
    Date.now = () => (T1 + 60) * 1000;
    click(s, 51.25, 0.25); await tick();
    const popup = s.stats.popups[0];
    assert.ok(popup.open);
    s.timers[0].fn();
    s.api.calls[1].fail();                                   // out of date ...
    await tick(); await tick();
    assert.ok(legendText(s).includes('out of date'));
    const data = s.layer._data, lines = s.stats.lines.length, polys = s.stats.polys.length, puts = s.layer._canvas.puts;
    s.timers[0].fn();
    s.api.calls[2].ok(clone(SAMPLE));                        // ... then the same forecast again
    await tick(); await tick();
    assert.ok(!legendText(s).includes('out of date'), 'notice cleared');
    assert.strictEqual(s.layer._data, data, 'held data kept');
    assert.strictEqual(s.stats.lines.length, lines, 'edge not rebuilt');
    assert.strictEqual(s.stats.polys.length, polys, 'outline not rebuilt');
    assert.strictEqual(s.layer._canvas.puts, puts, 'field not redrawn');
    assert.ok(popup.open && s.map._popup === popup, 'popup still open');
    // Same forecast later on: the hour moves on (no user pick), still no rebuild.
    Date.now = () => (T2 + 60) * 1000;
    s.timers[0].fn();
    s.api.calls[3].ok(clone(SAMPLE));
    await tick(); await tick();
    assert.strictEqual(s.layer._idx, 2, 'hour moved on');
    assert.strictEqual(s.stats.lines.length, lines, 'still not rebuilt');
    assert.ok(/Elevated duct/.test(popupText(s)) && /Base: 1200 m/.test(popupText(s)), 'popup shows the new hour: ' + popupText(s));
  } finally { Date.now = realNow; }
});

test('a refresh with a larger area moves the field bounds and resizes its canvas', async () => {
  const s = await mount(SAMPLE, T1 + 60);
  const body = clone(SAMPLE);
  body.cells.push({ lat: 52.75, lon: 1.25, c: [4, 4, 4], f: [300, 300, 300], b: [0, 0, 0], t: [90, 90, 90], dm: [80, 80, 80], g: [-400, -400, -400], n: [-95, -95, -95] });
  body.generatedAt += 1800;
  s.timers[0].fn();
  s.api.calls[1].ok(body);
  await tick(); await tick();
  const f = s.stats.fields[0];
  assert.strictEqual(s.stats.fields.length, 1);
  assert.strictEqual(f.setBoundsCalls, 1);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(f._bounds)).b, [[51, -0.5], [53, 1.5]]);
  assert.strictEqual(s.layer._canvas.width, 192);
  assert.ok(pixelAt(s, 52.75, 1.25).rgba[3] > 40, 'new cell drawn (outer-edge cell: half faded)');
});

// ── teardown ────────────────────────────────────────────────────────────────

test('remove during an in-flight fetch: nothing drawn afterwards, timer cleared, controls removed', async () => {
  const s = makeSandbox();
  const map = makeMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  const group = layer._group, ctls = map._controls.slice();
  assert.strictEqual(s.timers.length, 1);
  layer.onRemove(map);
  assert.strictEqual(s.timers.length, 0);
  assert.strictEqual(s.cleared.length, 1);
  assert.ok(ctls.every(c => c._removed) && map._controls.length === 0);
  assert.ok(!map._layers.includes(group));
  s.api.calls[0].ok(SAMPLE);
  await tick(); await tick();
  assert.strictEqual(layer._cells.length, 0);
  assert.strictEqual(s.stats.lines.length + s.stats.polys.length, 0, 'no outlines after remove');
  assert.strictEqual(s.stats.fields.length, 0, 'no field image after remove');
  assert.strictEqual(map._layers.length, 0);
});

test('remove after data: the field image layer leaves the map with the cells', async () => {
  const s = await mount(SAMPLE);
  const f = s.stats.fields[0];
  assert.ok(s.map._layers.includes(f));
  s.layer.onRemove(s.map);
  assert.ok(!s.map._layers.includes(f), 'field removed');
  assert.strictEqual(s.map._layers.length, 0, 'nothing left on the map');
  assert.strictEqual(s.layer._field, null);
  assert.strictEqual(s.layer._canvas, null);
  s.layer.onAdd(s.map);
  s.api.calls[1].ok(SAMPLE);
  await tick(); await tick();
  assert.strictEqual(s.stats.fields.length, 2, 're-add builds a fresh field');
  assert.strictEqual(s.map._layers.filter(l => l._mcField).length, 1, 'one field per map');
});

test('remove clears the slider listener; a late failure and a gone map never throw', async () => {
  const s = makeSandbox();
  const map = makeMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  const slider = layer._slider;
  map._gone = true;
  assert.doesNotThrow(() => layer.onRemove(map));
  assert.strictEqual((slider.listeners.input || []).length, 0);
  assert.doesNotThrow(() => layer.onRemove(map), 'double remove');
  s.api.calls[0].fail();
  await tick(); await tick();
});

test('re-adding after remove fetches again and ignores the first fetch', async () => {
  const s = makeSandbox();
  const layer = s.ctx.MC_createDuctingLayer({});
  const map = makeMap();
  layer.onAdd(map);
  layer.onRemove(map);
  layer.onAdd(map);
  s.api.calls[0].ok(SAMPLE);
  await tick(); await tick();
  assert.strictEqual(layer._cells.length, 0, 'stale generation ignored');
  assert.strictEqual(s.stats.lines.length, 0);
  s.api.calls[1].ok(SAMPLE);
  await tick(); await tick();
  assert.strictEqual(layer._cells.length, 3);
  assert.strictEqual(s.api.calls[0].url, '/api/ducting');
});

test('re-add then pending: cells and field are drawn from the kept data at once, and not called out of date', async () => {
  const realNow = Date.now;
  const s = await mount(SAMPLE, T0);
  try {
    Date.now = () => (T0 + 60) * 1000;
    s.layer.onRemove(s.map);
    s.stats.lines.length = 0; s.stats.fields.length = 0;
    s.layer.onAdd(s.map);
    assert.strictEqual(s.layer._cells.length, 3, 'cells listed before any answer');
    assert.strictEqual(s.stats.lines.length, 1, 'edge drawn before any answer');
    assert.ok(s.map._layers.some(l => l._mcField), 'field drawn before any answer');
    assert.ok(!legendText(s).includes('out of date'), 'nothing wrong yet');
    s.api.calls[s.api.calls.length - 1].ok({ status: 'pending' });
    await tick(); await tick();
    assert.strictEqual(s.layer._cells.length, 3);
    assert.ok(s.map._layers.some(l => l._mcField), 'field kept');
    assert.ok(!legendText(s).includes('out of date'), 'a restarting server answers pending: the held forecast is still valid');
  } finally { Date.now = realNow; }
});

test('re-add then fetch failure: the kept data stays drawn', async () => {
  const s = await mount(SAMPLE, T0);
  s.layer.onRemove(s.map);
  s.layer.onAdd(s.map);
  s.api.calls[s.api.calls.length - 1].fail();
  await tick(); await tick();
  assert.strictEqual(s.layer._cells.length, 3);
  assert.ok(s.map._layers.some(l => l._mcField));
  assert.ok(!legendText(s).includes('unavailable'));
  assert.ok(legendText(s).includes('out of date'), 'a failed request marks it out of date');
});

test('re-add then success: the kept data is replaced by the new data', async () => {
  const s = await mount(SAMPLE, T0);
  s.layer.onRemove(s.map);
  s.stats.lines.length = 0;
  s.layer.onAdd(s.map);
  assert.strictEqual(s.stats.lines.length, 1, 'kept data first');
  const body = clone(SAMPLE);
  body.generatedAt = T2;
  s.api.calls[s.api.calls.length - 1].ok(body);
  await tick(); await tick();
  assert.strictEqual(s.stats.lines.length, 2, 'rebuilt from the new answer');
  assert.strictEqual(s.layer._data.generatedAt, T2);
  assert.ok(s.layer._cells.every(c => body.cells.includes(c)), 'cells of the new answer');
});

test('re-add re-picks the hour; a user-picked hour is kept while it exists', async () => {
  const s = await mount(SAMPLE, T0);
  assert.strictEqual(s.layer._idx, 0);
  s.layer.onRemove(s.map);
  const realNow = Date.now;
  Date.now = () => (T2 + 5) * 1000;
  try {
    s.layer.onAdd(s.map);
    assert.strictEqual(s.layer._idx, 2, 'latest hour <= now, not the old one');
    s.layer._userPicked = true;
    s.layer._setIndex(1);
    s.layer.onRemove(s.map);
    s.layer.onAdd(s.map);
    assert.strictEqual(s.layer._idx, 1, 'picked hour kept');
  } finally { Date.now = realNow; }
});

test('out-of-date notice: on error and failure while old data is drawn, not on pending; gone on the next ok', async () => {
  const realNow = Date.now;
  const s = await mount(SAMPLE, T0);
  const MSG = 'Forecast out of date \u2014 showing the last one received';
  const refresh = async (body) => {
    s.timers[0].fn();
    const c = s.api.calls[s.api.calls.length - 1];
    if (body) c.ok(body); else c.fail();
    await tick(); await tick();
  };
  try {
    Date.now = () => (T0 + 60) * 1000;
    assert.ok(!legendText(s).includes('out of date'));
    await refresh({ status: 'pending' });
    assert.ok(!legendText(s).includes('out of date'), 'pending alone: still valid');
    for (const body of [{ status: 'error', error: 'forecast expired' }, { status: 'error', error: 'boom' }, null, { status: 'pending' }]) {
      await refresh(body);
      assert.ok(legendText(s).includes(MSG), JSON.stringify(body));
      assert.strictEqual(s.layer._cells.length, 3, 'old data still drawn');
      const status = find(s.map._controls[1]._container, n => n.className === 'mc-duct-legend-status');
      assert.strictEqual(status.length, 1, 'one status line');
    }
    const body = clone(SAMPLE); body.generatedAt += 60;
    await refresh(body);
    assert.ok(!legendText(s).includes('out of date'), 'cleared by ok');
  } finally { Date.now = realNow; }
});

test('out-of-date notice: shown once the held forecast has run out, whatever the server says', async () => {
  const realNow = Date.now;
  const s = await mount(SAMPLE, T0);
  try {
    const last = SAMPLE.times[SAMPLE.times.length - 1];
    Date.now = () => (last + 3599) * 1000;                  // inside the last hour: fine
    s.timers[0].fn();
    s.api.calls[s.api.calls.length - 1].ok({ status: 'pending' });
    await tick(); await tick();
    assert.ok(!legendText(s).includes('out of date'), 'last hour still running');
    Date.now = () => (last + 3600) * 1000;                   // the last hour has ended
    for (const body of [{ status: 'pending' }, clone(SAMPLE)]) {
      s.timers[0].fn();
      s.api.calls[s.api.calls.length - 1].ok(body);
      await tick(); await tick();
      assert.ok(legendText(s).includes('out of date'), 'run out, after ' + body.status);
    }
    const fresh = await mount(SAMPLE, last + 7200);          // first answer already run out
    assert.ok(legendText(fresh).includes('out of date'));
  } finally { Date.now = realNow; }
});

test('an ok answer re-picks the hour, keeping a user-picked one that is still there', async () => {
  const s = await mount(SAMPLE, T0);
  const realNow = Date.now;
  Date.now = () => (T2 + 5) * 1000;
  try {
    s.timers[0].fn();
    s.api.calls[s.api.calls.length - 1].ok(clone(SAMPLE));
    await tick(); await tick();
    assert.strictEqual(s.layer._idx, 2);
  } finally { Date.now = realNow; }
});

// ── legend text, compact legend ─────────────────────────────────────────────

test('legend shows only the attribution, not the model twice', async () => {
  const s = await mount(SAMPLE);
  const t = legendText(s);
  assert.strictEqual(t.split('NOAA GFS').length - 1, 1, 'model name once (inside the attribution)');
  assert.ok(t.includes('Weather data by Open-Meteo.com (CC BY 4.0), NOAA GFS'));
  assert.ok(!/NOAA GFS\. /.test(t));
});

test('legend is collapsible and starts collapsed on narrow screens', async () => {
  const s = makeSandbox();
  s.ctx.matchMedia = (q) => ({ matches: /max-width: 600px/.test(q) });
  const map = makeMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  s.api.calls[0].ok(SAMPLE);
  await tick(); await tick();
  const box = map._controls[1]._container;
  const head = () => find(box, n => n.tagName === 'button')[0];
  assert.strictEqual(head().attrs['aria-expanded'], 'false');
  assert.ok(!box.textContent.includes('Indicator only'), 'body hidden when compact');
  head().fire('click');
  assert.strictEqual(head().attrs['aria-expanded'], 'true');
  assert.ok(box.textContent.includes('Indicator only'));
});

test('bottom-left corner is lifted above the attribution bar, re-measured, and released', () => {
  const s = makeSandbox();
  const map = makeMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  assert.strictEqual(map.corner.style.marginBottom, '36px', 'two-line bar');
  map.attr.offsetHeight = 54;
  map.fire('resize');
  assert.strictEqual(map.corner.style.marginBottom, '54px', 'follows a taller bar');
  map.attr.rect = { left: 400 };            // short bar at the right: no overlap
  map.fire('layeradd');
  assert.strictEqual(map.corner.style.marginBottom, '', 'no lift without overlap');
  map.attr.rect = { left: 0 };
  map.fire('resize');
  assert.strictEqual(map.corner.style.marginBottom, '54px');
  layer.onRemove(map);
  assert.strictEqual(map.corner.style.marginBottom, '', 'released on remove');
  assert.strictEqual((map._on.resize || []).length, 0);
  assert.strictEqual((map._on.layeradd || []).length, 0);
});

// ── theme & missing variables ───────────────────────────────────────────────

test('missing variables: no stops -> transparent field, no LoRa variable -> no outline; no throw', async () => {
  const s = makeSandbox();
  for (let k = 1; k <= 8; k++) delete s.vars['--mc-duct-s' + k];
  delete s.vars['--mc-duct-lora'];
  delete s.vars['--mc-duct-alpha'];
  const map = makeMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  s.api.calls[0].ok(SAMPLE);
  await tick(); await tick();
  layer._setIndex(2);
  const d = layer._canvas.img.data;
  assert.ok(d.every(v => v === 0), 'nothing drawn without colours');
  // (-58 is the only stop below; with --mc-duct-alpha unset the opacity is the default.)
  assert.strictEqual(s.stats.polys.length, 0, 'no outline without --mc-duct-lora');
  assert.strictEqual(layer._lora, null);
  assert.ok(!/magenta|orange|gold|lightblue|#[0-9a-f]{6}\b/i.test(SRC.replace(/\/\*[\s\S]*?\*\/|\/\/.*$/gm, '')), 'no colour literals in map-ducting.js');
  s.vars['--mc-duct-s4'] = '#00a000';
  s.ctx.dispatch('theme-changed');
  assert.ok(layer._canvas.img.data.some(v => v !== 0), 'one stop is enough to draw; default opacity used');
});

test('theme-changed redraws the field and outlines from the new variables, same cells; inert after remove', async () => {
  const s = await mount(SAMPLE, T2);
  const cells = s.layer._cells.slice();
  const before = pixelAt(s, 51.25, -0.25).rgba;
  const puts = s.layer._canvas.puts;
  s.vars['--mc-duct-s4'] = '#ff00ff';
  s.vars['--mc-duct-s5'] = '#ff00ff';
  s.vars['--mc-duct-lora'] = '#fefefe';
  s.ctx.dispatch('theme-changed');
  assert.strictEqual(s.layer._canvas.puts, puts + 1);
  const after = pixelAt(s, 51.25, -0.25).rgba;
  assert.notDeepStrictEqual(after, before);
  assert.ok(after[0] > 240 && after[1] < 15 && after[2] > 240, 'magenta now: ' + after);
  assert.strictEqual(loraLayer(s).opts.color, '#fefefe');
  assert.deepStrictEqual(loraSet(s), ['51.25,-0.25']);
  assert.strictEqual(s.layer._group.layers.filter(l => l._mcLora).length, 1, 'old outline replaced, not added to');
  assert.ok(s.layer._cells.length === cells.length && s.layer._cells.every((c, i) => c === cells[i]), 'cells not rebuilt');
  assert.strictEqual(s.stats.fields.length, 1);
  assert.strictEqual((s.winL['theme-changed'] || []).length, 1);
  s.layer.onRemove(s.map);
  assert.strictEqual((s.winL['theme-changed'] || []).length, 0);
  const polys = s.stats.polys.length;
  s.vars['--mc-duct-lora'] = '#00ff00';
  assert.doesNotThrow(() => s.ctx.dispatch('theme-changed'));
  assert.strictEqual(s.stats.polys.length, polys, 'no redraw after remove');
});

// ── global grid mode (MC_DUCTING_CFG.source "gfs-grid") ────────────────────

const URLSTATE = fs.readFileSync(path.join(REPO_ROOT, 'public', 'url-state.js'), 'utf8');
const GMETA = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, 'tests', 'fixtures', 'ducting-grid-meta.json'), 'utf8'));
const GWIN = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, 'tests', 'fixtures', 'ducting-grid-window.json'), 'utf8'));
const GLINKS = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, 'tests', 'fixtures', 'ducting-links.json'), 'utf8'));
const GT = GMETA.times[8];            // the default time at now = GT + 1 h
const BASE_BBOX = '50,-2,54,2';       // the default view 51..53 x -1..1, padded 30 %, rounded out

// A sandbox in grid mode: url-state.js, fake setTimeout (flushed by hand),
// location/history, Node's AbortController and a Date whose now() is s.now.
function makeGridSandbox(hash) {
  const s = makeSandbox();
  const ctx = s.ctx;
  ctx.MC_DUCTING_CFG = { source: 'gfs-grid', historyDays: 7, attribution: 'Weather data: NOAA GFS (public domain)' };
  ctx.URLSearchParams = URLSearchParams;
  ctx.AbortController = AbortController;
  vm.runInContext(URLSTATE, ctx);
  s.now = GT + 3600;
  class FakeDate extends Date {}
  FakeDate.now = () => s.now * 1000;
  ctx.Date = FakeDate;
  const timeouts = [];
  let tid = 1000;
  ctx.setTimeout = (fn, ms) => { const id = tid++; timeouts.push({ id, fn, ms }); return id; };
  ctx.clearTimeout = (id) => { const i = timeouts.findIndex(t => t.id === id); if (i >= 0) timeouts.splice(i, 1); };
  s.timeouts = timeouts;
  s.flush = () => { while (timeouts.length) timeouts.shift().fn(); };
  s.replaced = [];
  ctx.location = { hash: hash == null ? '#/map' : hash };
  ctx.history = { state: null, replaceState(st, title, h) { s.replaced.push(h); ctx.location.hash = h; } };
  s.vars['--mc-duct-link'] = '#505050';
  s.vars['--mc-duct-link-duct'] = '#7c2d12';
  s.vars['--mc-duct-link-casing'] = 'rgba(255, 255, 255, 0.85)';
  return s;
}

// A map with a view box (degrees) and a flat projection: 100 px per degree.
function makeGridMap(view) {
  const m = makeMap();
  m.view = Object.assign({ s: 51, w: -1, n: 53, e: 1 }, view);
  m.getBounds = () => ({ getSouth: () => m.view.s, getWest: () => m.view.w, getNorth: () => m.view.n, getEast: () => m.view.e });
  m.latLngToContainerPoint = (ll) => {
    const lat = Array.isArray(ll) ? ll[0] : ll.lat, lng = Array.isArray(ll) ? ll[1] : ll.lng;
    return { x: (lng - m.view.w) * 100, y: (m.view.n - lat) * 100 };
  };
  m.move = (v) => { m.view = Object.assign({}, v); m.fire('moveend'); };
  return m;
}

const callsTo = (s, p) => s.api.calls.filter(c => c.url.indexOf(p) === 0);
const lastCall = (s, p) => { const c = callsTo(s, p); return c[c.length - 1]; };
const settle = async () => { await tick(); await tick(); };
const openPopup = (s) => s.stats.popups.find(p => p.open) || null;
const linkLines = (s) => (s.layer._linkGroup ? s.layer._linkGroup.layers : []);
const slide = (s, i) => { s.layer._slider.value = String(i); s.layer._slider.fire('input'); };

// Adds a grid-mode layer and answers meta, links and the first window
// (each unless false; a value replaces the fixture).
async function mountGrid(opts) {
  opts = opts || {};
  const s = makeGridSandbox(opts.hash);
  if (opts.now != null) s.now = opts.now;
  const map = makeGridMap(opts.view);
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  Object.assign(s, { map, layer });
  if (opts.meta !== false) { callsTo(s, '/api/ducting/meta')[0].ok(opts.meta || clone(GMETA)); await settle(); }
  if (opts.links !== false) { callsTo(s, '/api/ducting/links')[0].ok(opts.links || clone(GLINKS)); await settle(); }
  if (opts.meta !== false && opts.window !== false) {
    s.flush();
    lastCall(s, '/api/ducting/grid').ok(opts.window || clone(GWIN));
    await settle();
  }
  return s;
}

test('grid: MC_ductWindowBox pads 30 %, rounds outward to whole degrees, clamps', () => {
  const { ctx } = makeGridSandbox();
  const b = (...a) => JSON.parse(JSON.stringify(ctx.MC_ductWindowBox(...a)));
  assert.deepStrictEqual(b(51, -1, 53, 1), { s: 50, w: -2, n: 54, e: 2, shift: 0 });
  assert.deepStrictEqual(b(51.3, -0.2, 51.7, 0.4, 0), { s: 51, w: -1, n: 52, e: 1, shift: 0 }, 'no padding: still whole degrees outward');
  assert.deepStrictEqual(b(-80, 150, 80, 175), { s: -90, w: 142, n: 90, e: 180 - 0, shift: 0 }, 'latitude and longitude clamped');
  assert.deepStrictEqual(b(-60, -200, 70, 200), { s: -90, w: -180, n: 90, e: 180, shift: 0 }, 'whole world');
  assert.deepStrictEqual(b(10, -100, 20, 100), { s: 7, w: -160, n: 23, e: 160, shift: 0 }, '320 deg padded: clamped, not whole');
  assert.deepStrictEqual(b(10, -130, 20, 130), { s: 7, w: -180, n: 23, e: 180, shift: 0 }, 'padded span over 360 -> whole range');
  // A view on the next world copy asks for the -180..180 box and remembers the shift.
  assert.deepStrictEqual(b(51, 359, 53, 361), { s: 50, w: -2, n: 54, e: 2, shift: 360 });
  assert.deepStrictEqual(b(51, -721, 53, -719), { s: 50, w: -2, n: 54, e: 2, shift: -720 });
  assert.strictEqual(ctx.MC_ductWindowBox(51, 1, 53, -1), null, 'empty view');
  assert.strictEqual(ctx.MC_ductWindowBox(NaN, 0, 1, 1), null);
});

test('grid: MC_ductWindowStep mirrors the server ladder (<= 4000 cells); MC_ductBoxCovers', () => {
  const { ctx } = makeGridSandbox();
  const st = (s, w, n, e) => ctx.MC_ductWindowStep({ s, w, n, e }, 0.5);
  assert.strictEqual(st(50, -2, 54, 2), 0.5);                 // 8 x 8 cells
  assert.strictEqual(st(30, -30, 60, 30), 1);                 // 60 x 120 = 7200 at 0.5 -> 30 x 60 at 1
  assert.strictEqual(st(-90, -180, 90, 180), 8, 'whole world: 4 deg is 45 x 90 = 4050 cells, too many');
  assert.strictEqual(st(-60, -180, 60, 180), 4);
  assert.ok(ctx.MC_ductBoxCovers({ s: 50, w: -2, n: 54, e: 2 }, { s: 51, w: -1, n: 53, e: 1 }));
  assert.ok(ctx.MC_ductBoxCovers({ s: 50, w: -2, n: 54, e: 2 }, { s: 50, w: -2, n: 54, e: 2 }));
  assert.ok(!ctx.MC_ductBoxCovers({ s: 50, w: -2, n: 54, e: 2 }, { s: 51, w: -1, n: 54.5, e: 1 }));
  assert.ok(!ctx.MC_ductBoxCovers(null, { s: 0, w: 0, n: 1, e: 1 }));
});

test('grid: MC_ductLru keeps the most recently used, evicts the oldest', () => {
  const { ctx } = makeGridSandbox();
  const lru = ctx.MC_ductLru(3);
  lru.set('a', 1); lru.set('b', 2); lru.set('c', 3);
  assert.strictEqual(lru.get('a'), 1, 'a is now the newest');
  lru.set('d', 4);
  assert.strictEqual(lru.size, 3);
  assert.ok(!lru.has('b'), 'b was the least recently used');
  assert.ok(lru.has('a') && lru.has('c') && lru.has('d'));
  lru.set('c', 30);
  assert.strictEqual(lru.size, 3);
  assert.strictEqual(lru.get('c'), 30);
  assert.strictEqual(lru.get('zz'), undefined);
  lru.clear();
  assert.strictEqual(lru.size, 0);
});

test('grid: MC_ductTimeLabel says forecast ahead of now, analysis and how long ago before it', () => {
  const { ctx } = makeGridSandbox();
  const lab = (t, now) => ctx.MC_ductTimeLabel(t, now).split(' · ')[1];
  assert.strictEqual(lab(GT + 1, GT), 'forecast');
  assert.strictEqual(lab(GT, GT), 'analysis, under 1 h ago');
  assert.strictEqual(lab(GT, GT + 1799), 'analysis, under 1 h ago');
  assert.strictEqual(lab(GT, GT + 6 * 3600), 'analysis, 6 h ago');
  assert.strictEqual(lab(GT, GT + 47 * 3600), 'analysis, 47 h ago');
  assert.strictEqual(lab(GT, GT + 3 * 86400), 'analysis, 3 d ago');
  assert.ok(ctx.MC_ductTimeLabel(GT, GT).indexOf(' · ') > 0, 'the time comes first');
});

test('grid: MC_ductHashTime / MC_ductHashWith read and write ductT on the map page only', () => {
  const { ctx } = makeGridSandbox();
  assert.strictEqual(ctx.MC_ductHashTime('#/map?ductT=' + GT), GT);
  assert.strictEqual(ctx.MC_ductHashTime('#/map?lat=1&lon=2&ductT=' + GT + '&duct=1'), GT);
  assert.strictEqual(ctx.MC_ductHashTime('#/live?ductT=' + GT), null, 'other pages are not read');
  assert.strictEqual(ctx.MC_ductHashTime('#/map?ductT=12.5'), null);
  assert.strictEqual(ctx.MC_ductHashTime('#/map?ductT=-3'), null);
  assert.strictEqual(ctx.MC_ductHashTime('#/map?ductT=abc'), null);
  assert.strictEqual(ctx.MC_ductHashTime('#/map'), null);
  assert.strictEqual(ctx.MC_ductHashWith('#/map?lat=1&lon=2', { duct: '1', ductT: String(GT) }), '#/map?lat=1&lon=2&duct=1&ductT=' + GT);
  assert.strictEqual(ctx.MC_ductHashWith('#/map?duct=1&ductT=5', { ductT: null }), '#/map?duct=1');
  assert.strictEqual(ctx.MC_ductHashWith('#/packets?x=1', { ductT: '5' }), null, 'never written off the map page');
});

test('grid: MC_ductLinksAt keeps links heard within +-90 min, nearest reception first; MC_ductLinkKind', () => {
  const { ctx } = makeGridSandbox();
  const items = JSON.parse(JSON.stringify(ctx.MC_ductLinksAt(GLINKS.links, GT, 5400)));
  assert.deepStrictEqual(items.map(i => i.link.a.pubkey + ':' + i.kind), ['aa11:duct', 'cc33:plain', 'ee55:nodata'],
    'T+5400 s is in, T+5401 s is out');
  assert.deepStrictEqual(clone(ctx.MC_ductLinksAt(GLINKS.links, GT - 20000, 5400).map(i => i.kind)), ['plain'], 'the other reception of aa11');
  const two = [{ t: GT + 100, meanGradNkm: -40, highFrac: 0.1 }, { t: GT - 50, meanGradNkm: -60, highFrac: 0.9 }, { t: GT + 50, meanGradNkm: -50, highFrac: 0.6 }];
  const link = { a: { lat: 1, lon: 1 }, b: { lat: 2, lon: 2 }, receptions: two };
  const r = ctx.MC_ductLinksAt([link], GT, 5400)[0];
  assert.deepStrictEqual(clone(r.recs.map(x => x.t)), [GT - 50, GT + 50, GT + 100], 'nearest first, earlier on a tie');
  assert.strictEqual(r.kind, 'duct', 'the colour follows the nearest reception');
  // Emphasized: half or more of the path in the High band or stronger (highFrac);
  // neither the class-based super-refractive share nor the LoRa-duct share decides it.
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: -50, highFrac: 0.5 }), 'duct');
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: -50, highFrac: 0.4999, superFrac: 1, ductFrac: 1 }), 'plain', 'saturated class share or a LoRa duct is not enough');
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: -70, highFrac: 0.9, superFrac: 0, ductFrac: 0 }), 'duct');
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: -30, highFrac: 0 }), 'plain');
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: -30, highFrac: -1 }), 'plain', 'gradient without a High share: plain');
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: -30, superFrac: 0.9 }), 'plain', 'no highFrac (older server): plain');
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: null, highFrac: 0.9 }), 'nodata', 'no path gradient: dashed');
  assert.strictEqual(ctx.MC_ductLinkKind({ highFrac: 0.9 }), 'nodata');
  assert.strictEqual(ctx.MC_ductLinkKind({ meanGradNkm: 'x', highFrac: 0.9 }), 'nodata');
  assert.strictEqual(ctx.MC_ductLinkKind({}), 'nodata');
  assert.strictEqual(ctx.MC_ductLinkKind(null), 'nodata');
  assert.strictEqual(ctx.MC_ductLinkKind(GLINKS.links[1].receptions[0]), 'plain', 'fixture cc33: super-refractive 95%, High 20%');
  const bad = [{ a: { lat: 'x', lon: 0 }, b: { lat: 1, lon: 1 }, receptions: [{ t: GT, ductFrac: 1 }] }, null, { a: {}, b: {} }];
  assert.strictEqual(ctx.MC_ductLinksAt(bad, GT, 5400).length, 0, 'links without positions are skipped');
});

test('grid: MC_ductSegDist is the distance to the segment, clamped at its ends', () => {
  const { ctx } = makeGridSandbox();
  const d = ctx.MC_ductSegDist;
  assert.strictEqual(d({ x: 5, y: 3 }, { x: 0, y: 0 }, { x: 10, y: 0 }), 3);
  assert.strictEqual(d({ x: -4, y: 3 }, { x: 0, y: 0 }, { x: 10, y: 0 }), 5, 'past the start: distance to the end point');
  assert.strictEqual(d({ x: 13, y: 4 }, { x: 0, y: 0 }, { x: 10, y: 0 }), 5);
  assert.strictEqual(d({ x: 3, y: 4 }, { x: 0, y: 0 }, { x: 0, y: 0 }), 5, 'zero-length segment');
});

const FULL = { links: 56, receptions: 1234, withGrid: 1200, duringDuct: 0, baselineDuctFrac: 0.01, unverifiedPairs: 0,
  recvMeanGradNkm: -58.4, baselineMeanGradNkm: -49.2, recvEnhancedFrac: 0.41, baselineEnhancedFrac: 0.22 };
const LEAD = 'Last 7 days: 1,234 receptions on 56 node pairs without terrain line of sight.';
const GRAD = 'Along their paths the modelled refractivity gradient averaged −58 N/km when heard, against −49 N/km for the same paths over the whole period';
const ENH = 'half or more of the path was in the High band or stronger for 41% of receptions vs 22% overall';

test('grid: losMethod is read from the summary (where the server sends it) or the top level', () => {
  const { ctx } = makeGridSandbox();
  assert.strictEqual(ctx.MC_ductLinksMethod({ summary: { losMethod: 'terrain' } }), 'terrain');
  assert.strictEqual(ctx.MC_ductLinksMethod({ losMethod: 'smooth-earth', summary: {} }), 'smooth-earth');
  assert.strictEqual(ctx.MC_ductLinksMethod({}), '');
  assert.strictEqual(ctx.MC_ductLinksMethod(null), '');
});

test('grid: MC_ductLinksSummary leads with the gradient comparison; says only what the numbers say', () => {
  const { ctx } = makeGridSandbox();
  const f = (sum, days, m) => ctx.MC_ductLinksSummary(Object.assign({}, FULL, sum), days == null ? 7 : days, m == null ? 'terrain' : m);
  assert.strictEqual(f({}), LEAD + ' ' + GRAD + '; ' + ENH + '.', 'no LoRa duct: no LoRa sentence');
  assert.strictEqual(f({}, 7, 'smooth-earth'), LEAD.replace('without terrain line of sight', 'beyond the radio horizon') + ' ' + GRAD + '; ' + ENH + '.');
  assert.strictEqual(f({}, 7, ''), f({}, 7, 'smooth-earth'), 'unknown method: the smooth-earth wording');
  assert.strictEqual(f({ duringDuct: 3, unverifiedPairs: 12 }),
    LEAD + ' ' + GRAD + '; ' + ENH + '. Half or more of the path was in a modelled duct that may carry LoRa for 3 receptions. 12 more pairs not checked yet.');
  assert.strictEqual(f({ duringDuct: 1, unverifiedPairs: 1 }).slice(-96),
    'Half or more of the path was in a modelled duct that may carry LoRa for 1 reception. 1 more pair not checked yet.'.slice(-96));
  // Null values drop their comparison, never invent one.
  assert.strictEqual(f({ baselineMeanGradNkm: null }),
    LEAD + ' Along their paths the modelled refractivity gradient averaged −58 N/km when heard; ' + ENH + '.');
  assert.strictEqual(f({ baselineEnhancedFrac: null }), LEAD + ' ' + GRAD + '; half or more of the path was in the High band or stronger for 41% of receptions.');
  assert.strictEqual(f({ recvMeanGradNkm: null, baselineMeanGradNkm: null }),
    LEAD + ' Half or more of the path was in the High band or stronger for 41% of receptions vs 22% overall.');
  assert.strictEqual(f({ recvEnhancedFrac: null }), LEAD + ' ' + GRAD + '.');
  assert.strictEqual(f({ recvMeanGradNkm: null, recvEnhancedFrac: null, baselineEnhancedFrac: -1 }), LEAD, 'nothing to compare');
  assert.strictEqual(f({ recvMeanGradNkm: undefined, recvEnhancedFrac: undefined }), LEAD, 'fields missing (older server)');
  assert.strictEqual(f({ links: 1, receptions: 1 }, 1).slice(0, 72), 'Last 24 hours: 1 reception on 1 node pair without terrain line of sight.');
  const lead50 = LEAD.replace('56 node pairs', 'the 50 longest node pairs');
  assert.strictEqual(f({ listedPairs: 50 }).slice(0, lead50.length), lead50,
    'fewer listed than all pairs: the longest ones are named');
  assert.ok(f({ listedPairs: 56 }).indexOf(LEAD) === 0, 'all listed: the total');
  assert.ok(f({ listedPairs: 500 }).indexOf(LEAD) === 0, 'cap above the total: the total');
  assert.ok(f({ listedPairs: null }).indexOf(LEAD) === 0 && f({ listedPairs: 0 }).indexOf(LEAD) === 0, 'absent or 0: the total');
  assert.ok(f({ links: 900, listedPairs: 1 }).indexOf('on the 1 longest node pair without') > 0);
  assert.strictEqual(f({ links: 0, receptions: 0, unverifiedPairs: 2 }, 3),
    'Last 3 days: no receptions between node pairs without terrain line of sight. 2 more pairs not checked yet.');
  assert.strictEqual(f({ recvMeanGradNkm: 12.4, baselineMeanGradNkm: 0 }).indexOf('averaged 12 N/km when heard, against 0 N/km') > 0, true, 'positive and zero plain');
  assert.strictEqual(ctx.MC_ductLinksSummary(GLINKS.summary, 7, 'terrain'), ctxSummary() + ' ' +
    GRAD + '; ' + ENH + '. Half or more of the path was in a modelled duct that may carry LoRa for 2 receptions. 3 more pairs not checked yet.');
});

test('grid: link popup shows node names as text only (a hostile name stays inert), heights, receptions', () => {
  const { ctx } = makeGridSandbox();
  const item = ctx.MC_ductLinksAt(GLINKS.links, GT, 5400)[0];
  const box = ctx.MC_ductLinkPopup(item, 869.525, 'terrain');
  const text = box.textContent;
  assert.ok(text.indexOf('Sent by: Hill Top') >= 0);
  assert.ok(text.indexOf('Heard by: <img src=x onerror=alert(1)>') >= 0, 'the name is shown literally');
  assert.strictEqual(find(box, n => /^img$/i.test(n.tagName || '')).length, 0, 'no element made from the name');
  assert.ok(!('innerHTML' in box) || box.innerHTML === undefined);
  assert.ok(text.indexOf('190 km apart; no terrain line of sight (smooth-earth radio horizon 59 km)') >= 0, text);
  assert.ok(ctx.MC_ductLinkPopup(item, 869.525, 'smooth-earth').textContent.indexOf('190 km apart; radio horizon 59 km') >= 0);
  assert.ok(text.indexOf('Antenna heights: sender 12 m, receiver 10 m (site default)') >= 0);
  assert.ok(/ · heard directly by the observer · SNR −5\.5 dB · RSSI −118 dBm · path gradient −62 N\/km · High band or stronger along 75% of the path · super-refractive along 80% · LoRa duct along 80%$/m.test(text), text);
  assert.ok(text.indexOf('Heard 3 times in the period') >= 0);
  assert.ok(text.indexOf('LoRa duct: a modelled duct that may carry 869.525 MHz.') >= 0, 'explained when a LoRa share is shown');
  // No LoRa-duct share: not listed, not explained.
  const late = ctx.MC_ductLinksAt(GLINKS.links, GT + 5401, 0)[0];
  const lt = ctx.MC_ductLinkPopup(late, 869.525, 'terrain').textContent;
  assert.ok(lt.indexOf('path gradient \u221270 N/km \u00b7 High band or stronger along 90% of the path \u00b7 super-refractive along 90%') >= 0, lt);
  assert.ok(lt.indexOf('High band: \u221262 N/km or stronger, as in the legend.') >= 0, 'the popup says what the High band is');
  assert.ok(lt.indexOf('LoRa duct') < 0, lt);
  // No model data, no name (key prefix), a very long name capped.
  const nodata = ctx.MC_ductLinksAt(GLINKS.links, GT + 5400, 0)[0];
  assert.ok(ctx.MC_ductLinkPopup(nodata, 869.525).textContent.indexOf('no model data for this time') >= 0);
  assert.ok(ctx.MC_ductLinkPopup(nodata, 869.525).textContent.indexOf('Sent by: ee55…') >= 0);
  const long = clone(GLINKS.links[0]);
  long.a.name = 'x'.repeat(500);
  const t = ctx.MC_ductLinkPopup({ link: long, recs: long.receptions, kind: 'duct' }, 0).textContent;
  assert.ok(t.indexOf('x'.repeat(63) + '…') >= 0 && t.indexOf('x'.repeat(64)) < 0, 'name capped at 64 characters');
});

test('grid: link popup says how each reception was heard; a relay reception shows no SNR/RSSI', () => {
  const { ctx } = makeGridSandbox();
  // aa11's older reception (T - 20000 s) and cc33's only one are relay receptions without SNR/RSSI.
  const relay = ctx.MC_ductLinksAt(GLINKS.links, GT - 20000, 0)[0];
  const lines = find(ctx.MC_ductLinkPopup(relay, 869.525), n => n.tagName === 'li').map(n => n.textContent);
  assert.strictEqual(lines.length, 1);
  assert.ok(/ · relayed: the second repeater heard the first directly · path gradient −46 N\/km · High band or stronger along 10% of the path · super-refractive along 20% · LoRa duct along 10%$/.test(lines[0]), lines[0]);
  assert.ok(lines[0].indexOf('SNR') < 0 && lines[0].indexOf('RSSI') < 0, 'absent values are left out');
  const cc = ctx.MC_ductLinksAt(GLINKS.links, GT - 1800, 0)[0];
  assert.strictEqual(cc.link.a.pubkey, 'cc33');
  const t = ctx.MC_ductLinkPopup(cc, 869.525).textContent;
  assert.ok(t.indexOf('relayed: the second repeater heard the first directly') >= 0 && t.indexOf('SNR') < 0);
  // Both kinds in one list, nearest first; an unknown or missing via says nothing about it.
  const mixed = { link: GLINKS.links[0], recs: [
    { t: GT, via: 'observer', snr: 3, ductFrac: 0.6 }, { t: GT + 60, via: 'relay', ductFrac: 0.2 },
    { t: GT + 120, via: '<b>x</b>', rssi: -100, ductFrac: 0 }, { t: GT + 180, ductFrac: -1 }], kind: 'duct' };
  const li = find(ctx.MC_ductLinkPopup(mixed, 869.525), n => n.tagName === 'li').map(n => n.textContent);
  assert.ok(li[0].indexOf('heard directly by the observer · SNR 3.0 dB') > 0, li[0]);
  assert.ok(li[1].indexOf('relayed: the second repeater heard the first directly') > 0 && li[1].indexOf('SNR') < 0);
  assert.ok(li[2].indexOf('<b>x</b>') < 0 && li[2].indexOf('heard directly') < 0 && li[2].indexOf('relayed') < 0, li[2]);
  assert.ok(li[3].indexOf('heard directly') < 0 && li[3].indexOf('relayed') < 0 && li[3].indexOf('no model data') > 0, li[3]);
});

const gradRows = (box) => find(box, n => n.tagName === 'tr').map(tr => tr.children.map(c => c.textContent.replace(/\n/g, ' ')));

test('grid: MC_ductGradientTable: semantic table of the heard share by path gradient, all and intermittent pairs', () => {
  const { ctx } = makeGridSandbox();
  const box = ctx.MC_ductGradientTable(GLINKS.summary);
  const table = find(box, n => n.tagName === 'table')[0];
  assert.ok(table, 'a real <table>');
  assert.strictEqual(find(table, n => n.tagName === 'caption')[0].textContent, 'How often the pairs were heard, by path gradient');
  const ths = find(table, n => n.tagName === 'th');
  assert.ok(ths.length === 3 + 4 && ths.every(th => th.attrs.scope === (ths.indexOf(th) < 3 ? 'col' : 'row')), 'column and row headers carry scope');
  assert.deepStrictEqual(gradRows(box), [
    ['Path gradient (N/km)', 'All pairs', 'Intermittent pairs (2)'],
    ['weaker than −40', '8% n=1,200', '3% n=310'],
    ['−40 to −50', '10% n=2,410', '7% n=402'],
    ['−50 to −62', '15% n=830', '—'],
    ['−62 or stronger (High)', '32% n=160', '34% n=44'],
  ]);
  const t = box.textContent;
  assert.ok(t.indexOf('If ducting matters, the share should rise towards the High band, most clearly for intermittent pairs.') >= 0);
  assert.strictEqual(find(box, n => n.tagName === 'td').filter(td => td.textContent === '—').length, 1, 'pathSteps 0: a dash, no n');
});

test('grid: MC_ductGradientTable guards: no byGradient -> null; no or mismatched intermittent bins -> one column; bad values', () => {
  const { ctx } = makeGridSandbox();
  assert.strictEqual(ctx.MC_ductGradientTable({}), null, 'older server');
  assert.strictEqual(ctx.MC_ductGradientTable(null), null);
  assert.strictEqual(ctx.MC_ductGradientTable({ byGradient: [] }), null);
  assert.strictEqual(ctx.MC_ductGradientTable({ byGradient: 'x' }), null);
  assert.strictEqual(ctx.MC_ductGradientTable({ byGradient: [null] }), null);
  const only = ctx.MC_ductGradientTable({ byGradient: GLINKS.summary.byGradient });
  assert.deepStrictEqual(gradRows(only)[0], ['Path gradient (N/km)', 'All pairs']);
  const mism = ctx.MC_ductGradientTable({ byGradient: GLINKS.summary.byGradient, byGradientIntermittent: GLINKS.summary.byGradientIntermittent.slice(1), intermittentPairs: 2 });
  assert.strictEqual(gradRows(mism)[0].length, 2, 'bins that do not line up: no intermittent column');
  const odd = ctx.MC_ductGradientTable({ byGradient: [{ fromNkm: null, toNkm: -40, pathSteps: 'x', heardSteps: 3 }, { fromNkm: -62, toNkm: null, pathSteps: 10, heardSteps: 12 }] });
  assert.deepStrictEqual(gradRows(odd).slice(1).map(r => r[1]), ['—', '100% n=10'], 'non-numeric steps: a dash; heard capped at path');
  assert.strictEqual(ctx.MC_ductGradBinLabel({ fromNkm: -50, toNkm: null }), '−50 or stronger', 'only the High band says High');
  assert.strictEqual(ctx.MC_ductGradBinLabel({ fromNkm: null, toNkm: null }), 'all');
  assert.strictEqual(ctx.MC_ductGradBinLabel({}), 'all');
});

test('grid legend: the gradient table shows only when the long-link section is open', async () => {
  const s = await mountGrid();
  assert.strictEqual(linksToggle(s).attrs['aria-expanded'], 'true');
  assert.ok(find(legendBox(s), n => n.tagName === 'table').length === 1, 'open: table');
  const t = legendText(s);
  assert.ok(t.indexOf('How often the pairs were heard, by path gradient') < t.indexOf('Positions are self-reported'), 'above the caveat');
  linksToggle(s).fire('click');
  assert.strictEqual(find(legendBox(s), n => n.tagName === 'table').length, 0, 'folded: no table');
  assert.ok(legendText(s).indexOf('If ducting matters') < 0);
  const old = clone(GLINKS);
  delete old.summary.byGradient; delete old.summary.byGradientIntermittent; delete old.summary.intermittentPairs;
  const o = await mountGrid({ links: old });
  assert.strictEqual(find(legendBox(o), n => n.tagName === 'table').length, 0, 'older server: no table, no throw');
  assert.ok(legendText(o).indexOf(ctxSummary()) >= 0);
});

test('grid: on add fetches meta and links (historyDays), not /api/ducting; default time = latest <= now', async () => {
  const s = await mountGrid({ window: false });
  assert.strictEqual(callsTo(s, '/api/ducting/meta').length, 1);
  assert.deepStrictEqual(callsTo(s, '/api/ducting/links').map(c => c.url), ['/api/ducting/links?days=7']);
  assert.strictEqual(s.api.calls.filter(c => c.url === '/api/ducting').length, 0, 'never the Open-Meteo snapshot');
  assert.strictEqual(s.layer._sel, 8);
  assert.strictEqual(s.layer._slider.max, String(GMETA.times.length - 1));
  assert.strictEqual(s.layer._slider.disabled, false);
  assert.ok(/^Time: .* · analysis, 1 h ago$/.test(s.layer._timeLabel.textContent), s.layer._timeLabel.textContent);
  assert.strictEqual(s.layer._slider.attrs['aria-valuetext'], s.layer._timeLabel.textContent.slice(6));
  // Every 10 minutes while ok.
  assert.deepStrictEqual(s.timers.map(t => t.ms), [600000]);
});

test('grid: the window for view + time is fetched once after the debounce, bbox padded and rounded', async () => {
  const s = await mountGrid({ window: false });
  assert.strictEqual(callsTo(s, '/api/ducting/grid').length, 0, 'debounced');
  assert.deepStrictEqual(s.timeouts.map(t => t.ms), [250]);
  s.map.fire('moveend'); s.map.fire('zoomend');
  assert.strictEqual(s.timeouts.length, 1, 'rapid events coalesce');
  s.flush();
  assert.deepStrictEqual(callsTo(s, '/api/ducting/grid').map(c => c.url), ['/api/ducting/grid?t=' + GT + '&bbox=' + BASE_BBOX + '&run=' + GMETA.runTime]);
  lastCall(s, '/api/ducting/grid').ok(clone(GWIN));
  await settle();
  assert.ok(s.layer._field && s.map._layers.indexOf(s.layer._field) >= 0, 'field drawn');
  assert.strictEqual(s.stats.lines.filter(l => l._mcEdge).length, 0, 'no forecast-area edge line in grid mode');
  assert.deepStrictEqual(loraSet(s), ['52.25,0.25'], 'LoRa outline from the window');
  assert.strictEqual(s.layer._idx, 0, 'a window holds one time');
});

test('grid: click lookup and popup work on the window lattice', async () => {
  const s = await mountGrid();
  click(s, 52.05, 0.45);                // the LoRa cell (52.25, 0.25), 25 px from the nearest link
  await tick();
  const p = openPopup(s);
  assert.ok(p && p.content.textContent.indexOf('Surface-based duct') >= 0, p && p.content.textContent);
  assert.ok(s.layer.openCellPopup({ lat: 52.75, lng: -0.75 }));
  assert.ok(!s.layer.openCellPopup({ lat: 45, lng: 10 }), 'outside the window');
});

test('grid: legend has no edge row, says what past times are, and the long-link section', async () => {
  const s = await mountGrid();
  const t = legendText(s);
  assert.ok(t.indexOf('edge of the forecast area') < 0, 'no edge row: coverage is global');
  assert.ok(t.indexOf('Past times show the model’s analysis or short-range forecast for that time, not measurements.') >= 0);
  assert.ok(t.indexOf('Observed long links') >= 0);
  assert.ok(t.indexOf(ctxSummary()) >= 0, t);
  assert.ok(t.indexOf('On the map: 3 links heard within 90 min of the selected time. Click one for details.') >= 0, t);
  assert.ok(t.indexOf('Positions are self-reported; a long link can also come from a wrong position.') >= 0);
  assert.ok(t.indexOf('Weather data: NOAA GFS (public domain)') >= 0, 'attribution from meta');
});
function ctxSummary() { return 'Last 7 days: 6 receptions on 4 node pairs without terrain line of sight.'; }

test('grid: panning inside the drawn window fetches nothing; leaving it fetches the new box', async () => {
  const s = await mountGrid();
  const n0 = callsTo(s, '/api/ducting/grid').length;
  const canvasPuts = s.layer._canvas.puts;
  s.map.move({ s: 51.4, w: -0.6, n: 53.4, e: 1.4 });
  assert.strictEqual(s.timeouts.length, 0, 'covered at the same step: no fetch queued');
  assert.strictEqual(s.layer._canvas.puts, canvasPuts, 'and no redraw');
  s.map.move({ s: 40, w: 10, n: 42, e: 12 });
  s.flush();
  assert.strictEqual(callsTo(s, '/api/ducting/grid').length, n0 + 1);
  assert.strictEqual(lastCall(s, '/api/ducting/grid').url, '/api/ducting/grid?t=' + GT + '&bbox=39,9,43,13' + '&run=' + GMETA.runTime);
  // Zooming far out inside the drawn box changes the step: refetch.
  const s2 = await mountGrid({ view: { s: 30, w: -30, n: 60, e: 30 } });
  const before = callsTo(s2, '/api/ducting/grid').length;
  s2.map.move({ s: 40, w: -1, n: 41, e: 0 });
  s2.flush();
  assert.strictEqual(callsTo(s2, '/api/ducting/grid').length, before + 1, 'zoomed in: finer step wanted');
});

test('grid: the drawn image stays while a new window loads; superseded requests are aborted', async () => {
  const s = await mountGrid();
  const field = s.layer._field, data = s.layer._data;
  slide(s, 9);
  s.flush();
  const a = lastCall(s, '/api/ducting/grid');
  assert.strictEqual(a.url, '/api/ducting/grid?t=' + GMETA.times[9] + '&bbox=' + BASE_BBOX + '&run=' + GMETA.runTime, 'the drawn box is reused for a new time');
  assert.ok(s.map._layers.indexOf(field) >= 0 && s.layer._data === data, 'old image kept while loading');
  slide(s, 10);
  s.flush();
  const b = lastCall(s, '/api/ducting/grid');
  assert.ok(a.opts.signal.aborted, 'the superseded request is aborted');
  assert.ok(!b.opts.signal.aborted);
  const win10 = clone(GWIN); win10.times = [GMETA.times[10]];
  b.ok(win10);
  await settle();
  assert.strictEqual(s.layer._data.times[0], GMETA.times[10]);
  assert.strictEqual(s.layer._field, field, 'same image layer, redrawn');
});

test('grid: windows come from the LRU without a request; a same-window refresh redraws nothing', async () => {
  const s = await mountGrid();
  slide(s, 9); s.flush();
  const w9 = clone(GWIN); w9.times = [GMETA.times[9]];
  lastCall(s, '/api/ducting/grid').ok(w9);
  await settle();
  const n = callsTo(s, '/api/ducting/grid').length;
  slide(s, 8);
  assert.strictEqual(s.timeouts.filter(t => t.ms === 250).length, 0, 'no fetch queued');
  assert.strictEqual(s.layer._data.times[0], GT, 'drawn at once from the cache');
  slide(s, 9);
  assert.strictEqual(s.layer._data.times[0], GMETA.times[9]);
  s.flush();
  assert.strictEqual(callsTo(s, '/api/ducting/grid').length, n, 'no request');
  // A meta refresh with the same run: no rebuild.
  const puts = s.layer._canvas.puts, polys = s.stats.polys.length;
  s.timers[0].fn();
  lastCall(s, '/api/ducting/meta').ok(clone(GMETA));
  await settle();
  s.flush();
  assert.strictEqual(s.layer._canvas.puts, puts);
  assert.strictEqual(s.stats.polys.length, polys);
  assert.strictEqual(callsTo(s, '/api/ducting/grid').length, n);
});

test('grid: a new model run empties the cache and refetches the shown window, keeping the image meanwhile', async () => {
  const s = await mountGrid();
  const n = callsTo(s, '/api/ducting/grid').length, field = s.layer._field;
  const m = clone(GMETA); m.generatedAt += 3 * 3600; m.runTime += 6 * 3600;
  s.timers[0].fn();
  lastCall(s, '/api/ducting/meta').ok(m);
  await settle();
  assert.strictEqual(s.layer._lru.size, 0);
  assert.ok(s.map._layers.indexOf(field) >= 0);
  s.flush();
  assert.strictEqual(callsTo(s, '/api/ducting/grid').length, n + 1);
  assert.strictEqual(lastCall(s, '/api/ducting/grid').url, '/api/ducting/grid?t=' + GT + '&bbox=' + BASE_BBOX + '&run=' + m.runTime,
    'a new URL per run: the HTTP cache cannot answer with the old run');
  assert.notStrictEqual(m.runTime, GMETA.runTime);
});

test('grid: a user-picked time is kept across meta refreshes while stored, else back to the default', async () => {
  const s = await mountGrid();
  slide(s, 3);
  const picked = GMETA.times[3];
  const m = clone(GMETA); m.times = m.times.slice(1).concat([m.times[16] + 10800]); m.generatedAt += 1;
  s.timers[0].fn();
  lastCall(s, '/api/ducting/meta').ok(m);
  await settle();
  assert.strictEqual(s.layer._meta.times[s.layer._sel], picked, 'same time, new index');
  assert.strictEqual(s.layer._sel, 2);
  const m2 = clone(m); m2.times = m2.times.filter(t => t !== picked); m2.generatedAt += 1;
  s.timers[0].fn();
  lastCall(s, '/api/ducting/meta').ok(m2);
  await settle();
  assert.strictEqual(s.layer._meta.times[s.layer._sel], GT, 'pruned: back to the latest <= now');
  assert.strictEqual(s.layer._userPicked, false);
});

test('grid: without a pick the shown time follows now on refresh', async () => {
  const s = await mountGrid();
  s.now = GT + 3 * 3600 + 60;
  s.timers[0].fn();
  lastCall(s, '/api/ducting/meta').ok(clone(GMETA));
  await settle();
  assert.strictEqual(s.layer._sel, 9);
});

test('grid deep link: ductT on #/map picks a stored time when added; unknown time or other page ignored', async () => {
  const s = await mountGrid({ hash: '#/map?duct=1&ductT=' + GMETA.times[4], window: false });
  assert.strictEqual(s.layer._sel, 4);
  assert.strictEqual(s.layer._userPicked, true);
  s.flush();
  assert.ok(lastCall(s, '/api/ducting/grid').url.indexOf('t=' + GMETA.times[4] + '&') > 0);
  const u = await mountGrid({ hash: '#/map?ductT=' + (GT + 1), window: false });
  assert.strictEqual(u.layer._sel, 8, 'not a stored time: default');
  const o = await mountGrid({ hash: '#/live?ductT=' + GMETA.times[4], window: false });
  assert.strictEqual(o.layer._sel, 8);
});

test('grid deep link: picking writes duct=1&ductT (debounced), the default removes ductT, removal clears both', async () => {
  const s = await mountGrid({ hash: '#/map?lat=52&lon=0' });
  slide(s, 2); slide(s, 3);
  assert.strictEqual(s.replaced.length, 0, 'debounced');
  s.flush();
  assert.deepStrictEqual(s.replaced, ['#/map?lat=52&lon=0&duct=1&ductT=' + GMETA.times[3]]);
  slide(s, 8);                           // the default
  s.flush();
  assert.strictEqual(s.ctx.location.hash, '#/map?lat=52&lon=0&duct=1');
  assert.strictEqual(s.layer._userPicked, false);
  slide(s, 5); s.flush();
  s.layer.onRemove(s.map);
  assert.strictEqual(s.ctx.location.hash, '#/map?lat=52&lon=0', 'overlay off: out of the link');
  // Off the map page nothing is written.
  const o = await mountGrid({ hash: '#/live' });
  slide(o, 3); o.flush();
  o.layer.onRemove(o.map);
  assert.deepStrictEqual(o.replaced, []);
});

test('grid links: drawn on the line canvas by kind (dashed without model data), filtered by time', async () => {
  const s = await mountGrid();
  const lines = linkLines(s);
  assert.deepStrictEqual(lines.map(l => l._mcLinks), ['nodata', 'casing', 'plain', 'duct'], 'ducted drawn last (on top)');
  const by = {};
  lines.forEach(l => { by[l._mcLinks] = l; });
  assert.strictEqual(by.duct.opts.color, '#7c2d12');
  assert.strictEqual(by.plain.opts.color, '#505050');
  assert.strictEqual(by.nodata.opts.color, '#505050');
  assert.strictEqual(by.nodata.opts.dashArray, '6 5');
  assert.ok(!by.plain.opts.dashArray && !by.duct.opts.dashArray);
  assert.strictEqual(by.casing.opts.color, 'rgba(255, 255, 255, 0.85)');
  assert.ok(by.casing.opts.weight > by.duct.opts.weight, 'a halo wider than the line');
  assert.strictEqual(by.casing.latlngs.length, 2, 'under the solid links only (the dashed one keeps its gaps)');
  lines.forEach(l => {
    assert.strictEqual(l.opts.interactive, false);
    assert.strictEqual(l.opts.renderer, s.layer._renderer, 'the shared line canvas');
  });
  assert.deepStrictEqual(JSON.parse(JSON.stringify(by.duct.latlngs)), [[[51.2, -0.5], [52.8, 0.7]]]);
  // Another time: other links; same time again: no rebuild.
  const made = s.stats.lines.length;
  slide(s, 6);                          // T - 6 h: aa11's older reception (T - 20000 s)
  assert.deepStrictEqual(linkLines(s).map(l => l._mcLinks), ['casing', 'plain']);
  assert.ok(legendText(s).indexOf('On the map: 1 link heard within 90 min') >= 0, 'count updated in place');
  slide(s, 6);
  assert.strictEqual(s.stats.lines.length, made + 2, 'same time: not redrawn');
  slide(s, 0);
  assert.strictEqual(linkLines(s).length, 0);
  assert.ok(legendText(s).indexOf('None heard within 90 min of the selected time.') >= 0);
});

test('grid links: no colour variable -> not drawn; theme change recolours', async () => {
  const s = await mountGrid();
  s.vars['--mc-duct-link-duct'] = '';
  s.ctx.dispatch('theme-changed');
  assert.deepStrictEqual(linkLines(s).map(l => l._mcLinks), ['nodata', 'casing', 'plain']);
  s.vars['--mc-duct-link-casing'] = '';
  s.ctx.dispatch('theme-changed');
  assert.deepStrictEqual(linkLines(s).map(l => l._mcLinks), ['nodata', 'plain'], 'no casing variable: no casing');
  s.vars['--mc-duct-link-duct'] = '#123456';
  s.ctx.dispatch('theme-changed');
  assert.strictEqual(linkLines(s).find(l => l._mcLinks === 'duct').opts.color, '#123456');
});

test('grid links: a click within 6 px of a drawn link opens its popup before any cell lookup', async () => {
  const s = await mountGrid();
  click(s, 52.0, 0.1);                  // the middle of aa11 -> bb22
  await tick();
  const p = openPopup(s);
  assert.ok(p && p.content.textContent.indexOf('Long link, heard directly') >= 0);
  assert.strictEqual(s.layer._popup, undefined, 'no cell popup made');
  // 6 px from the line (perpendicular) still hits; 7 px does not.
  const seg = [{ x: 50, y: 180 }, { x: 170, y: 20 }];
  const len = Math.hypot(120, -160), nx = 160 / len, ny = 120 / len;  // unit normal
  const at = (px) => ({ lat: 53 - (100 + ny * px) / 100, lng: -1 + (110 + nx * px) / 100 });
  assert.ok(Math.abs(s.ctx.MC_ductSegDist({ x: 110 + nx * 6, y: 100 + ny * 6 }, seg[0], seg[1]) - 6) < 1e-9);
  p.open = false;
  assert.strictEqual(s.layer.openLinkPopup(at(5.9)), true);
  p.open = false;
  assert.strictEqual(s.layer.openLinkPopup(at(7)), false);
  click(s, 52.75, -0.75);               // far from every link: the cell
  await tick();
  assert.ok(s.layer._popup && s.layer._popup.open);
  assert.ok(s.layer._popup.content.textContent.indexOf('Normal') >= 0);
});

test('grid links: an open link popup follows the time and closes when the link is not drawn', async () => {
  const s = await mountGrid();
  assert.ok(s.layer.openLinkPopup({ lat: 52.0, lng: 0.1 }));
  const p = s.layer._linkPopup;
  slide(s, 9);                          // T + 3 h: aa11 not heard within 90 min
  assert.strictEqual(p.open, false);
});

test('grid links: refreshed with each meta refresh; a failed refresh keeps the held links', async () => {
  const s = await mountGrid();
  s.timers[0].fn();
  assert.strictEqual(callsTo(s, '/api/ducting/links').length, 2);
  lastCall(s, '/api/ducting/meta').ok(clone(GMETA));
  lastCall(s, '/api/ducting/links').fail();
  await settle();
  assert.strictEqual(linkLines(s).length, 4);
  const e = await mountGrid({ links: { status: 'error', error: 'x' } });
  assert.ok(legendText(e).indexOf('Long links are not available right now') >= 0);
  assert.strictEqual(linkLines(e).length, 0);
});

test('grid: pending meta retries every minute and shows the preparing message; ok then every 10 min', async () => {
  const s = await mountGrid({ meta: false, links: false });
  callsTo(s, '/api/ducting/meta')[0].ok({ status: 'pending' });
  await settle();
  assert.ok(legendText(s).indexOf(MSG_PENDING_TEXT) >= 0);
  assert.deepStrictEqual(s.timers.map(t => t.ms), [60000]);
  s.timers[0].fn();
  lastCall(s, '/api/ducting/meta').ok(clone(GMETA));
  await settle();
  assert.deepStrictEqual(s.timers.map(t => t.ms), [600000]);
});
const MSG_PENDING_TEXT = 'Ducting forecast is being prepared';

test('grid: out of date on a failed meta refresh (data stays) and once the last time + 3 h has passed', async () => {
  const s = await mountGrid();
  s.timers[0].fn();
  lastCall(s, '/api/ducting/meta').fail();
  await settle();
  assert.ok(legendText(s).indexOf('Forecast out of date') >= 0);
  assert.ok(s.map._layers.indexOf(s.layer._field) >= 0);
  assert.deepStrictEqual(s.timers.map(t => t.ms), [60000], 'asks again every minute');
  const last = GMETA.times[GMETA.times.length - 1];
  const e = await mountGrid({ now: last + 3 * 3600 - 1 });
  assert.ok(legendText(e).indexOf('Forecast out of date') < 0, 'not before last + 3 h');
  const x = await mountGrid({ now: last + 3 * 3600 });
  assert.ok(legendText(x).indexOf('Forecast out of date') >= 0);
});

test('grid: a failed window keeps the previous image and says so; the next success clears it', async () => {
  const s = await mountGrid();
  const field = s.layer._field;
  slide(s, 9); s.flush();
  lastCall(s, '/api/ducting/grid').fail();
  await settle();
  assert.ok(legendText(s).indexOf('Could not load ducting data for this area') >= 0);
  assert.ok(s.map._layers.indexOf(field) >= 0);
  slide(s, 10); s.flush();
  lastCall(s, '/api/ducting/grid').ok({ status: 'error', error: 'busy', cells: [], times: [] });
  await settle();
  assert.ok(legendText(s).indexOf('Could not load ducting data for this area') >= 0, 'an error answer counts as a failure');
  slide(s, 11); s.flush();
  const w = clone(GWIN); w.times = [GMETA.times[11]];
  lastCall(s, '/api/ducting/grid').ok(w);
  await settle();
  assert.ok(legendText(s).indexOf('Could not load ducting data for this area') < 0);
});

test('grid: a view on another world copy draws the window, LoRa outline and links shifted onto it', async () => {
  const s = await mountGrid({ view: { s: 51, w: 359, n: 53, e: 361 } });
  assert.strictEqual(lastCall(s, '/api/ducting/grid').url, '/api/ducting/grid?t=' + GT + '&bbox=' + BASE_BBOX + '&run=' + GMETA.runTime);
  assert.strictEqual(s.layer._grid.west, 359);
  const ring = loraLayer(s).latlngs[0][0];
  assert.deepStrictEqual(JSON.parse(JSON.stringify(ring[0])), [52, 360]);
  const duct = linkLines(s).find(l => l._mcLinks === 'duct');
  assert.deepStrictEqual(JSON.parse(JSON.stringify(duct.latlngs)), [[[51.2, 359.5], [52.8, 360.7]]]);
  click(s, 52.05, 360.45);
  await tick();
  assert.ok(openPopup(s).content.textContent.indexOf('Surface-based duct') >= 0, 'click lookup on the copy');
});

test('grid: remove aborts fetches, clears timers, removes links; re-add draws the held window at once', async () => {
  const s = await mountGrid();
  slide(s, 9); s.flush();
  const pending = lastCall(s, '/api/ducting/grid');
  s.map.move({ s: 40, w: 10, n: 42, e: 12 });
  const group = s.layer._linkGroup;
  s.layer.onRemove(s.map);
  assert.ok(pending.opts.signal.aborted);
  assert.strictEqual(s.timers.length, 0);
  assert.strictEqual(s.timeouts.length, 0, 'debounce timers cleared');
  assert.ok(s.map._layers.indexOf(group) < 0);
  s.map.move({ s: 51, w: -1, n: 53, e: 1 });
  assert.strictEqual(s.timeouts.length, 0, 'no view listener left');
  pending.ok(clone(GWIN));
  await settle();
  assert.strictEqual(s.layer._field, null, 'nothing drawn after remove');
  const puts = s.stats.fields.length;
  s.layer.onAdd(s.map);
  assert.ok(s.layer._field && s.map._layers.indexOf(s.layer._field) >= 0, 'held window drawn at once');
  assert.ok(s.stats.fields.length === puts + 1);
  assert.deepStrictEqual(linkLines(s).map(l => l._mcLinks), ['nodata', 'casing', 'duct'], 'held links drawn at once (time T + 3 h)');
});

const linksToggle = (s) => find(s.layer._legendEl, n => n.tagName === 'button' && /mc-duct-links-toggle/.test(n.className))[0];
const legendBox = (s) => s.layer._legendEl;

test('grid legend: the long-link line key folds behind a button (aria-expanded); folded keeps summary, count and caveat', async () => {
  const s = await mountGrid();
  let tog = linksToggle(s);
  assert.ok(tog, 'a button, not a plain heading');
  assert.strictEqual(tog.type, 'button');
  assert.strictEqual(tog.textContent, 'Observed long links');
  assert.strictEqual(tog.attrs['aria-expanded'], 'true', 'open by default on a tall screen');
  assert.ok(legendText(s).indexOf('Half or more of the path in the High band or stronger (model)') >= 0);
  FOCUSED.length = 0;
  tog.fire('click');
  tog = linksToggle(s);
  assert.strictEqual(tog.attrs['aria-expanded'], 'false');
  assert.strictEqual(FOCUSED[FOCUSED.length - 1], tog, 'focus stays on the (rebuilt) toggle');
  const t = legendText(s);
  assert.ok(t.indexOf(ctxSummary()) >= 0, 'the summary stays');
  assert.ok(t.indexOf('Positions are self-reported; a long link can also come from a wrong position.') >= 0, 'the caveat stays');
  assert.ok(t.indexOf('Half or more of the path in the High band') < 0 && t.indexOf('Dashed: no model data') < 0, 'line key folded away');
  assert.ok(t.indexOf('On the map: 3 links heard within 90 min') >= 0, 'the count at the shown time stays');
  slide(s, 6);
  assert.ok(legendText(s).indexOf('On the map: 1 link heard within 90 min') >= 0, 'and follows the slider while folded');
  assert.strictEqual(linkLines(s).length > 0, true, 'lines on the map are unaffected');
  linksToggle(s).fire('click');
  assert.strictEqual(linksToggle(s).attrs['aria-expanded'], 'true');
  assert.ok(legendText(s).indexOf('Half or more of the path in the High band or stronger (model)') >= 0, 'line key back');
  // The state survives a legend rebuild (links refresh) and a fold of the whole legend.
  linksToggle(s).fire('click');
  s.layer._renderLegend();
  assert.strictEqual(linksToggle(s).attrs['aria-expanded'], 'false');
  const head = find(legendBox(s), n => n.tagName === 'button' && n.className === 'mc-duct-legend-title')[0];
  head.fire('click');
  assert.strictEqual(linksToggle(s), undefined, 'legend folded: no links section');
  assert.strictEqual(FOCUSED[FOCUSED.length - 1].className, 'mc-duct-legend-title', 'focus stays on the title toggle');
  find(legendBox(s), n => n.className === 'mc-duct-legend-title')[0].fire('click');
  assert.strictEqual(linksToggle(s).attrs['aria-expanded'], 'false');
});

test('grid legend: long-link details start folded on a short screen (max-height 820px), open on a tall one', async () => {
  const s = makeGridSandbox();
  s.ctx.matchMedia = (q) => ({ matches: q === '(max-height: 820px)' });
  const map = makeGridMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  Object.assign(s, { map, layer });
  callsTo(s, '/api/ducting/meta')[0].ok(clone(GMETA)); await settle();
  callsTo(s, '/api/ducting/links')[0].ok(clone(GLINKS)); await settle();
  assert.strictEqual(linksToggle(s).attrs['aria-expanded'], 'false');
  assert.ok(legendText(s).indexOf(ctxSummary()) >= 0);
  assert.ok(find(legendBox(s), n => n.className === 'mc-duct-legend-title')[0].attrs['aria-expanded'] === 'true', 'the legend itself stays open (it is not narrow)');
  // No links answer yet: a plain heading with the status, no toggle.
  const p = await mountGrid({ links: false });
  assert.strictEqual(linksToggle(p), undefined);
  assert.ok(legendText(p).indexOf('Loading long links') >= 0);
});

test('grid legend: height capped to end 10 px below the lowest top-left control; never under 40 px; re-measured on render', async () => {
  const s = await mountGrid();
  const box = legendBox(s);
  let bottom = 700;
  box.getBoundingClientRect = () => ({ bottom });
  const kids = [
    { getBoundingClientRect: () => ({ bottom: 152, height: 90 }) },    // zoom
    { getBoundingClientRect: () => ({ bottom: 210, height: 48 }) },    // layers picker
    { getBoundingClientRect: () => ({ bottom: 400, height: 0 }) },     // hidden control: ignored
  ];
  s.map.getContainer = () => ({
    getBoundingClientRect: () => ({ top: 52 }),
    querySelector: (q) => (q === '.leaflet-top.leaflet-left' ? { children: kids } : null),
  });
  s.layer._fitLegend();
  assert.strictEqual(box.style.maxHeight, '480px', '700 - 210 - 10');
  bottom = 230;
  s.layer._renderLegend();
  assert.strictEqual(box.style.maxHeight, '40px', 'floor: the title row stays visible');
  // No top-left controls: up to the map's top.
  kids.length = 0;
  bottom = 700;
  s.ctx.dispatch('resize');
  s.map.fire('resize');
  assert.strictEqual(box.style.maxHeight, String(700 - 52 - 10) + 'px', 'map resize re-measures');
  // A map without a container (not laid out) leaves it alone and does not throw.
  s.map.getContainer = () => { throw new Error('gone'); };
  assert.doesNotThrow(() => s.layer._fitLegend());
});

test('grid legend: the wheel scrolls the capped legend, not the map', () => {
  const s = makeGridSandbox();
  const seen = [];
  s.ctx.L.DomEvent = { disableClickPropagation() {}, disableScrollPropagation(b) { seen.push(b); } };
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(makeGridMap());
  assert.ok(seen.indexOf(layer._legendEl) >= 0);
});

test('grid: Open-Meteo source keeps the old path (/api/ducting, edge line, no links)', async () => {
  const s = makeGridSandbox();
  s.ctx.MC_DUCTING_CFG = { source: 'open-meteo', attribution: 'a' };
  const map = makeGridMap();
  const layer = s.ctx.MC_createDuctingLayer({});
  layer.onAdd(map);
  assert.deepStrictEqual(s.api.calls.map(c => c.url), ['/api/ducting']);
  s.api.calls[0].ok(clone(SAMPLE));
  await settle();
  assert.ok(s.stats.lines.some(l => l._mcEdge));
  assert.strictEqual(layer._linkGroup, undefined);
  assert.ok(map._controls.map(c => c._container.textContent).join('\n').indexOf('edge of the forecast area') >= 0);
  layer._slider.value = '0'; layer._slider.fire('input');
  s.flush();
  assert.deepStrictEqual(s.replaced, [], 'no deep link written in Open-Meteo mode');
});

// Not a gate (wall-clock asserts are flaky in CI): prints how long one
// field draw of a full 4000-cell window takes on this machine.
test('grid perf: a 4000-cell window (50 x 80 at 0.5 deg) draws its field; time reported, not asserted', async () => {
  const cells = [];
  for (let r = 0; r < 50; r++) {
    for (let c = 0; c < 80; c++) {
      const n = -40 - 30 * (0.5 + 0.5 * Math.sin(r / 4) * Math.cos(c / 6));
      cells.push({ lat: 30.25 + r * 0.5, lon: -19.75 + c * 0.5, c: [n < -79 ? 2 : 1], f: [0], b: [0], t: [0], dm: [0], g: [n], n: [Math.round(n)] });
    }
  }
  const win = { status: 'ok', generatedAt: 1, stepDeg: 0.5, times: [GT], cells };
  const s = await mountGrid({ view: { s: 36, w: -12, n: 49, e: 12 }, window: win });
  assert.strictEqual(s.layer._data.cells.length, 4000);
  const g = s.layer._grid;
  s.ctx.Date = Date;                    // real clock for _fieldMs
  const ms = [];
  for (let k = 0; k < 7; k++) { s.layer._drawField(); ms.push(s.layer._fieldMs); }
  ms.sort((a, b) => a - b);
  console.log('    _drawField, 4000 cells, ' + g.w + 'x' + g.h + ' px, inside this vm sandbox (much slower than a page): median ' + ms[3] + ' ms (min ' + ms[0] + ', max ' + ms[6] + ')');
  let colored = 0;
  const d = s.layer._canvas.img.data;
  for (let i = 3; i < d.length; i += 4) if (d[i]) colored++;
  assert.ok(colored > 0, 'pixels drawn');
});

// ── overlay registry ────────────────────────────────────────────────────────

function loadProviders(before) {
  const ctx = {
    console: { log() {}, error() {}, warn() {} },
    JSON, Date, Math, Object, Array, String, Number, Boolean, isFinite,
    document: { documentElement: { getAttribute: () => null }, querySelector: () => null, createElement: makeEl },
  };
  ctx.window = { addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, matchMedia: () => ({ matches: false }), localStorage: null };
  ctx.window.document = ctx.document;
  ctx.L = ctx.window.L = { tileLayer: () => ({ kind: 'tile' }) };
  vm.createContext(ctx);
  if (before) before(ctx.window);
  vm.runInContext(PROV, ctx);
  return ctx.window;
}

test('overlay is not registered without MC_DUCTING_CFG', () => {
  const w = loadProviders();
  assert.ok(!('ducting' in w.MC_TILE_OVERLAYS));
});

test('overlay is registered from MC_DUCTING_CFG and rebuilt when config arrives later', () => {
  const w = loadProviders();
  // The EOX labels are an opt-in group: on here, to compare the stacking.
  w.MC_MAP_CFG = { tiles: { providers: { eox: { enabled: true } } } };
  w.MC_DUCTING_CFG = { model: 'gfs', modelLabel: 'NOAA GFS', stepDeg: 0.5, attribution: 'Weather data by X' };
  w.MC_initTileRegistry(true);
  const o = w.MC_TILE_OVERLAYS.ducting;
  assert.ok(o);
  assert.strictEqual(o.kind, 'ducting');
  assert.strictEqual(o.label, 'Tropospheric ducting');
  assert.strictEqual(o.attribution, 'Weather data by X');
  assert.ok(o.zIndex > w.MC_TILE_OVERLAYS['eox-labels'].zIndex - 1);
  w.MC_initTileRegistry(true);
  assert.strictEqual(w.MC_TILE_OVERLAYS.ducting, o, 'same entry object across rebuilds');
  w.MC_DUCTING_CFG = null;
  w.MC_initTileRegistry(true);
  assert.ok(!('ducting' in w.MC_TILE_OVERLAYS));
});

test('MC_buildOverlayLayer: kind ducting calls the factory; absent factory -> null, no throw', () => {
  const w = loadProviders(x => { x.MC_DUCTING_CFG = { attribution: 'a' }; });
  w.MC_initTileRegistry();
  const entry = w.MC_TILE_OVERLAYS.ducting;
  assert.strictEqual(w.MC_createDuctingLayer, undefined);
  assert.strictEqual(w.MC_buildOverlayLayer(entry, { pane: 'p' }), null);
  const seen = [];
  w.MC_createDuctingLayer = (d) => { seen.push(d); return { kind: 'ducting-layer' }; };
  assert.strictEqual(w.MC_buildOverlayLayer(entry).kind, 'ducting-layer');
  assert.strictEqual(seen[0], entry);
  w.MC_createDuctingLayer = () => { throw new Error('x'); };
  assert.strictEqual(w.MC_buildOverlayLayer(entry), null);
});

test('factory without Leaflet returns null instead of throwing', () => {
  const s = makeSandbox({ noLeaflet: true });
  assert.strictEqual(s.ctx.MC_createDuctingLayer({}), null);
});

// ── run ─────────────────────────────────────────────────────────────────────

(async () => {
  let passed = 0, failed = 0;
  for (const [name, fn] of queue) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
