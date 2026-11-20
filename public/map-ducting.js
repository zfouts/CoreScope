/* map-ducting.js — tropospheric ducting map overlay.
 *
 * Draws the forecast mean refractivity gradient over the lowest 1.5 km (the
 * cells' `n`) as a semi-transparent field of colour bands (like the usual
 * tropo-ducting forecast maps): transparent where propagation is normal, then
 * purple, blue, cyan, green, yellow, orange, red and pink as the gradient gets
 * stronger; each colour is one range of N/km. Cells where the model shows a
 * duct that may carry LoRa (lowest trapped frequency, as shown, at most this
 * site's LoRa frequency, or at most 915 MHz when the server sets none) get a
 * dashed outline (from the duct analysis: c and f, not n).
 * Data: GET /api/ducting (hourly
 * forecast, one array entry per `times` entry, see cmd/server/ducting_types.go).
 *
 *   window.MC_createDuctingLayer(opts) -> L.Layer
 *       Fetches /api/ducting when added to a map, refreshes every 30 min while
 *       on it (every minute until it has data), draws the field as one canvas
 *       image overlay, the LoRa outlines and the forecast-area edge as
 *       non-interactive lines, a legend and a forecast-time slider.
 *       A layer removed mid-fetch never touches the map afterwards.
 *       Nothing it draws takes pointer events: a click on the map is looked
 *       up in the lattice (MC_ductCellAt) by one map click handler, which
 *       never stops the event, so the page's own map clicks (Planner sites,
 *       Live paths) work inside the forecast area. The cell popup opens only
 *       when the click did nothing else (see _watchClicks).
 *   layer.openCellPopup(latlng) -> true when a cell with data at the shown
 *       hour is there and its popup opened
 *   window.MC_ductCellAt(grid, lat, lon) -> index into the cells of the cell
 *       whose square contains (lat, lon), or -1
 *   window.MC_ductTimeIndex(times, now) -> index of the latest time <= now
 *   window.MC_ductFieldColor(v, stops)  -> [r, g, b, a] (band colour) for a field value (N/km)
 *   window.MC_ductFieldStops()          -> stops read from the CSS variables
 *   window.MC_ductGrid(cells, step)     -> the cells' lattice (see ductGrid)
 *   window.MC_ductGridValues(grid, cells, i) -> Float32Array, NaN = no data
 *   window.MC_ductSample(grid, vals, lat, lon) -> { g, a }: g = field value
 *       (n, N/km; the name predates n), a = coverage
 *   window.MC_ductFieldValues(grid, vals) -> { g, a } Float32Arrays, one entry
 *       per image pixel: what the field image is coloured from
 *   window.MC_ductRowLat(y, h, north, south)   -> latitude of image row y
 *
 * The field image:
 *   - The cells sit on a regular lattice: centre = floor(x/step)*step + step/2.
 *     The image covers the bounding box of every cell that has data at some
 *     hour (the cells' outer edges), so its bounds stay put while the slider
 *     moves. Several separate regions share one image; its long side is
 *     capped at MAX_SIDE px, so far-apart regions get fewer pixels per cell
 *     (the browser smooths the upscaling). PX_PER_CELL is high so the band
 *     edges stay smooth curves at the zoom a region is usually viewed at. Cells on both sides of the
 *     antimeridian are drawn where Leaflet puts them (lon -180..180); the
 *     image then spans the whole width, nothing is wrapped.
 *   - Rows are sampled in Web Mercator (row -> latitude by the inverse
 *     projection), so the field lines up with the basemap at every zoom.
 *   - The value is the cell's mean dN/dh over the lowest 1.5 km (n; 0 or
 *     missing = no data at that hour), interpolated between cell
 *     centres with Catmull-Rom bicubic over the 4x4 neighbourhood (lattice
 *     indices clamped at its border), so band edges are smooth curves rather
 *     than the straight segments bilinear gives. Next to a cell without data
 *     (any of the 16 missing) it falls back to bilinear over the cells that
 *     have data. Where a neighbour has no data the field fades to
 *     transparent by the cell's edge (coverage, bilinear), so there are no
 *     hard square edges. At the outer edge of the forecast area (the lattice
 *     slots that have data at some hour) the fade is twice as wide: it starts
 *     a full cell inside the edge (see areaSoft).
 *   - The area's outer boundary is drawn as thin dotted lines (ductEdges),
 *     one segment per run of cell edges, so users see where the forecast ends.
 *   - Colour: the flat colour of the band n falls in (no blending between
 *     stops), every band at the same field opacity, times the coverage.
 *   - Built only when the data, the forecast hour or the theme changes,
 *     O(image pixels): the interpolation is separable (per image column the
 *     lattice taps and weights are computed once per grid, per image row the
 *     vertical sums once per lattice column), so a pixel costs a handful of
 *     multiply-adds. Pan and zoom only move/scale the image.
 *
 * Colours come from the --mc-duct-s1..s8 CSS variables (hex or rgb()), the
 * field opacity from --mc-duct-alpha and the LoRa outline from
 * --mc-duct-lora and the dotted edge line from --mc-duct-edge, all read at
 * draw time.
 *   window.MC_ductEdges(grid)           -> [[[lat, lon], [lat, lon]], ...]
 *
 * Global grid with history (MC_DUCTING_CFG.source "gfs-grid"; any other
 * source is the Open-Meteo mode above, unchanged):
 *   - GET /api/ducting/meta gives the stored times (every 3 h, past and
 *     forecast); refreshed every 10 min while ok, every minute otherwise.
 *     The slider spans them; default = the latest time <= now. A time the
 *     user picked is kept while it is still stored.
 *   - GET /api/ducting/grid?t=&bbox=s,w,n,e gives one time inside a box, at
 *     a step the server coarsens to at most ~4000 cells. The box is the view
 *     padded by 30 %, rounded outward to whole degrees and clamped
 *     (windowBox); a drawn window that still covers the view at the same step
 *     is reused, so panning inside it fetches nothing. Fetches are debounced
 *     (250 ms), superseded ones aborted, answers kept in a small LRU (24
 *     windows, cleared on a new model run). A window is drawn through the
 *     lattice/field/LoRa code above as time index 0, without the edge line;
 *     the previous image stays until the next one is drawn.
 *   - A view on a world copy (centre beyond +-180) asks for the wrapped box
 *     and draws it shifted by the copy's multiple of 360 (shiftFor).
 *   - Deep link (#/map only): picking a time writes duct=1&ductT=<unix>
 *     (history.replaceState, debounced), the default time removes ductT,
 *     removing the overlay removes both. ductT is read when the overlay is
 *     added and used when it is a stored time. map.js turns the overlay on
 *     for duct=1.
 *   - GET /api/ducting/links?days=<historyDays> (on add and with each meta
 *     refresh): node pairs heard directly without terrain line of sight
 *     (losMethod "terrain"; else beyond the smooth-earth radio horizon).
 *     The ones within +-90 min of the shown time are drawn as
 *     non-interactive lines on the line canvas (--mc-duct-link-duct when
 *     half or more of that reception's path was in the legend's High band
 *     or stronger in the model, else --mc-duct-link; dashed without a modelled
 *     path gradient; the solid ones over a --mc-duct-link-casing halo). The
 *     map click handler opens a link popup for a click
 *     within 6 px of one before it looks up a cell. Names are node-chosen
 *     text: textContent only.
 *   window.MC_ductWindowBox(s, w, n, e, pad) -> { s, w, n, e, shift } or null
 *   window.MC_ductWindowStep(box, nativeStep, maxCells) -> the server's step
 *   window.MC_ductBoxCovers(outer, inner), window.MC_ductLru(max)
 *   window.MC_ductTimeLabel(t, now)     -> 'time · forecast' / '· analysis, N h ago'
 *   window.MC_ductHashTime(hash), window.MC_ductHashWith(hash, updates)
 *   window.MC_ductLinksAt(links, t, winS) -> [{ link, recs, kind }]
 *   window.MC_ductLinkKind(reception)   -> 'duct' | 'plain' | 'nodata'
 *   window.MC_ductSegDist(p, a, b)      -> px from point p to segment ab
 *   window.MC_ductLinksSummary(summary, days, losMethod) -> legend sentence(s)
 *   window.MC_ductLinkPopup(item, freqMHz, losMethod) -> popup DOM (textContent only)
 */
(function () {
  'use strict';

  var API_URL = '/api/ducting';
  var REFRESH_MS = 30 * 60 * 1000;
  // While there is no data yet (server still preparing, or a failed fetch),
  // ask again every minute instead of waiting a full refresh period.
  var RETRY_MS = 60 * 1000;
  // A duct "may carry LoRa" at a frequency when its lowest trapped frequency
  // (as shown, two significant figures) is at most that frequency: this
  // site's LoRa frequency when the server sets one (los.frequencyMhz), else
  // 868 and 915 MHz each. With model levels ~200 m apart it is an estimate.
  var BAND_868 = 868;           // LoRa bands judged when the site's frequency is unknown
  var BAND_915 = 915;
  var TYPE_NAME = { 0: 'No data', 1: 'Normal', 2: 'Super-refraction', 3: 'Elevated duct', 4: 'Surface-based duct' };

  // Colour bands: --mc-duct-sN colours n (mean dN/dh over the lowest 1.5 km,
  // N/km) from STOP_N[N-1] down to (not including) the next stop; the last one
  // everything below it. Above the first (-44) is transparent: normal air is
  // about -40. Whole numbers: the colour table relies on it.
  var STOP_N = [-44, -48, -54, -58, -62, -70, -80, -90];
  var DEFAULT_ALPHA = 0.8;      // when --mc-duct-alpha is unset
  // Colour table: entry k is the colour of the value -k, and of every value
  // in (-k - 1, -k] (same band, the stops being whole numbers); the last
  // entry covers everything stronger.
  var LUT_SIZE = 1 - STOP_N[STOP_N.length - 1];
  var PX_PER_CELL = 48;
  var MAX_SIDE = 2048;          // image long side, px
  var MAX_GRID = 1 << 22;       // lattice slots (0.25 deg worldwide is ~1M)
  var MAX_LAT = 85.0511287798;  // Web Mercator limit
  var FIELD_PANE = 'mcDuctFieldPane';
  var FIELD_Z = '245';          // above tiles (200), below the tile overlays (250) and vectors (400)
  // The LoRa outlines and the forecast-area edge: their own canvas in their
  // own pane, above the tile overlays, below every page's vectors (400) and
  // Planner's node pane (390), and never hit by the pointer, so the canvas
  // never sits over another page's clickable paths.
  var LINE_PANE = 'mcDuctLinePane';
  var LINE_Z = '380';
  var LORA_VAR = '--mc-duct-lora';
  var ALPHA_VAR = '--mc-duct-alpha';
  var EDGE_VAR = '--mc-duct-edge';
  var EDGE_STYLE = { stroke: true, weight: 1, opacity: 0.7, dashArray: '1 5', interactive: false };
  var LORA_STYLE = { stroke: true, weight: 1.5, opacity: 0.95, dashArray: '4 3', fill: false, interactive: false };

  // Legend: where each band starts (0..1 along the bar; the last runs to the
  // end, transparent before the first). Spread out instead of linear in n,
  // so the labels have room. "Very high" sits right-aligned at the end.
  var BAR_POS = [0.19, 0.26, 0.34, 0.41, 0.48, 0.6, 0.73, 0.86];
  var BAR_LABELS = [[0, 'Normal'], [BAR_POS[0], 'Above normal'], [BAR_POS[4], 'High'], [1, 'Very high', true]];
  var CAPTION = 'Refractivity gradient over the lowest 1.5 km (N/km)';
  var MINUS = '\u2212';

  var NOTE = 'Indicator only: model levels are ~200 m apart, so thinner ducts are not visible.';
  var MSG_PENDING = 'Ducting forecast is being prepared';
  var MSG_ERROR = 'Ducting forecast unavailable';
  var MSG_STALE = 'Forecast out of date \u2014 showing the last one received';
  // 'Dashed outline: … LoRa at 910.525 MHz' (this site) or '868 / 915 MHz'.
  function msgLoraRow() {
    return 'Dashed outline: the model shows a duct that may carry LoRa at ' + loraFreqs().map(function (f) { return Math.round(f * 1000) / 1000; }).join(' / ') + ' MHz';
  }
  var HOUR_S = 3600;

  // Global grid mode (source "gfs-grid"). Fixed for now; candidates for the
  // customizer later (see the PR's follow-up list).
  var META_URL = '/api/ducting/meta';
  var GRID_URL = '/api/ducting/grid';
  var LINKS_URL = '/api/ducting/links';
  var GRID_REFRESH_MS = 10 * 60 * 1000;
  var GRID_STEP_S = 3 * HOUR_S;         // spacing of the stored times
  var WINDOW_PAD = 0.3;                 // view padding on each side
  var WINDOW_DEBOUNCE_MS = 250;
  var HASH_DEBOUNCE_MS = 300;
  var LRU_MAX = 24;
  var GRID_MAX_CELLS = 4000;            // the server's cap per window
  var STEP_LADDER = [1, 2, 4, 8, 16];   // x the native step (0.5 -> 0.5..8 deg)
  var LINK_WINDOW_S = 90 * 60;          // receptions drawn within +-90 min
  var LINK_ENHANCED_FRAC = 0.5;         // emphasized: half the path or more in the High band or stronger (highFrac)
  var LINK_HIT_PX = 6;
  var LINK_MAX_RECS = 5;                // receptions listed in a link popup
  var NAME_MAX = 64;
  var LINK_VAR = '--mc-duct-link';
  var LINK_DUCT_VAR = '--mc-duct-link-duct';
  var LINK_CASING_VAR = '--mc-duct-link-casing';  // halo under the solid links: legible over any band
  var LINK_STYLE = { stroke: true, weight: 2.5, opacity: 0.9, fill: false, interactive: false };
  var MSG_WINDOW_ERROR = 'Could not load ducting data for this area';
  var MSG_PAST = 'Past times show the model’s analysis or short-range forecast for that time, not measurements.';
  var MSG_POSITIONS = 'Positions are self-reported; a long link can also come from a wrong position.';
  // The legend never grows over the controls above it (zoom, layers): its
  // max-height is the room between its bottom and the lowest top-left
  // control, less LEGEND_GAP; it scrolls inside. Never below LEGEND_MIN_H
  // (the title row). On short screens the long-link details start folded.
  var LEGEND_GAP = 10;
  var LEGEND_MIN_H = 40;
  var SHORT_SCREEN = '(max-height: 820px)';

  function isGridMode() {
    var c = (typeof window !== 'undefined' && window.MC_DUCTING_CFG) || null;
    return !!(c && c.source === 'gfs-grid');
  }

  // ── Pure helpers ──────────────────────────────────────────────────────────

  // Latest time <= now; the first when every time is in the future; -1 when
  // there are no times.
  function ductTimeIndex(times, nowSec) {
    if (!Array.isArray(times) || !times.length) return -1;
    var best = 0;
    for (var i = 0; i < times.length; i++) {
      if (times[i] <= nowSec) best = i; else break;
    }
    return best;
  }

  function fmtTime(sec) {
    try {
      return new Date(sec * 1000).toLocaleString([], { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    } catch (_) { return String(sec); }
  }

  function at(arr, i) { return (arr && arr[i] != null) ? Number(arr[i]) : 0; }

  // The field value of `cell` at time index i: n (N/km), or NaN when there is
  // no data (class 0; n missing, 0 or not a number).
  function fieldValue(cell, i) {
    if (at(cell.c, i) < 1) return NaN;
    var n = at(cell.n, i);
    return (n !== 0 && isFinite(n)) ? n : NaN;
  }

  // '-52' with a real minus sign.
  function fmtSigned(v) { return v < 0 ? MINUS + Math.abs(v) : String(v); }

  // Two significant figures: 78 -> 78, 380 -> 380, 868 -> 870, 1234 -> 1200.
  function sig2(v) { return Number(Number(v).toPrecision(2)); }

  // The LoRa frequencies to judge a duct against: this site's own (the line
  // of sight setting, /api/config/client los.frequencyMhz) when the server
  // has one, else both common bands. In MHz, ascending.
  function loraFreqs() {
    var c = (typeof window !== 'undefined' && window.MC_LOS_CFG) || null;
    var f = c ? Number(c.frequencyMhz) : NaN;
    return (f >= 100 && f <= 6000) ? [f] : [BAND_868, BAND_915];
  }

  // A duct's lowest trapped frequency as shown (two significant figures):
  // the verdicts compare this same value, so the number and the answer never
  // disagree at a band edge. 0 = no duct.
  function shownFmin(cell, i) {
    var f = at(cell.f, i);
    return (at(cell.c, i) >= 3 && f > 0) ? sig2(f) : 0;
  }

  // Dashed outline: the model's duct may trap at least one of loraFreqs().
  function isLora(cell, i) {
    var f = shownFmin(cell, i), fr = loraFreqs();
    return f > 0 && f <= fr[fr.length - 1];
  }

  function fmtMHz(f) { return (Math.round(f * 1000) / 1000) + ' MHz'; }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // Popup body for cell `cell` at time index `i` (DOM nodes, textContent only).
  function popupContent(cell, i) {
    var cls = at(cell.c, i);
    var box = el('div', 'mc-duct-popup');
    box.appendChild(el('div', 'mc-duct-popup-title', TYPE_NAME[cls] || TYPE_NAME[0]));
    // What the field colour shows; for every class, since it can be strong
    // where the duct analysis finds no duct.
    var n = fieldValue(cell, i);
    if (n === n) box.appendChild(el('div', null, 'Gradient, lowest 1.5 km: ' + fmtSigned(Math.round(n)) + ' N/km'));
    if (cls < 2) return box;
    var rows = [];
    if (cls >= 3) {
      rows.push('Trapping layer in the model (its levels are ~200 m apart, so thinner layers are not resolved)');
      rows.push('Base: ' + Math.round(at(cell.b, i)) + ' m above ground');
      rows.push('Top: ' + Math.round(at(cell.t, i)) + ' m above ground');
    }
    rows.push('Strength: ' + (at(cell.dm, i) / 10).toFixed(1) + ' M-units');
    var shown = shownFmin(cell, i);
    rows.push('Lowest trapped frequency: ' + (shown > 0 ? '\u2248 ' + shown + ' MHz (model estimate)' : 'none'));
    loraFreqs().forEach(function (fr) {
      rows.push('LoRa ' + fmtMHz(fr) + ': ' + (shown > 0 && shown <= fr ? 'possibly trapped (model estimate)' : 'not trapped by this layer'));
    });
    for (var r = 0; r < rows.length; r++) box.appendChild(el('div', null, rows[r]));
    return box;
  }

  // '' when the variable is undefined.
  function cssVar(name) {
    try {
      var v = getComputedStyle(document.documentElement).getPropertyValue(name);
      return v ? String(v).trim() : '';
    } catch (_) { return ''; }
  }

  // '#rgb', '#rrggbb', '#rrggbbaa', 'rgb(r, g, b)' or 'rgba(...)' -> [r, g, b]
  // (alpha ignored: the field has one opacity); null for anything else.
  function parseColor(s) {
    s = String(s || '').trim().toLowerCase();
    var m = /^#([0-9a-f]{3}|[0-9a-f]{6}|[0-9a-f]{8})$/.exec(s);
    if (m) {
      var h = m[1];
      if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
      return [parseInt(h.slice(0, 2), 16), parseInt(h.slice(2, 4), 16), parseInt(h.slice(4, 6), 16)];
    }
    m = /^rgba?\(\s*([\d.]+)[\s,]+([\d.]+)[\s,]+([\d.]+)/.exec(s);
    if (m) return [Math.round(+m[1]), Math.round(+m[2]), Math.round(+m[3])];
    return null;
  }

  // The colour stops from the CSS variables: [{ g, rgb }] (g = the stop's
  // field value, N/km), strongest last.
  // A missing or unreadable variable drops its stop (nothing is invented).
  function fieldStops() {
    var out = [];
    for (var k = 0; k < STOP_N.length; k++) {
      var rgb = parseColor(cssVar('--mc-duct-s' + (k + 1)));
      if (rgb) out.push({ g: STOP_N[k], rgb: rgb });
    }
    return out;
  }

  function fieldAlpha() {
    var a = parseFloat(cssVar(ALPHA_VAR));
    return (isFinite(a) && a >= 0 && a <= 1) ? a : DEFAULT_ALPHA;
  }

  // Colour of field value g (N/km): [r, g, b, a], the flat colour of its band
  // (stops[k].g >= g > stops[k + 1].g -> stops[k]; at or below the last stop
  // -> the last), alpha 1; transparent above the first stop, for NaN or
  // without stops. No blending between stops, no opacity ramp.
  function fieldColor(g, stops) {
    if (!stops || !stops.length || !(g <= stops[0].g)) return [0, 0, 0, 0];
    for (var k = stops.length - 1; k > 0; k--) if (g <= stops[k].g) break;
    var c = stops[k].rgb;
    return [c[0], c[1], c[2], 1];
  }

  // RGBA table: entry n is the colour of the value -n, alpha scaled by `alpha`.
  function fieldLut(stops, alpha) {
    var lut = new Uint8ClampedArray(LUT_SIZE * 4);
    for (var n = 0; n < LUT_SIZE; n++) {
      var c = fieldColor(-n, stops);
      lut[n * 4] = c[0]; lut[n * 4 + 1] = c[1]; lut[n * 4 + 2] = c[2];
      lut[n * 4 + 3] = Math.round(c[3] * alpha * 255);
    }
    return lut;
  }

  function pct(f) { return Math.round(f * 1000) / 10 + '%'; }

  function mercY(lat) { return Math.log(Math.tan(Math.PI / 4 + lat * Math.PI / 360)); }

  // Latitude at y px from the top of an image h px tall spanning north..south
  // in Web Mercator (y may be fractional; 0 = north edge, h = south edge).
  function rowLat(y, h, north, south) {
    var yn = mercY(north), ys = mercY(south);
    var m = yn + (ys - yn) * (y / h);
    return (2 * Math.atan(Math.exp(m)) - Math.PI / 2) * 180 / Math.PI;
  }

  // Lattice of the cells that have data (class >= 1) at some hour:
  // { step, i0, j0, rows, cols, slot (cell index -> lattice slot, -1 = none),
  //   cell (lattice slot -> cell index, -1 = none),
  //   south, west, north, east (outer cell edges), w, h (image px) }, or null.
  // shift (degrees, a multiple of 360; default 0) moves every cell east by
  // that much: a window drawn on a world copy.
  function ductGrid(cells, step, shift) {
    step = Number(step);
    shift = Number(shift) || 0;
    if (!(step > 0) || !Array.isArray(cells)) return null;
    var slot = new Int32Array(cells.length).fill(-1);
    var ii = new Int32Array(cells.length), jj = new Int32Array(cells.length);
    var i0 = Infinity, i1 = -Infinity, j0 = Infinity, j1 = -Infinity, n = 0;
    for (var k = 0; k < cells.length; k++) {
      var cell = cells[k];
      if (!cell || !Array.isArray(cell.c) || !isFinite(cell.lat) || !isFinite(cell.lon)) continue;
      var any = false;
      for (var t = 0; t < cell.c.length; t++) if (cell.c[t] >= 1) { any = true; break; }
      if (!any) continue;
      var i = Math.round(cell.lat / step - 0.5), j = Math.round((cell.lon + shift) / step - 0.5);
      ii[k] = i; jj[k] = j; slot[k] = 0; n++;
      if (i < i0) i0 = i; if (i > i1) i1 = i;
      if (j < j0) j0 = j; if (j > j1) j1 = j;
    }
    if (!n) return null;
    var rows = i1 - i0 + 1, cols = j1 - j0 + 1;
    if (rows * cols > MAX_GRID) return null;
    var present = new Uint8Array(rows * cols);
    var owner = new Int32Array(rows * cols).fill(-1);
    for (k = 0; k < cells.length; k++) {
      if (slot[k] !== 0) continue;
      slot[k] = (ii[k] - i0) * cols + (jj[k] - j0);
      present[slot[k]] = 1;
      owner[slot[k]] = k;
    }
    var g = { step: step, i0: i0, j0: j0, rows: rows, cols: cols, slot: slot, cell: owner, soft: areaSoft(present, rows, cols),
      south: Math.max(-MAX_LAT, i0 * step), north: Math.min(MAX_LAT, (i1 + 1) * step),
      west: j0 * step, east: (j1 + 1) * step };
    if (!(g.north > g.south)) return null;
    // Image size: PX_PER_CELL per cell across, the Mercator aspect down,
    // long side capped.
    var w = cols * PX_PER_CELL;
    var h = w * ((mercY(g.north) - mercY(g.south)) * 180 / Math.PI) / (g.east - g.west);
    var scale = Math.min(1, MAX_SIDE / Math.max(w, h));
    g.w = Math.max(1, Math.round(w * scale));
    g.h = Math.max(1, Math.round(h * scale));
    return g;
  }

  // The cell (index into the cells) whose square [i*step, (i+1)*step) x
  // [j*step, (j+1)*step) contains (lat, lon), or -1. Longitude is wrapped
  // into the 360 degrees east of the lattice's west edge first (a click on a
  // world copy; for a lattice at -180..180 that is the usual -180..180). O(1).
  function ductCellAt(grid, lat, lon) {
    lat = Number(lat); lon = Number(lon);
    if (!grid || !grid.cell || !isFinite(lat) || !isFinite(lon)) return -1;
    var w0 = Number(grid.west) || 0;
    lon = ((lon - w0) % 360 + 360) % 360 + w0;
    var i = Math.floor(lat / grid.step) - grid.i0, j = Math.floor(lon / grid.step) - grid.j0;
    if (i < 0 || i >= grid.rows || j < 0 || j >= grid.cols) return -1;
    return grid.cell[i * grid.cols + j];
  }

  // Per lattice slot: 0 outside the forecast area, 1 on its outer edge (some
  // of the 8 neighbours is outside, or off the lattice), 2 inside. The
  // coverage (rowSums/evalAt) uses it to widen the fade at the outer edge only.
  function areaSoft(present, rows, cols) {
    var out = new Uint8Array(rows * cols);
    for (var r = 0; r < rows; r++) {
      for (var c = 0; c < cols; c++) {
        var s = r * cols + c;
        if (!present[s]) continue;
        var inner = r > 0 && r < rows - 1 && c > 0 && c < cols - 1;
        if (inner) {
          for (var d = -1; d <= 1 && inner; d++) {
            var o = s + d * cols;
            if (!present[o - 1] || !present[o] || !present[o + 1]) inner = false;
          }
        }
        out[s] = inner ? 2 : 1;
      }
    }
    return out;
  }

  // The outer boundary of the forecast area: the cell edges whose neighbour
  // cell is outside the area, merged into runs along each lattice row and
  // column: [[[lat, lon], [lat, lon]], ...]. A filled rectangle is 4 segments.
  // Across the antimeridian a cell at lon 180 and one at -180 are neighbours,
  // so there is no edge at the seam when both exist. O(lattice slots).
  function ductEdges(grid) {
    var out = [];
    if (!grid || !grid.soft) return out;
    var soft = grid.soft, rows = grid.rows, cols = grid.cols, step = grid.step;
    var clampLat = function (lat) { return Math.max(-MAX_LAT, Math.min(MAX_LAT, lat)); };
    var r, c, start, lat, lon;
    for (r = 0; r <= rows; r++) {
      lat = clampLat((grid.i0 + r) * step);
      start = -1;
      for (c = 0; c <= cols; c++) {
        var edge = c < cols && ((r > 0 && soft[(r - 1) * cols + c] > 0) !== (r < rows && soft[r * cols + c] > 0));
        if (edge && start < 0) start = c;
        if (!edge && start >= 0) {
          out.push([[lat, (grid.j0 + start) * step], [lat, (grid.j0 + c) * step]]);
          start = -1;
        }
      }
    }
    var seamCol = function (lon, east) {   // lattice column across the antimeridian, or -1
      var j = Math.round(east ? -180 / step : 180 / step - 1) - grid.j0;
      return (Math.abs(lon - (east ? 180 : -180)) < 1e-9 && j >= 0 && j < cols) ? j : -1;
    };
    for (c = 0; c <= cols; c++) {
      lon = (grid.j0 + c) * step;
      var mirror = c === cols ? seamCol(lon, true) : c === 0 ? seamCol(lon, false) : -1;
      start = -1;
      for (r = 0; r <= rows; r++) {
        var has = function (col) { return r < rows && col >= 0 && col < cols && soft[r * cols + col] > 0; };
        var left = c > 0 ? has(c - 1) : has(mirror);
        var right = c < cols ? has(c) : has(mirror);
        var vedge = r < rows && left !== right;
        if (vedge && start < 0) start = r;
        if (!vedge && start >= 0) {
          out.push([[clampLat((grid.i0 + start) * step), lon], [clampLat((grid.i0 + r) * step), lon]]);
          start = -1;
        }
      }
    }
    return out;
  }

  // The field value (n) at time index i per lattice slot; NaN where there is
  // no data (class 0, n 0 or missing: an older server sends no n, and then
  // the whole field is empty while the cells, popups and outlines still work).
  function ductGridValues(grid, cells, i) {
    var vals = new Float32Array(grid.rows * grid.cols).fill(NaN);
    for (var k = 0; k < cells.length; k++) {
      var s = grid.slot[k];
      if (s >= 0) vals[s] = fieldValue(cells[k], i);
    }
    return vals;
  }

  // ── Interpolation ──
  // At lattice row v / column u (fractional, 0 = first centre):
  //   g: Catmull-Rom bicubic over the 4x4 slots around (v, u), indices
  //      clamped to the lattice; bilinear over the slots with data when any
  //      of the 16 has none (NaN propagates through the sums, which is how
  //      that is detected).
  //   a: coverage, from the bilinear weights of the slots with data: 1 where
  //      all four have data, falling to 0 by the edge of a cell whose
  //      neighbour has none; with `soft` (areaSoft) also scaled down towards
  //      the outer edge of the forecast area, so the fade there spans a full
  //      cell (1 at the second cell in, 0 at the edge).
  // Separable: colTaps holds the horizontal taps of a set of columns,
  // rowSums the vertical sums of one row per lattice column, rowEval combines
  // them. The renderer and sample() share all three, so they agree exactly.

  // Catmull-Rom weights of the 4 taps at t (0..1) into w[o..o+3]; t = 0 is
  // exactly [0, 1, 0, 0], so the field equals the data at the cell centres.
  function crWeights(t, w, o) {
    var t2 = t * t, t3 = t2 * t;
    w[o] = 0.5 * (2 * t2 - t3 - t);
    w[o + 1] = 0.5 * (3 * t3 - 5 * t2 + 2);
    w[o + 2] = 0.5 * (4 * t2 - 3 * t3 + t);
    w[o + 3] = 0.5 * (t3 - t2);
  }

  // Horizontal taps for lattice columns us[x]: bj = the interval between
  // lattice columns bj - 1 and bj (index into rowSums' padded arrays; -1 =
  // off the lattice), fu = the fraction across it.
  function colTaps(cols, us) {
    var n = us.length;
    var T = { bj: new Int32Array(n), fu: new Float64Array(n) };
    for (var x = 0; x < n; x++) {
      var u = us[x], j = Math.floor(u);
      T.bj[x] = (j >= -1 && j < cols) ? j + 1 : -1;
      T.fu[x] = u - j;
    }
    return T;
  }

  // Reusable per-row buffers for a lattice `cols` wide. P/S/Q are padded by
  // one column on each side (zeros), so bilinear taps never need a check.
  function rowBufs(cols) {
    return { ok: false, P: new Float64Array(cols + 2), S: new Float64Array(cols + 2), Q: new Float64Array(cols + 2),
      cub: new Float64Array(cols), C: new Float64Array((cols + 1) * 4), wv: new Float64Array(4) };
  }

  // Vertical sums of lattice row v into R, per lattice column c:
  //   P/S/Q[c + 1] = bilinear weight, weighted value and weighted soft of the
  //   two rows around v that have data; cub[c] = bicubic sum of the 4 rows
  //   (NaN when any has no data). Then, per interval b between columns b - 1
  //   and b, the horizontal Catmull-Rom polynomial through cub[b - 2..b + 1]
  //   (clamped): C[4b..4b+3] = c0..c3, g(t) = ((c3 t + c2) t + c1) t + c0.
  //   c0 is the column's own value, so g(0) is exact; c3 involves all four,
  //   so a missing one makes g NaN everywhere in the interval.
  //   R.ok = false when v is off the lattice.
  function rowSums(vals, soft, rows, cols, v, R) {
    var i = Math.floor(v), fv = v - i;
    R.ok = i >= -1 && i < rows;
    if (!R.ok) return R;
    var P = R.P, S = R.S, Q = R.Q, cub = R.cub, wv = R.wv, c;
    P.fill(0); S.fill(0); Q.fill(0);
    for (var di = 0; di < 2; di++) {
      var r = i + di;
      if (r < 0 || r >= rows) continue;
      var w = di ? fv : 1 - fv, base = r * cols;
      for (c = 0; c < cols; c++) {
        var x = vals[base + c];
        if (x !== x) continue;               // NaN: no data
        P[c + 1] += w; S[c + 1] += w * x;
        if (soft) Q[c + 1] += w * soft[base + c];
      }
    }
    crWeights(fv, wv, 0);
    var last = rows - 1;
    var b0 = Math.max(0, Math.min(last, i - 1)) * cols, b1 = Math.max(0, Math.min(last, i)) * cols;
    var b2 = Math.max(0, Math.min(last, i + 1)) * cols, b3 = Math.max(0, Math.min(last, i + 2)) * cols;
    var w0 = wv[0], w1 = wv[1], w2 = wv[2], w3 = wv[3];
    for (c = 0; c < cols; c++) cub[c] = w0 * vals[b0 + c] + w1 * vals[b1 + c] + w2 * vals[b2 + c] + w3 * vals[b3 + c];
    var C = R.C, lc = cols - 1;
    for (var b = 0; b <= cols; b++) {
      var j = b - 1;
      var p0 = cub[Math.max(0, Math.min(lc, j - 1))], p1 = cub[Math.max(0, Math.min(lc, j))];
      var p2 = cub[Math.max(0, Math.min(lc, j + 1))], p3 = cub[Math.max(0, Math.min(lc, j + 2))];
      C[4 * b] = p1;
      C[4 * b + 1] = 0.5 * (p2 - p0);
      C[4 * b + 2] = 0.5 * (2 * p0 - 5 * p1 + 4 * p2 - p3);
      C[4 * b + 3] = 0.5 * (3 * p1 - p0 - 3 * p2 + p3);
    }
    return R;
  }

  // Field along the row in R at every column of taps T: gs[x] = g (NaN = no
  // data), as[x] = coverage (scaled by the soft sums in Q when hasSoft).
  // The hot loop. It walks runs of columns in the same lattice interval, so
  // the interval's coefficients stay in locals and a pixel costs only the
  // polynomial; inside the forecast area (both columns full) the coverage
  // needs no smoothstep or division.
  function rowEval(R, T, hasSoft, gs, as) {
    var n = gs.length, none = NaN;         // local: a global lookup per pixel is slow in some engines
    if (!R.ok) { gs.fill(none); as.fill(0); return; }
    var bj = T.bj, fu = T.fu, P = R.P, S = R.S, Q = R.Q, C = R.C;
    var x = 0, end, f, e;
    while (x < n) {
      var b = bj[x];
      for (end = x + 1; end < n && bj[end] === b; end++);
      if (b < 0) { for (; x < end; x++) { gs[x] = none; as[x] = 0; } continue; }
      var p0 = P[b], p1 = P[b + 1], s0 = S[b], s1 = S[b + 1], q0 = hasSoft ? Q[b] : 0, q1 = hasSoft ? Q[b + 1] : 0;
      var o = 4 * b, c0 = C[o], c1 = C[o + 1], c2 = C[o + 2], c3 = C[o + 3];
      var cubic = c3 === c3;               // NaN when one of the 16 has no data
      if (p0 === 1 && p1 === 1) {          // full coverage on both sides
        for (; x < end; x++) {
          f = fu[x]; e = 1 - f;
          as[x] = hasSoft ? (e * q0 + f * q1) / 2 : 1;
          gs[x] = cubic ? ((c3 * f + c2) * f + c1) * f + c0 : e * s0 + f * s1;
        }
        continue;
      }
      for (; x < end; x++) {
        f = fu[x]; e = 1 - f;
        var p = e * p0 + f * p1;
        if (!(p > 0)) { gs[x] = none; as[x] = 0; continue; }
        var a = 2 * p - 1;
        a = a <= 0 ? 0 : a >= 1 ? 1 : a * a * (3 - 2 * a);
        as[x] = hasSoft ? a * (e * q0 + f * q1) / (2 * p) : a;
        gs[x] = cubic ? ((c3 * f + c2) * f + c1) * f + c0 : (e * s0 + f * s1) / p;
      }
    }
  }

  function sample(grid, vals, lat, lon) {
    var R = rowSums(vals, grid.soft, grid.rows, grid.cols, lat / grid.step - 0.5 - grid.i0, rowBufs(grid.cols));
    var T = colTaps(grid.cols, [lon / grid.step - 0.5 - grid.j0]);
    var gs = new Float64Array(1), as = new Float64Array(1);
    rowEval(R, T, !!grid.soft, gs, as);
    return { g: gs[0], a: as[0] };
  }

  // The image columns' taps: computed once per grid (its width never changes).
  function gridTaps(grid) {
    if (grid._taps) return grid._taps;
    var W = grid.w, us = new Float64Array(W);
    for (var x = 0; x < W; x++) us[x] = (grid.west + (grid.east - grid.west) * (x + 0.5) / W) / grid.step - 0.5 - grid.j0;
    return (grid._taps = colTaps(grid.cols, us));
  }

  // Runs fn(y, gs, as) for every image row y with that row's g and coverage
  // (buffers reused from row to row).
  function eachRow(grid, vals, fn) {
    var W = grid.w, H = grid.h, T = gridTaps(grid), R = rowBufs(grid.cols), soft = !!grid.soft;
    var gs = new Float64Array(W), as = new Float64Array(W);
    for (var y = 0; y < H; y++) {
      rowSums(vals, grid.soft, grid.rows, grid.cols, rowLat(y + 0.5, H, grid.north, grid.south) / grid.step - 0.5 - grid.i0, R);
      rowEval(R, T, soft, gs, as);
      fn(y, gs, as);
    }
  }

  // g and coverage per image pixel (row-major), as renderField sees them.
  function fieldValues(grid, vals) {
    var W = grid.w, out = { g: new Float32Array(W * grid.h), a: new Float32Array(W * grid.h) };
    eachRow(grid, vals, function (y, gs, as) { out.g.set(gs, y * W); out.a.set(as, y * W); });
    return out;
  }

  // Fills `out` (RGBA, grid.w x grid.h, all zero) with the field. O(pixels).
  function renderField(grid, vals, lut, out) {
    var W = grid.w, top = LUT_SIZE - 1;
    eachRow(grid, vals, function (y, gs, as) {
      for (var x = 0, o = y * W * 4; x < W; x++, o += 4) {
        var cov = as[x];
        if (!(cov > 0)) continue;
        var ng = -gs[x];
        if (!(ng >= 0)) continue;              // no colour, or NaN
        var n = ng > top ? top : ng | 0;       // floor
        var a = lut[n * 4 + 3];
        if (!a) continue;
        out[o] = lut[n * 4]; out[o + 1] = lut[n * 4 + 1]; out[o + 2] = lut[n * 4 + 2];
        out[o + 3] = a * cov;
      }
    });
    return out;
  }

  // ── Global grid mode: pure helpers ───────────────────────────────────────

  function clamp(v, lo, hi) { return v < lo ? lo : v > hi ? hi : v; }

  // setTimeout, or run now where there is none.
  function later(fn, ms) {
    if (typeof setTimeout === 'function') return setTimeout(fn, ms);
    fn();
    return null;
  }

  // The world copy a view is on: the multiple of 360 nearest its centre
  // longitude (0 for a view centred within -180..180).
  function shiftFor(west, east) {
    var s = Math.round((Number(west) + Number(east)) / 2 / 360) * 360;
    return isFinite(s) ? s + 0 : 0;      // + 0: no -0
  }

  // The box to ask /api/ducting/grid for: the view (south, west, north,
  // east, as Leaflet gives them; longitudes may be on a world copy) padded
  // by `pad` of its size on each side, moved to the -180..180 copy, rounded
  // outward to whole degrees and clamped to [-90, 90] x [-180, 180]; the
  // whole longitude range when the padded view spans 360 degrees or more.
  // { s, w, n, e, shift } (shift = what was taken off the longitudes), or
  // null for a bad or empty view.
  function windowBox(south, west, north, east, pad) {
    south = Number(south); west = Number(west); north = Number(north); east = Number(east);
    if (!isFinite(south) || !isFinite(west) || !isFinite(north) || !isFinite(east) || !(north > south) || !(east > west)) return null;
    pad = pad == null ? WINDOW_PAD : Number(pad);
    if (!(pad >= 0)) pad = 0;
    var dLat = (north - south) * pad, dLon = (east - west) * pad, shift = shiftFor(west, east);
    var s = clamp(Math.floor(south - dLat), -90, 90), n = clamp(Math.ceil(north + dLat), -90, 90);
    var w = -180, e = 180;
    if (east - west + 2 * dLon < 360) {
      w = clamp(Math.floor(west - dLon - shift), -180, 180);
      e = clamp(Math.ceil(east + dLon - shift), -180, 180);
    }
    if (!(n > s) || !(e > w)) return null;
    return { s: s + 0, w: w + 0, n: n + 0, e: e + 0, shift: shift };
  }

  // The step the server answers box with: the finest of the ladder (x the
  // native step) that gives at most maxCells cells, else the coarsest
  // (ducting.Window / windowShape). Only used to tell whether a drawn window
  // can serve a new view at the same detail.
  function windowStep(box, native, maxCells) {
    native = Number(native) > 0 ? Number(native) : 0.5;
    maxCells = maxCells || GRID_MAX_CELLS;
    var st = native;
    for (var k = 0; k < STEP_LADDER.length; k++) {
      st = STEP_LADDER[k] * native;
      var rows = Math.min(Math.ceil(box.n / st), Math.ceil(90 / st)) - Math.max(Math.floor(box.s / st), Math.floor(-90 / st));
      var cols = Math.min(Math.ceil(box.e / st) - Math.floor(box.w / st), Math.max(1, Math.floor(360 / st + 1e-9)));
      if (rows * cols <= maxCells) return st;
    }
    return st;
  }

  function boxCovers(outer, inner) {
    return !!(outer && inner) && outer.s <= inner.s && outer.n >= inner.n && outer.w <= inner.w && outer.e >= inner.e;
  }

  // Least-recently-used map of at most `max` entries.
  function Lru(max) { this.max = max > 0 ? max : 1; this.map = new Map(); }
  Lru.prototype.get = function (k) {
    if (!this.map.has(k)) return undefined;
    var v = this.map.get(k);
    this.map.delete(k);
    this.map.set(k, v);
    return v;
  };
  Lru.prototype.set = function (k, v) {
    this.map.delete(k);
    this.map.set(k, v);
    while (this.map.size > this.max) this.map.delete(this.map.keys().next().value);
  };
  Lru.prototype.has = function (k) { return this.map.has(k); };
  Lru.prototype.clear = function () { this.map.clear(); };
  Object.defineProperty(Lru.prototype, 'size', { get: function () { return this.map.size; } });

  // 'Fri, Nov 20, 14:00 · forecast' after now; '… · analysis, 6 h ago' (or
  // '3 d ago' from 48 h) at or before it. The past steps are what the model
  // made of that time (analysis or a short forecast), not observations.
  function timeLabel(t, nowSec) {
    var s = fmtTime(t) + ' · ';
    if (t > nowSec) return s + 'forecast';
    var h = Math.round((nowSec - t) / HOUR_S);
    if (h < 1) return s + 'analysis, under 1 h ago';
    return s + 'analysis, ' + (h < 48 ? h + ' h' : Math.round(h / 24) + ' d') + ' ago';
  }

  // The deep-linked time: ductT of a #/map hash (whole seconds), else null.
  function hashTime(hash) {
    var U = (typeof window !== 'undefined' && window.URLState) || null;
    if (!U) return null;
    var p = U.parseHash(hash);
    if (p.route !== 'map') return null;
    var v = p.params.ductT;
    return (typeof v === 'string' && /^\d{1,12}$/.test(v)) ? Number(v) : null;
  }

  // `hash` with `updates` applied (null / '' removes a key); null when the
  // hash is not the map page's, so nothing is written elsewhere.
  function hashWith(hash, updates) {
    var U = (typeof window !== 'undefined' && window.URLState) || null;
    if (!U || U.parseHash(hash).route !== 'map') return null;
    return U.updateHashParams(updates, hash);
  }

  function num(v) { return (v != null && v !== '' && isFinite(Number(v))) ? Number(v) : null; }

  // The line kind of a reception: 'duct' (the emphasized colour) when half
  // or more of the path was in the legend's High band or stronger
  // (highFrac: path samples with a mean gradient <= STOP_N[4], -62 N/km),
  // 'plain' when less, 'nodata' without a modelled path gradient (no stored
  // step). Neither the yes/no "duct can carry LoRa" test (almost never true
  // at GFS resolution) nor the class-based super-refractive share
  // (saturated over humid land) decides the colour.
  function linkKind(r) {
    if (!r || num(r.meanGradNkm) == null) return 'nodata';
    var f = frac(r.highFrac);
    return (f != null && f >= LINK_ENHANCED_FRAC) ? 'duct' : 'plain';
  }

  function endOk(e) {
    if (!e) return false;
    var lat = Number(e.lat), lon = Number(e.lon);
    return isFinite(lat) && isFinite(lon) && lat >= -90 && lat <= 90;
  }

  // The links with a reception within winS seconds of t: [{ link, recs (the
  // receptions in the window, nearest first), kind (of the nearest) }].
  // O(links x receptions); the server caps both (500 x 50).
  function linksAt(links, t, winS) {
    var out = [];
    t = Number(t);
    if (!Array.isArray(links) || !isFinite(t)) return out;
    winS = winS == null ? LINK_WINDOW_S : Number(winS);
    for (var i = 0; i < links.length; i++) {
      var L0 = links[i];
      if (!L0 || !endOk(L0.a) || !endOk(L0.b) || !Array.isArray(L0.receptions)) continue;
      var near = [];
      for (var k = 0; k < L0.receptions.length; k++) {
        var r = L0.receptions[k];
        if (r && isFinite(Number(r.t)) && Math.abs(Number(r.t) - t) <= winS) near.push(r);
      }
      if (!near.length) continue;
      near.sort(function (x, y) { return (Math.abs(x.t - t) - Math.abs(y.t - t)) || (x.t - y.t); });
      out.push({ link: L0, recs: near, kind: linkKind(near[0]) });
    }
    return out;
  }

  // [[latA, lonA], [latB, lonB]] moved east by shift, B on the side of the
  // antimeridian nearest A (a link across it is not drawn round the world).
  function linkLatLngs(link, shift) {
    var la = Number(link.a.lon), lb = Number(link.b.lon);
    lb += Math.round((la - lb) / 360) * 360;
    shift = shift || 0;
    return [[Number(link.a.lat), la + shift], [Number(link.b.lat), lb + shift]];
  }

  // Distance in px from point p to segment ab ({ x, y } each).
  function segDist(p, a, b) {
    var dx = b.x - a.x, dy = b.y - a.y, l2 = dx * dx + dy * dy;
    var u = l2 > 0 ? clamp(((p.x - a.x) * dx + (p.y - a.y) * dy) / l2, 0, 1) : 0;
    var x = a.x + u * dx - p.x, y = a.y + u * dy - p.y;
    return Math.sqrt(x * x + y * y);
  }

  function plural(n, word) { return n + ' ' + word + (n === 1 ? '' : 's'); }

  function int0(v) { v = Math.round(Number(v)); return isFinite(v) && v > 0 ? v : 0; }

  // 1234 -> '1,234'.
  function fmtInt(n) { return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ','); }

  function pctOf(f) { return Math.round(f * 100) + '%'; }

  // A share 0..1, else null (missing, null or -1 = no data).
  function frac(v) { v = num(v); return (v != null && v >= 0 && v <= 1) ? v : null; }

  // '-58 N/km' with a real minus sign.
  function fmtGrad(v) { return fmtSigned(Math.round(v)) + ' N/km'; }

  // The server reports losMethod in the summary ("terrain" or
  // "smooth-earth"); a top-level field is accepted too.
  function linksMethod(ld) {
    if (!ld) return '';
    return String(ld.losMethod || (ld.summary && ld.summary.losMethod) || '');
  }

  // What makes a pair count: no terrain line of sight (losMethod
  // "terrain"), else beyond the smooth-earth radio horizon.
  function losPhrase(method) { return method === 'terrain' ? 'without terrain line of sight' : 'beyond the radio horizon'; }

  // The legend's period summary: only what the numbers say, leading with
  // the modelled path gradient when heard against the same paths over the
  // whole period, then the share of receptions with half or more of the
  // path in the High band or stronger (each comparison only when both
  // values exist), the LoRa-duct count only when there is one, and the
  // pairs whose line of sight is not checked yet.
  function linksSummary(sum, days, method) {
    if (!sum) return '';
    days = int0(days);
    var period = days === 1 ? 'Last 24 hours' : days ? 'Last ' + days + ' days' : 'Recently';
    var rec = int0(sum.receptions), pairs = int0(sum.links), dd = int0(sum.duringDuct), unv = int0(sum.unverifiedPairs);
    var out = [];
    if (!rec) out.push(period + ': no receptions between node pairs ' + losPhrase(method) + '.');
    else {
      out.push(period + ': ' + fmtInt(rec) + ' reception' + (rec === 1 ? '' : 's') + ' on ' + fmtInt(pairs) + ' node pair' +
        (pairs === 1 ? '' : 's') + ' ' + losPhrase(method) + '.');
      var g = num(sum.recvMeanGradNkm), gb = num(sum.baselineMeanGradNkm);
      var e = frac(sum.recvEnhancedFrac), eb = frac(sum.baselineEnhancedFrac);
      var grad = g == null ? '' : 'Along their paths the modelled refractivity gradient averaged ' + fmtGrad(g) + ' when heard' +
        (gb == null ? '' : ', against ' + fmtGrad(gb) + ' for the same paths over the whole period');
      var enh = e == null ? '' : 'half or more of the path was in the High band or stronger for ' + pctOf(e) + ' of receptions' +
        (eb == null ? '' : ' vs ' + pctOf(eb) + ' overall');
      if (grad && enh) out.push(grad + '; ' + enh + '.');
      else if (grad) out.push(grad + '.');
      else if (enh) out.push(enh.charAt(0).toUpperCase() + enh.slice(1) + '.');
      if (dd) out.push('Half or more of the path was in a modelled duct that may carry LoRa for ' + fmtInt(dd) + ' reception' + (dd === 1 ? '' : 's') + '.');
    }
    if (unv) out.push(fmtInt(unv) + ' more pair' + (unv === 1 ? '' : 's') + ' not checked yet.');
    return out.join(' ');
  }

  // A node's name for display (plain text, capped), else its key prefix.
  function nodeName(e) {
    var n = e && typeof e.name === 'string' ? e.name.trim() : '';
    if (n) return n.length > NAME_MAX ? n.slice(0, NAME_MAX - 1) + '…' : n;
    var k = e && typeof e.pubkey === 'string' ? e.pubkey.slice(0, 8) : '';
    return k ? k + '…' : 'Unnamed node';
  }

  function fmtKm(v) { v = Number(v); return isFinite(v) ? Math.round(v) + ' km' : '?'; }

  function fmtHeight(e) {
    var h = Number(e && e.heightM);
    return (isFinite(h) ? Math.round(h) + ' m' : '?') + (e && e.heightKnown ? '' : ' (site default)');
  }

  function fmtNum(v, digits) {
    var s = Number(v).toFixed(digits);
    return s.charAt(0) === '-' ? MINUS + s.slice(1) : s;
  }

  // How a reception was heard (the API's via); nothing for an unknown value.
  var VIA_TEXT = { observer: 'heard directly by the observer', relay: 'relayed: the second repeater heard the first directly' };

  // One reception: time, how it was heard, SNR/RSSI when present (a relay
  // reception has none), the modelled path gradient and the share of the
  // path in the High band or stronger, then (secondary) the class-based
  // super-refractive share; the LoRa-duct share only when there is one.
  function recLine(r) {
    var parts = [fmtTime(Number(r.t))];
    if (typeof r.via === 'string' && Object.prototype.hasOwnProperty.call(VIA_TEXT, r.via)) parts.push(VIA_TEXT[r.via]);
    if (r.snr != null && isFinite(Number(r.snr))) parts.push('SNR ' + fmtNum(r.snr, 1) + ' dB');
    if (r.rssi != null && isFinite(Number(r.rssi))) parts.push('RSSI ' + fmtNum(r.rssi, 0) + ' dBm');
    var g = num(r.meanGradNkm), hf = frac(r.highFrac), sf = frac(r.superFrac), df = frac(r.ductFrac);
    if (g == null && hf == null && sf == null) {
      parts.push('no model data for this time');
      return parts.join(' \u00b7 ');
    }
    if (g != null) parts.push('path gradient ' + fmtGrad(g));
    if (hf != null) parts.push('High band or stronger along ' + pctOf(hf) + ' of the path');
    if (sf != null) parts.push('super-refractive along ' + pctOf(sf));
    if (df != null && df > 0) parts.push('LoRa duct along ' + pctOf(df));
    return parts.join(' \u00b7 ');
  }

  // Popup body of a drawn link (DOM, textContent only: names are chosen by
  // whoever runs the node). method: the answer's losMethod.
  function linkPopupContent(item, freqMHz, method) {
    var L0 = item.link, box = el('div', 'mc-duct-popup mc-duct-link-popup');
    box.appendChild(el('div', 'mc-duct-popup-title', 'Long link, heard directly'));
    box.appendChild(el('div', 'mc-duct-link-end', 'Sent by: ' + nodeName(L0.a)));
    box.appendChild(el('div', 'mc-duct-link-end', 'Heard by: ' + nodeName(L0.b)));
    box.appendChild(el('div', null, fmtKm(L0.distKm) + ' apart; ' +
      (method === 'terrain' ? 'no terrain line of sight (smooth-earth radio horizon ' + fmtKm(L0.horizonKm) + ')' : 'radio horizon ' + fmtKm(L0.horizonKm))));
    box.appendChild(el('div', null, 'Antenna heights: sender ' + fmtHeight(L0.a) + ', receiver ' + fmtHeight(L0.b)));
    box.appendChild(el('div', 'mc-duct-popup-sub', 'Heard near the selected time:'));
    var list = el('ul', 'mc-duct-link-recs'), lora = false;
    for (var i = 0; i < item.recs.length && i < LINK_MAX_RECS; i++) {
      list.appendChild(el('li', null, recLine(item.recs[i])));
      if ((frac(item.recs[i].ductFrac) || 0) > 0) lora = true;
    }
    box.appendChild(list);
    var count = int0(L0.count);
    if (count) box.appendChild(el('div', 'mc-duct-popup-note', 'Heard ' + plural(count, 'time') + ' in the period'));
    var f = Number(freqMHz);
    box.appendChild(el('div', 'mc-duct-popup-note', 'Path gradient: the model\u2019s mean refractivity gradient along the line (more negative bends more). High band: ' +
      fmtGrad(STOP_N[4]) + ' or stronger, as in the legend. Shares are of the points along the line.' +
      (lora ? ' LoRa duct: a modelled duct that may carry ' + (f > 0 ? fmtMHz(f) : 'LoRa') + '.' : '')));
    return box;
  }

  function linkId(link) { return String(link.a && link.a.pubkey) + '>' + String(link.b && link.b.pubkey); }

  // ── Layer ─────────────────────────────────────────────────────────────────

  // Leaflet 1.9.4's canvas _updatePaths (run on every map move) redraws at
  // once but leaves an already queued animation-frame redraw in place, its id
  // dropped. If the overlay is switched off before that frame, removing the
  // renderer cannot cancel it, and the orphaned callback draws to the deleted
  // context ("reading 'clearRect'"). This renderer cancels the queued frame
  // before redrawing.
  var CanvasClass = null;
  function newRenderer(pane) {
    if (!CanvasClass && L.Canvas && typeof L.Canvas.extend === 'function') {
      CanvasClass = L.Canvas.extend({
        _updatePaths: function () {
          if (!this._postponeUpdatePaths && this._redrawRequest) {
            L.Util.cancelAnimFrame(this._redrawRequest);
            this._redrawRequest = null;
          }
          L.Canvas.prototype._updatePaths.call(this);
        }
      });
    }
    var o = { padding: 0.5 };
    if (pane) o.pane = pane;
    return CanvasClass ? new CanvasClass(o) : L.canvas(o);
  }

  // An image overlay whose image is a canvas: the field is drawn straight
  // into it (no data URL encode/decode), and Leaflet positions and scales it
  // like any image overlay. null when this Leaflet has no ImageOverlay.
  var FieldClass = null;
  function newField(canvas, bounds, pane) {
    if (!FieldClass && L.ImageOverlay && typeof L.ImageOverlay.extend === 'function') {
      FieldClass = L.ImageOverlay.extend({
        _initImage: function () {
          var img = this._image = this._url;
          L.DomUtil.addClass(img, 'leaflet-image-layer');
          if (this._zoomAnimated) L.DomUtil.addClass(img, 'leaflet-zoom-animated');
          if (this.options.className) L.DomUtil.addClass(img, this.options.className);
          img.onselectstart = L.Util.falseFn;
          img.onmousemove = L.Util.falseFn;
        }
      });
    }
    if (!FieldClass) return null;
    var opts = { interactive: false, className: 'mc-duct-field' };
    if (pane) opts.pane = pane;
    var f = new FieldClass(canvas, bounds, opts);
    f._mcField = true;
    return f;
  }

  // A pane of the overlay's own at z-index z that never takes pointer
  // events: FIELD_PANE (the field: above the tiles, below the tile overlays,
  // vectors and markers) and LINE_PANE (the outlines).
  function inertPane(map, name, z) {
    try {
      if (map.getPane(name)) return name;
      var p = map.createPane(name);
      p.style.zIndex = z;
      p.style.pointerEvents = 'none';
      return name;
    } catch (_) { return null; }
  }

  // True when the click landed on another layer's interactive element: an
  // SVG path or marker, or a canvas that is hovering one of its interactive
  // paths (Leaflet marks both with leaflet-interactive; it is also what
  // shows the pointer cursor).
  function hitInteractive(e) {
    var t = e && e.originalEvent && e.originalEvent.target;
    return !!(t && t.classList && typeof t.classList.contains === 'function' && t.classList.contains('leaflet-interactive'));
  }

  function gridBounds(g) { return [[g.south, g.west], [g.north, g.east]]; }

  function makeLayerClass() {
    return L.Layer.extend({
      initialize: function (opts) {
        L.setOptions(this, opts || {});
        this._mcDuct = true;
        this._gen = 0;
        this._cells = [];
        this._data = null;
        this._state = 'loading';   // loading | pending | error | ok
        this._timer = null;
        this._idx = -1;
        this._userPicked = false;
        // Grid mode (kept across remove/re-add like _data):
        this._meta = null;         // /api/ducting/meta answer held
        this._sel = -1;            // index into _meta.times shown
        this._links = null;        // /api/ducting/links answer held
        this._linksState = 'loading';
        this._lru = new Lru(LRU_MAX);
        this._dataKey = '';        // key and box of the window in _data
        this._dataBox = null;
      },

      onAdd: function (map) {
        this._map = map;
        var gen = ++this._gen;
        this._gridMode = isGridMode();
        this._cells = [];
        this._edge = this._lora = null;
        this._edgeColor = this._loraKey = '';
        this._grid = null;
        this._pane = inertPane(map, FIELD_PANE, FIELD_Z);
        try {
          this._renderer = newRenderer(inertPane(map, LINE_PANE, LINE_Z));
          this._group = L.layerGroup();
          map.addLayer(this._group);
        } catch (_) { this._group = null; }
        this._addControls(map);
        this._watchAttribution(map);
        this._watchClicks(map, gen);
        if (this._gridMode) this._addGrid(map, gen);
        else {
          // Kept data is drawn at once, so a re-add never shows a blank map while
          // the refetch is pending or failing; the refetch below replaces it.
          if (this._data) this._show();
          else this._renderLegend();
          this._load(gen);
          this._schedule(REFRESH_MS);
        }
        var self = this;
        // Colours are read from CSS variables, so a theme switch needs a redraw.
        this._onTheme = function () {
          if (self._data) self._restyle();
          if (self._gridMode) self._drawLinks();
        };
        try { window.addEventListener('theme-changed', this._onTheme); } catch (_) {}
      },

      onRemove: function (map) {
        this._gen++;                       // in-flight results are ignored
        if (this._timer != null) { try { clearInterval(this._timer); } catch (_) {} this._timer = null; }
        this._timerMs = 0;
        if (this._abort) { try { this._abort.abort(); } catch (_) {} this._abort = null; }
        var m = map || this._map;
        if (this._onTheme) { try { window.removeEventListener('theme-changed', this._onTheme); } catch (_) {} this._onTheme = null; }
        if (this._slider && this._onSlide) {
          try { this._slider.removeEventListener('input', this._onSlide); } catch (_) {}
        }
        this._unwatchAttribution(m);
        this._unwatchClicks(m);
        if (this._gridMode) this._removeGrid(m);
        if (this._legendCtl) { try { this._legendCtl.remove(); } catch (_) {} }
        if (this._timeCtl) { try { this._timeCtl.remove(); } catch (_) {} }
        if (this._group && m) { try { m.removeLayer(this._group); } catch (_) {} }
        if (this._field && m) { try { m.removeLayer(this._field); } catch (_) {} }
        if (this._renderer && m) { try { m.removeLayer(this._renderer); } catch (_) {} }
        this._map = this._group = this._renderer = this._field = this._canvas = this._grid = this._img = null;
        this._legendCtl = this._timeCtl = this._legendEl = this._statusEl = this._metaEl = null;
        this._slider = this._timeLabel = this._onSlide = null;
        this._cells = [];
        this._edge = this._lora = null;
        this._loraKey = '';
      },

      // ── map clicks ──
      // One map click handler instead of interactive cells: an interactive
      // layer with a popup stops the click (Leaflet's popup opener does), so
      // the page's own map click handlers would never run in the forecast
      // area. This one never stops anything; it runs after every other
      // handler of the same click (a microtask, so the whole synchronous
      // Leaflet dispatch is over) and opens the cell popup only when the
      // click did nothing else:
      //   - not on another layer's interactive element (hitInteractive);
      //   - no layer was added and no popup opened since the click's
      //     preclick: Planner placing a site or chain point, Live opening a
      //     path popup. Clicks the page uses keep the map to themselves.
      _watchClicks: function (map, gen) {
        var self = this, changes = 0, mark = -1;
        this._onMapChange = function () { changes++; };
        this._onPreclick = function () { mark = changes; };
        this._onMapClick = function (e) {
          var since = mark >= 0 ? mark : changes;
          mark = -1;
          if (!e || !e.latlng || hitInteractive(e)) return;
          var latlng = e.latlng;
          Promise.resolve().then(function () {
            if (gen !== self._gen || changes !== since) return;
            if (!self.openLinkPopup(latlng)) self.openCellPopup(latlng);
          });
        };
        try {
          map.on('layeradd popupopen', this._onMapChange);
          map.on('preclick', this._onPreclick);
          map.on('click', this._onMapClick);
        } catch (_) {}
      },

      _unwatchClicks: function (map) {
        if (map && this._onMapClick) {
          try {
            map.off('layeradd popupopen', this._onMapChange);
            map.off('preclick', this._onPreclick);
            map.off('click', this._onMapClick);
          } catch (_) {}
        }
        if (map && this._popup) { try { map.closePopup(this._popup); } catch (_) {} }
        this._onMapChange = this._onPreclick = this._onMapClick = null;
        this._popup = this._popupCell = null;
      },

      // Opens the popup of the cell under latlng at the shown hour; false
      // (nothing opened) outside the forecast area or where that cell has no
      // data at this hour.
      openCellPopup: function (latlng) {
        var map = this._map, d = this._data;
        if (!map || !d || !latlng || !L.popup) return false;
        var k = ductCellAt(this._grid, latlng.lat, latlng.lng);
        var cell = k >= 0 ? d.cells[k] : null;
        if (!cell || at(cell.c, this._idx) < 1) return false;
        try {
          if (!this._popup) this._popup = L.popup({ maxWidth: 280 });
          this._popupCell = cell;
          this._popup.setLatLng(latlng).setContent(popupContent(cell, this._idx)).openOn(map);
          return true;
        } catch (_) { return false; }
      },

      _popupOpen: function () {
        try { return !!(this._popup && this._popup.isOpen()); } catch (_) { return false; }
      },

      // Keeps an open cell popup in step with the shown hour and data: new
      // text for the cell under it, closed where there is no data any more.
      _syncPopup: function () {
        if (!this._popupOpen()) return;
        var d = this._data, ll = this._popup.getLatLng();
        var k = ll ? ductCellAt(this._grid, ll.lat, ll.lng) : -1;
        var cell = (d && k >= 0) ? d.cells[k] : null;
        if (!cell || at(cell.c, this._idx) < 1) {
          try { this._map.closePopup(this._popup); } catch (_) {}
          return;
        }
        this._popupCell = cell;
        this._popup.setContent(popupContent(cell, this._idx));
      },

      // ── controls ──
      _addControls: function (map) {
        var self = this;
        try {
          var timeCtl = new L.Control({ position: 'bottomleft' });
          timeCtl.onAdd = function () {
            var box = el('div', 'mc-duct-time');
            var label = el('label', 'mc-duct-time-label', self._gridMode ? 'Time' : 'Forecast time');
            label.setAttribute('for', 'mc-duct-time-input');
            var slider = el('input');
            slider.type = 'range';
            slider.id = 'mc-duct-time-input';
            slider.min = '0';
            slider.max = '0';
            slider.step = '1';
            slider.value = '0';
            slider.disabled = true;
            slider.setAttribute('aria-label', self._gridMode ? 'Ducting map time' : 'Ducting forecast time');
            self._onSlide = function () {
              var i = parseInt(slider.value, 10);
              if (!isFinite(i)) return;
              if (self._gridMode) self._pickTime(i);
              else { self._userPicked = true; self._setIndex(i); }
            };
            slider.addEventListener('input', self._onSlide);
            box.appendChild(label);
            box.appendChild(slider);
            self._timeLabel = label;
            self._slider = slider;
            if (L.DomEvent && L.DomEvent.disableClickPropagation) L.DomEvent.disableClickPropagation(box);
            return box;
          };
          this._timeCtl = timeCtl;
          timeCtl.addTo(map);

          var legendCtl = new L.Control({ position: 'bottomleft' });
          legendCtl.onAdd = function () {
            var box = el('div', 'mc-duct-legend');
            self._legendEl = box;
            if (L.DomEvent && L.DomEvent.disableClickPropagation) L.DomEvent.disableClickPropagation(box);
            // It scrolls when capped (_fitLegend): the wheel scrolls it, not the map.
            if (L.DomEvent && L.DomEvent.disableScrollPropagation) L.DomEvent.disableScrollPropagation(box);
            return box;
          };
          this._legendCtl = legendCtl;
          legendCtl.addTo(map);
        } catch (_) { /* map already gone */ }
      },

      // The bottom-left corner shares the map's bottom edge with Leaflet's
      // attribution bar, which wraps to several lines on narrow screens.
      // Lift the corner by the bar's measured height whenever the two overlap
      // horizontally; re-measured on resize, layer changes and bar resizes.
      _watchAttribution: function (map) {
        var self = this;
        this._lift = function () { self._applyLift(); };
        try { map.on('resize layeradd layerremove', this._lift); } catch (_) {}
        try {
          var attr = this._attrEl();
          if (attr && typeof ResizeObserver === 'function') {
            this._ro = new ResizeObserver(this._lift);
            this._ro.observe(attr);
          }
        } catch (_) {}
        this._applyLift();
      },

      _unwatchAttribution: function (map) {
        if (this._ro) { try { this._ro.disconnect(); } catch (_) {} this._ro = null; }
        if (this._lift && map) { try { map.off('resize layeradd layerremove', this._lift); } catch (_) {} }
        if (this._corner) { try { this._corner.style.marginBottom = ''; } catch (_) {} }
        this._lift = this._corner = null;
      },

      _attrEl: function () {
        var ac = this._map && this._map.attributionControl;
        return (ac && typeof ac.getContainer === 'function') ? ac.getContainer() : null;
      },

      _applyLift: function () {
        var box = this._legendEl, attr = this._attrEl();
        var corner = box && box.parentNode;
        if (!corner || !corner.style) return;
        this._corner = corner;
        var h = 0;
        try {
          if (attr && attr.offsetHeight) {
            var a = attr.getBoundingClientRect(), c = corner.getBoundingClientRect();
            // Overlap only if the bar reaches left over the ducting controls.
            if (a.left < c.right) h = attr.offsetHeight;
          }
        } catch (_) { h = 0; }
        corner.style.marginBottom = h ? h + 'px' : '';
        this._fitLegend();
      },

      // Caps the legend's height so it ends LEGEND_GAP below the lowest
      // control of the top-left corner (measured, so any page's controls
      // count). The legend's bottom does not move with its height (the
      // corner stacks from the bottom), so one measurement is enough.
      _fitLegend: function () {
        var box = this._legendEl, map = this._map;
        if (!box || !box.style || !map) return;
        try {
          var cont = map.getContainer(), top = cont.getBoundingClientRect().top;
          var tl = cont.querySelector('.leaflet-top.leaflet-left');
          var kids = tl ? tl.children : [];
          for (var i = 0; i < kids.length; i++) {
            var r = kids[i].getBoundingClientRect();
            if (r.height > 0 && r.bottom > top) top = r.bottom;
          }
          var avail = Math.floor(box.getBoundingClientRect().bottom - top - LEGEND_GAP);
          box.style.maxHeight = Math.max(LEGEND_MIN_H, avail) + 'px';
        } catch (_) { /* not laid out (no map container): leave it */ }
      },

      _renderLegend: function () {
        var box = this._legendEl;
        if (!box) return;
        var self = this;
        if (this._legendOpen == null) {
          // Compact by default on phones so the legend does not cover the map.
          var narrow = false;
          try { narrow = !!(window.matchMedia && window.matchMedia('(max-width: 600px)').matches); } catch (_) {}
          this._legendOpen = !narrow;
        }
        box.textContent = '';
        var head = el('button', 'mc-duct-legend-title', 'Tropospheric ducting');
        head.type = 'button';
        head.setAttribute('aria-expanded', this._legendOpen ? 'true' : 'false');
        head.addEventListener('click', function () { self._legendOpen = !self._legendOpen; self._refocus = 'title'; self._renderLegend(); });
        box.appendChild(head);
        this._linkCountEl = null;
        this._linksToggle = null;
        if (!this._legendOpen) { this._afterLegend(head); return; }
        var grid = this._gridMode;
        // Grid mode: the meta answer is what is "held"; windows come and go.
        var d = grid ? this._meta : this._data;
        if (!d) {
          var st = el('div', 'mc-duct-legend-status', this._state === 'pending' ? MSG_PENDING : this._state === 'error' ? MSG_ERROR : 'Loading ducting forecast');
          st.setAttribute('role', 'status');
          box.appendChild(st);
        } else {
          if (this._stale || (grid && this._winErr)) {
            var old = el('div', 'mc-duct-legend-status', this._stale ? MSG_STALE : MSG_WINDOW_ERROR);
            old.setAttribute('role', 'status');
            box.appendChild(old);
          }
          box.appendChild(this._scale());
          var lrow = el('div', 'mc-duct-legend-row');
          lrow.appendChild(el('span', 'mc-duct-swatch mc-duct-swatch-lora'));
          lrow.appendChild(el('span', null, msgLoraRow()));
          box.appendChild(lrow);
          if (grid) {
            // Global coverage: no forecast-area edge to explain.
            box.appendChild(el('div', 'mc-duct-legend-note', MSG_PAST));
            box.appendChild(this._linksLegend());
          } else {
            var erow = el('div', 'mc-duct-legend-row');
            erow.appendChild(el('span', 'mc-duct-swatch mc-duct-swatch-edge'));
            erow.appendChild(el('span', null, 'Dotted line: edge of the forecast area (no forecast beyond it)'));
            box.appendChild(erow);
          }
          var meta = d.attribution || d.modelLabel || d.model || '';
          if (meta) box.appendChild(el('div', 'mc-duct-legend-meta', meta));
          if (d.generatedAt) box.appendChild(el('div', 'mc-duct-legend-meta', 'Updated ' + fmtTime(d.generatedAt)));
        }
        box.appendChild(el('div', 'mc-duct-legend-note', NOTE));
        this._afterLegend(head);
      },

      // After a legend rebuild: the toggle that rebuilt it keeps the focus
      // (its button is a new element), and the height cap is re-measured.
      _afterLegend: function (head) {
        var f = this._refocus;
        this._refocus = null;
        var target = f === 'title' ? head : f === 'links' ? this._linksToggle : null;
        if (target && typeof target.focus === 'function') { try { target.focus(); } catch (_) {} }
        this._fitLegend();
      },

      // "Observed long links": the line key, the period summary, how many
      // are drawn at the shown time (kept in _linkCountEl, updated in place
      // when the time moves) and the caveat about positions.
      // Folded (_linksOpen false; the default on short screens) it keeps
      // the period summary, the count at the shown time and the caveat,
      // and drops the line key.
      _linksLegend: function () {
        var self = this, sec = el('div', 'mc-duct-links');
        if (this._linksOpen == null) {
          var short = false;
          try { short = !!(window.matchMedia && window.matchMedia(SHORT_SCREEN).matches); } catch (_) {}
          this._linksOpen = !short;
        }
        var ld = this._links;
        if (!ld) {
          sec.appendChild(el('div', 'mc-duct-legend-subtitle', 'Observed long links'));
          sec.appendChild(el('div', 'mc-duct-legend-meta', this._linksState === 'loading' ? 'Loading long links' : 'Long links are not available right now'));
          return sec;
        }
        var tog = el('button', 'mc-duct-legend-title mc-duct-links-toggle', 'Observed long links');
        tog.type = 'button';
        tog.setAttribute('aria-expanded', this._linksOpen ? 'true' : 'false');
        tog.addEventListener('click', function () { self._linksOpen = !self._linksOpen; self._refocus = 'links'; self._renderLegend(); });
        this._linksToggle = tog;
        sec.appendChild(tog);
        var rows = this._linksOpen ? [
          ['mc-duct-swatch-link-duct', 'Half or more of the path in the High band or stronger (model)'],
          ['mc-duct-swatch-link', 'Less of the path in the High band'],
          ['mc-duct-swatch-link-nodata', 'Dashed: no model data for that time']
        ] : [];
        for (var i = 0; i < rows.length; i++) {
          var r = el('div', 'mc-duct-legend-row');
          r.appendChild(el('span', 'mc-duct-swatch-line ' + rows[i][0]));
          r.appendChild(el('span', null, rows[i][1]));
          sec.appendChild(r);
        }
        sec.appendChild(el('div', 'mc-duct-links-summary', linksSummary(ld.summary, ld.days, linksMethod(ld))));
        this._linkCountEl = el('div', 'mc-duct-legend-meta');
        this._linkCountEl.setAttribute('aria-live', 'polite');
        this._updateLinkCount();
        sec.appendChild(this._linkCountEl);
        sec.appendChild(el('div', 'mc-duct-legend-note', MSG_POSITIONS));
        return sec;
      },

      _updateLinkCount: function () {
        var c = this._linkCountEl;
        if (!c) return;
        var n = this._shownLinks ? this._shownLinks.length : 0;
        c.textContent = n ? 'On the map: ' + plural(n, 'link') + ' heard within 90 min of the selected time. Click one for details.'
          : 'None heard within 90 min of the selected time.';
      },

      // Bar of the same colour bands as the field (hard stops: each colour
      // starts where the previous ends), labels under it.
      _scale: function () {
        var wrap = el('div', 'mc-duct-scale');
        var bar = el('div', 'mc-duct-scale-bar');
        var parts = ['transparent 0%', 'transparent ' + pct(BAR_POS[0])];
        for (var k = 0; k < STOP_N.length; k++) {
          var c = 'var(--mc-duct-s' + (k + 1) + ') ';
          parts.push(c + pct(BAR_POS[k]), c + pct(k + 1 < BAR_POS.length ? BAR_POS[k + 1] : 1));
        }
        bar.style.background = 'linear-gradient(to right, ' + parts.join(', ') + ')';
        bar.setAttribute('aria-hidden', 'true');
        wrap.appendChild(bar);
        var labels = el('div', 'mc-duct-scale-labels');
        for (var i = 0; i < BAR_LABELS.length; i++) {
          var b = BAR_LABELS[i];
          var lab = el('span', 'mc-duct-scale-label' + (b[2] ? ' mc-duct-scale-label-end' : ''), b[1]);
          lab.style.left = pct(b[0]);
          // A label wraps rather than run into the next one ("Above normal").
          var next = BAR_LABELS[i + 1];
          if (!b[2] && next && !next[2]) lab.style.maxWidth = pct(next[0] - b[0]);
          labels.appendChild(lab);
        }
        wrap.appendChild(labels);
        wrap.appendChild(el('div', 'mc-duct-legend-caption', CAPTION));
        return wrap;
      },

      // ── data ──
      _load: function (gen) {
        var self = this;
        if (gen !== this._gen) return;
        if (this._gridMode) { this._loadMeta(gen); this._loadLinks(gen); return; }
        var opts = { headers: { Accept: 'application/json' } };
        if (typeof AbortController === 'function') {
          if (this._abort) { try { this._abort.abort(); } catch (_) {} }
          this._abort = new AbortController();
          opts.signal = this._abort.signal;
        }
        var p;
        try {
          p = CSApi.fetch(API_URL, opts).then(function (r) {
            if (!r || r.ok === false) throw new Error('http');
            return r.json();
          });
        } catch (_) { p = Promise.reject(new Error('fetch')); }
        p.then(function (d) {
          if (gen !== self._gen) return;
          self._apply(d);
        }, function () {
          if (gen !== self._gen) return;
          self._fail('error');
        });
      },

      // Re-arms the refresh interval when its period changes.
      _schedule: function (ms) {
        if (this._timer != null && this._timerMs === ms) return;
        if (this._timer != null) { try { clearInterval(this._timer); } catch (_) {} }
        var self = this;
        this._timerMs = ms;
        this._timer = setInterval(function () { self._load(self._gen); }, ms);
      },

      // With data held, the last good forecast stays drawn: an error answer
      // (the server's forecast failed or expired) or a failed request marks
      // it out of date; pending does not (a restarting server answers
      // pending while the forecast held here is still valid).
      _fail: function (state) {
        if (this._data) {
          this._setStale(state === 'error' || this._stale);
          return;
        }
        this._schedule(RETRY_MS);
        this._state = state;
        this._renderLegend();
      },

      // The held forecast has run out: its last hour has ended.
      _expired: function () {
        var t = this._gridMode ? (this._meta && this._meta.times) : (this._data && this._data.times);
        var span = this._gridMode ? GRID_STEP_S : HOUR_S;
        return !!(t && t.length && Number(t[t.length - 1]) + span <= Date.now() / 1000);
      },

      // Out-of-date flag (always on once the forecast has run out); the
      // legend is redrawn only when it changes.
      _setStale: function (on) {
        on = !!on || this._expired();
        if (on === !!this._stale) return;
        this._stale = on;
        this._renderLegend();
      },

      _apply: function (d) {
        if (!d || typeof d !== 'object') return this._fail('error');
        if (d.status === 'pending') return this._fail('pending');
        if (d.status !== 'ok' || !Array.isArray(d.cells) || !Array.isArray(d.times) || !d.times.length) return this._fail('error');
        this._state = 'ok';
        this._schedule(REFRESH_MS);
        var held = this._data;
        if (held && d.generatedAt != null && held.generatedAt === d.generatedAt) {
          // The forecast already drawn: no rebuild (an open popup stays
          // open); only the hour moves on, unless the user picked one.
          this._setStale(false);
          if (!this._userPicked) {
            var idx = ductTimeIndex(held.times, Date.now() / 1000);
            if (idx !== this._idx) this._setIndex(idx);
          }
          return;
        }
        this._data = d;
        this._stale = this._expired();
        this._show();
      },

      // Draws the held forecast: cells, slider range, legend and the hour (a
      // user-picked one while it is still in the forecast, else the latest <= now).
      _show: function () {
        var d = this._data;
        var prevTime = this._shown != null ? this._shown : null;
        this._build();
        var idx = ductTimeIndex(d.times, Date.now() / 1000);
        if (this._userPicked && prevTime != null) {
          var keep = d.times.indexOf(prevTime);
          if (keep >= 0) idx = keep;
        }
        if (this._slider) {
          this._slider.max = String(d.times.length - 1);
          this._slider.disabled = d.times.length < 2;
        }
        this._renderLegend();
        this._setIndex(idx);
      },

      // The cells that have data (class >= 1) at some hour, the lattice the
      // field is drawn on and clicks are looked up in, and the area's edge.
      // The LoRa outlines follow in _restyle.
      _build: function () {
        var d = this._data, group = this._group;
        if (!group) return;
        try { group.clearLayers(); } catch (_) {}
        this._cells = [];
        this._edge = this._lora = null;
        this._loraKey = '';
        this._grid = ductGrid(d.cells, Number(d.stepDeg) || 0.5, this._gridMode ? this._winShift : 0);
        for (var i = 0; i < d.cells.length; i++) {
          var cell = d.cells[i];
          if (!cell || !Array.isArray(cell.c) || !isFinite(cell.lat) || !isFinite(cell.lon)) continue;
          for (var t = 0; t < cell.c.length; t++) if (cell.c[t] >= 1) { this._cells.push(cell); break; }
        }
        // A grid window's border is where the fetch stopped, not where the
        // data ends: no edge line.
        if (!this._gridMode) this._buildEdge(group);
      },

      // The forecast area's outline: one non-interactive multi-part polyline
      // (a part per merged run of boundary cell edges), on the shared canvas.
      _buildEdge: function (group) {
        var segs = ductEdges(this._grid);
        if (!segs.length || !L.polyline) return;
        var color = cssVar(EDGE_VAR);
        try {
          var line = L.polyline(segs, L.extend({ renderer: this._renderer, color: color || undefined }, EDGE_STYLE, { stroke: !!color }));
          line._mcEdge = true;
          group.addLayer(line);
          this._edge = line;
          this._edgeColor = color;
        } catch (_) { this._edge = null; }
      },

      // ── grid mode ──
      // Held meta and window are drawn at once on a re-add (no blank map
      // while the refetch is pending or failing); the view decides whether
      // the held window still fits.
      _addGrid: function (map, gen) {
        var self = this;
        this._winKey = '';
        this._winShift = 0;
        this._linksKey = '';
        this._shownLinks = [];
        this._winErr = false;
        try { this._linkGroup = L.layerGroup(); map.addLayer(this._linkGroup); } catch (_) { this._linkGroup = null; }
        try { this._wantT = hashTime(location.hash); } catch (_) { this._wantT = null; }
        this._onView = function () {
          if (gen !== self._gen) return;
          self._queueWindow();
          self._drawLinks();
        };
        try { map.on('moveend zoomend', this._onView); } catch (_) {}
        if (this._data && this._dataBox) this._applyWindow(this._data, this._dataKey, this._dataBox);
        if (this._meta) this._showMeta(null);
        else this._renderLegend();
        this._load(gen);
        this._schedule(this._meta ? GRID_REFRESH_MS : RETRY_MS);
      },

      _removeGrid: function (map) {
        if (map && this._onView) { try { map.off('moveend zoomend', this._onView); } catch (_) {} }
        this._onView = null;
        this._cancelWindowTimer();
        if (this._hashTimer != null) { try { clearTimeout(this._hashTimer); } catch (_) {} this._hashTimer = null; }
        if (this._winAbort) { try { this._winAbort.abort(); } catch (_) {} this._winAbort = null; }
        if (this._linksAbort) { try { this._linksAbort.abort(); } catch (_) {} this._linksAbort = null; }
        this._winFetchKey = this._wantKey = '';
        if (map && this._linkPopup) { try { map.closePopup(this._linkPopup); } catch (_) {} }
        if (map && this._linkGroup) { try { map.removeLayer(this._linkGroup); } catch (_) {} }
        this._linkGroup = this._linkPopup = this._linkPopupId = null;
        this._shownLinks = [];
        this._linksKey = this._winKey = '';
        // Unticking the overlay takes it out of the link too (a no-op off
        // the map page: hashWith answers null there).
        this._replaceHash({ duct: null, ductT: null });
      },

      // A JSON GET through CSApi; ctrl = an AbortController or null.
      _getJson: function (url, ctrl) {
        var opts = { headers: { Accept: 'application/json' } };
        if (ctrl) opts.signal = ctrl.signal;
        try {
          return CSApi.fetch(url, opts).then(function (r) {
            if (!r || r.ok === false) throw new Error('http');
            return r.json();
          });
        } catch (_) { return Promise.reject(new Error('fetch')); }
      },

      _loadMeta: function (gen) {
        var self = this, ctrl = null;
        if (typeof AbortController === 'function') {
          if (this._abort) { try { this._abort.abort(); } catch (_) {} }
          ctrl = this._abort = new AbortController();
        }
        this._getJson(META_URL, ctrl).then(function (m) {
          if (gen !== self._gen) return;
          self._applyMeta(m);
        }, function () {
          if (gen !== self._gen || (ctrl && ctrl.signal.aborted)) return;
          self._failMeta('error');
        });
      },

      // Same rules as the Open-Meteo _fail: with times held they stay
      // usable; an error or failed request marks them out of date, pending
      // does not. Either way ask again every minute.
      _failMeta: function (state) {
        this._schedule(RETRY_MS);
        if (this._meta) { this._setStale(state === 'error' || this._stale); return; }
        this._state = state;
        this._renderLegend();
      },

      _applyMeta: function (m) {
        if (!m || typeof m !== 'object') return this._failMeta('error');
        if (m.status === 'pending') return this._failMeta('pending');
        var times = [];
        if (Array.isArray(m.times)) {
          for (var i = 0; i < m.times.length; i++) {
            var t = Number(m.times[i]);
            if (isFinite(t) && (!times.length || t > times[times.length - 1])) times.push(t);
          }
        }
        if (m.status !== 'ok' || !times.length) return this._failMeta('error');
        this._state = 'ok';
        this._schedule(GRID_REFRESH_MS);
        var held = this._meta, prevT = (held && this._sel >= 0) ? held.times[this._sel] : null;
        var run = String(m.generatedAt) + '|' + String(m.runTime);
        if (held && held._run !== run) {
          // A new model run replaces forecast steps: cached windows and the
          // drawn one may be old. The drawn image stays until the refetch.
          this._lru.clear();
          this._winKey = '';
          this._dataKey = '';
        }
        this._meta = { times: times, attribution: m.attribution, modelLabel: m.modelLabel, model: m.model,
          generatedAt: m.generatedAt, runTime: m.runTime, nativeStepDeg: Number(m.nativeStepDeg) || 0.5,
          historyDays: m.historyDays, _run: run };
        this._stale = false;
        this._stale = this._expired();
        this._showMeta(prevT);
      },

      // Slider range, legend and the shown time: the deep-linked one on the
      // first answer that has it, else a user-picked one still stored, else
      // the latest <= now.
      _showMeta: function (prevT) {
        var times = this._meta.times, idx = ductTimeIndex(times, Date.now() / 1000);
        if (prevT == null && this._sel >= 0 && this._shownT != null) prevT = this._shownT;
        if (this._wantT != null) {
          var w = times.indexOf(this._wantT);
          this._wantT = null;
          if (w >= 0) { idx = w; this._userPicked = true; }
        } else if (this._userPicked) {
          var k = prevT != null ? times.indexOf(prevT) : -1;
          if (k >= 0) idx = k; else this._userPicked = false;
        }
        if (this._slider) {
          this._slider.max = String(times.length - 1);
          this._slider.disabled = times.length < 2;
        }
        this._renderLegend();
        this._setTime(idx);
      },

      // The user moved the slider: back on the default time it follows "now"
      // again and leaves the link; any other time is kept and linked.
      _pickTime: function (i) {
        var m = this._meta;
        if (!m || i < 0 || i >= m.times.length) return;
        this._userPicked = i !== ductTimeIndex(m.times, Date.now() / 1000);
        this._setTime(i);
        var self = this;
        if (this._hashTimer != null) { try { clearTimeout(this._hashTimer); } catch (_) {} }
        this._hashTimer = later(function () {
          self._hashTimer = null;
          if (!self._map || !self._meta) return;
          var t = self._meta.times[self._sel];
          self._replaceHash(self._userPicked && t != null ? { duct: '1', ductT: String(t) } : { ductT: null });
        }, HASH_DEBOUNCE_MS);
      },

      _replaceHash: function (updates) {
        try {
          var h = hashWith(location.hash, updates);
          if (h != null && h !== location.hash) history.replaceState(history.state, '', h);
        } catch (_) { /* no history: nothing to link */ }
      },

      _setTime: function (i) {
        var m = this._meta;
        if (!m || i < 0 || i >= m.times.length) return;
        this._sel = i;
        this._shownT = m.times[i];
        var label = timeLabel(m.times[i], Date.now() / 1000);
        if (this._slider) {
          this._slider.value = String(i);
          this._slider.setAttribute('aria-valuetext', label);
        }
        if (this._timeLabel) this._timeLabel.textContent = 'Time: ' + label;
        this._queueWindow();
        this._drawLinks();
      },

      // What the view needs: { t, box, key } (key = time + box, the LRU key),
      // or null. A drawn window at the same time... or any time: the box of
      // the drawn window is reused while it covers the view at the step the
      // view would get, so panning inside it and scrubbing the slider ask
      // for the same boxes (LRU and HTTP cache hits).
      _wantWindow: function () {
        var map = this._map, m = this._meta;
        if (!map || !m || this._sel < 0) return null;
        var b;
        try { b = map.getBounds(); } catch (_) { return null; }
        if (!b) return null;
        var south = b.getSouth(), west = b.getWest(), north = b.getNorth(), east = b.getEast();
        var box = windowBox(south, west, north, east, WINDOW_PAD);
        if (!box) return null;
        var cur = this._dataBox;
        if (cur && cur.shift === box.shift) {
          var view = { s: clamp(south, -90, 90), n: clamp(north, -90, 90),
            w: clamp(west - box.shift, -180, 180), e: clamp(east - box.shift, -180, 180) };
          if (boxCovers(cur, view) && windowStep(cur, m.nativeStepDeg) === windowStep(box, m.nativeStepDeg)) box = cur;
        }
        var t = m.times[this._sel];
        return { t: t, box: box, key: t + '|' + box.s + ',' + box.w + ',' + box.n + ',' + box.e };
      },

      _cancelWindowTimer: function () {
        if (this._winTimer != null) { try { clearTimeout(this._winTimer); } catch (_) {} }
        this._winTimer = null;
      },

      // Shows the window the view and time need: nothing when it is drawn,
      // at once from the LRU, else a fetch after WINDOW_DEBOUNCE_MS of quiet
      // (one already in flight for it is awaited, not repeated).
      _queueWindow: function () {
        var want = this._wantWindow();
        if (!want) return;
        this._wantKey = want.key;
        if (want.key === this._winKey && want.box.shift === this._winShift) { this._cancelWindowTimer(); return; }
        var hit = this._lru.get(want.key);
        if (hit) {
          this._cancelWindowTimer();
          this._applyWindow(hit, want.key, want.box);
          return;
        }
        if (this._winFetchKey === want.key) { this._cancelWindowTimer(); return; }
        this._cancelWindowTimer();
        var self = this, gen = this._gen;
        this._winTimer = later(function () {
          self._winTimer = null;
          if (gen === self._gen && self._wantKey === want.key) self._fetchWindow(gen, want);
        }, WINDOW_DEBOUNCE_MS);
      },

      _fetchWindow: function (gen, want) {
        var self = this, ctrl = null, b = want.box;
        if (this._winAbort) { try { this._winAbort.abort(); } catch (_) {} }
        if (typeof AbortController === 'function') ctrl = new AbortController();
        this._winAbort = ctrl;
        this._winFetchKey = want.key;
        var url = GRID_URL + '?t=' + encodeURIComponent(String(want.t)) + '&bbox=' + [b.s, b.w, b.n, b.e].join(',');
        var done = function () {
          if (self._winFetchKey === want.key) { self._winFetchKey = ''; self._winAbort = null; }
        };
        this._getJson(url, ctrl).then(function (d) {
          if (gen !== self._gen) return;
          done();
          if (!d || d.status !== 'ok' || !Array.isArray(d.cells) || !Array.isArray(d.times)) throw new Error('bad window');
          self._lru.set(want.key, d);
          self._setWinErr(false);
          if (self._wantKey === want.key) self._applyWindow(d, want.key, want.box);
        }).catch(function () {
          if (gen !== self._gen || (ctrl && ctrl.signal.aborted)) return;
          done();
          if (self._wantKey === want.key) self._setWinErr(true);
        });
      },

      _setWinErr: function (on) {
        on = !!on;
        if (on === !!this._winErr) return;
        this._winErr = on;
        this._renderLegend();
      },

      // Draws a window answer (one time: index 0) for box. The field canvas
      // and image layer are reused, so the old image shows until this one
      // is drawn.
      _applyWindow: function (d, key, box) {
        this._data = d;
        this._dataKey = key;
        this._dataBox = box;
        this._winKey = key;
        this._winShift = box.shift || 0;
        this._idx = 0;
        this._build();
        this._restyle();
        this._syncPopup();
      },

      _linkDays: function () {
        var c = (typeof window !== 'undefined' && window.MC_DUCTING_CFG) || {};
        var n = Math.round(Number(c.historyDays || (this._meta && this._meta.historyDays)));
        return (n >= 1 && n <= 366) ? n : 0;
      },

      _loadLinks: function (gen) {
        var self = this, ctrl = null, days = this._linkDays();
        if (typeof AbortController === 'function') {
          if (this._linksAbort) { try { this._linksAbort.abort(); } catch (_) {} }
          ctrl = this._linksAbort = new AbortController();
        }
        this._getJson(LINKS_URL + (days ? '?days=' + days : ''), ctrl).then(function (d) {
          if (gen !== self._gen) return;
          if (d && d.status === 'ok' && Array.isArray(d.links)) {
            self._links = d;
            self._linksState = 'ok';
          } else if (!self._links) self._linksState = (d && d.status === 'pending') ? 'pending' : 'error';
          self._linksKey = '';
          self._drawLinks();
          self._renderLegend();
        }, function () {
          if (gen !== self._gen || (ctrl && ctrl.signal.aborted)) return;
          if (!self._links) { self._linksState = 'error'; self._renderLegend(); }
        });
      },

      // The links heard within LINK_WINDOW_S of the shown time, as up to
      // four non-interactive multi-part polylines on the line canvas: no
      // model data (dashed), a casing under the solid ones, the rest, the
      // ducted ones on top. Rebuilt only when the time, the links, the
      // colours or the world copy change.
      _drawLinks: function () {
        var grp = this._linkGroup;
        if (!grp || !L.polyline) return;
        var m = this._meta, ld = this._links, t = (m && this._sel >= 0) ? m.times[this._sel] : NaN;
        var c1 = cssVar(LINK_VAR), c2 = cssVar(LINK_DUCT_VAR), c3 = cssVar(LINK_CASING_VAR), shift = 0;
        try { var b = this._map.getBounds(); shift = shiftFor(b.getWest(), b.getEast()); } catch (_) { shift = 0; }
        var key = [t, ld ? String(ld.generatedAt) + ':' + ld.links.length : '-', c1, c2, c3, shift].join('|');
        if (key === this._linksKey) return;
        this._linksKey = key;
        try { grp.clearLayers(); } catch (_) {}
        var items = (ld && t === t) ? linksAt(ld.links, t, LINK_WINDOW_S) : [];
        var parts = { nodata: [], casing: [], plain: [], duct: [] }, shown = [];
        for (var i = 0; i < items.length; i++) {
          var color = items[i].kind === 'duct' ? c2 : c1;
          if (!color) continue;                // no colour variable: not drawn, never invented
          items[i].latlngs = linkLatLngs(items[i].link, shift);
          parts[items[i].kind].push(items[i].latlngs);
          if (items[i].kind !== 'nodata' && c3) parts.casing.push(items[i].latlngs);
          shown.push(items[i]);
        }
        var order = ['nodata', 'casing', 'plain', 'duct'];
        for (var k = 0; k < order.length; k++) {
          var kind = order[k];
          if (!parts[kind].length) continue;
          var opts = L.extend({ renderer: this._renderer, color: kind === 'duct' ? c2 : kind === 'casing' ? c3 : c1 }, LINK_STYLE);
          if (kind === 'nodata') opts.dashArray = '6 5';
          if (kind === 'duct') opts.weight = 3.5;
          if (kind === 'casing') opts.weight = 6.5;
          try {
            var line = L.polyline(parts[kind], opts);
            line._mcLinks = kind;
            grp.addLayer(line);
          } catch (_) {}
        }
        this._shownLinks = shown;
        this._updateLinkCount();
        this._syncLinkPopup();
      },

      // Opens the popup of the drawn link nearest the click within
      // LINK_HIT_PX; false when there is none.
      openLinkPopup: function (latlng) {
        var map = this._map, items = this._shownLinks;
        if (!map || !latlng || !items || !items.length || !L.popup) return false;
        var best = -1, bd = LINK_HIT_PX;
        try {
          var p = map.latLngToContainerPoint(latlng);
          for (var k = 0; k < items.length; k++) {
            var ll = items[k].latlngs;
            var dist = segDist(p, map.latLngToContainerPoint(ll[0]), map.latLngToContainerPoint(ll[1]));
            if (dist <= bd) { bd = dist; best = k; }
          }
        } catch (_) { return false; }
        if (best < 0) return false;
        try {
          if (!this._linkPopup) this._linkPopup = L.popup({ maxWidth: 300 });
          this._linkPopupId = linkId(items[best].link);
          this._linkPopup.setLatLng(latlng).setContent(linkPopupContent(items[best], this._links && this._links.freqMHz, linksMethod(this._links))).openOn(map);
          return true;
        } catch (_) { return false; }
      },

      // An open link popup follows the time: new receptions while the link
      // is still drawn, closed when it is not.
      _syncLinkPopup: function () {
        var p = this._linkPopup, open = false;
        try { open = !!(p && p.isOpen()); } catch (_) { open = false; }
        if (!open) return;
        var items = this._shownLinks || [];
        for (var k = 0; k < items.length; k++) {
          if (linkId(items[k].link) === this._linkPopupId) {
            p.setContent(linkPopupContent(items[k], this._links && this._links.freqMHz, linksMethod(this._links)));
            return;
          }
        }
        try { this._map.closePopup(p); } catch (_) {}
      },

      _setIndex: function (i) {
        var d = this._data;
        if (!d || i < 0 || i >= d.times.length) return;
        this._idx = i;
        this._shown = d.times[i];
        if (this._slider) this._slider.value = String(i);
        if (this._timeLabel) this._timeLabel.textContent = 'Forecast time: ' + fmtTime(d.times[i]);
        this._restyle();
        this._syncPopup();
      },

      // Redraws the field image, the edge colour and the LoRa outlines for
      // the current hour and theme.
      _restyle: function () {
        this._drawField();
        var edgeColor = cssVar(EDGE_VAR);
        if (this._edge && edgeColor !== this._edgeColor) {   // theme change
          this._edgeColor = edgeColor;
          this._edge.setStyle(edgeColor ? { stroke: true, color: edgeColor } : { stroke: false });
        }
        this._drawLora();
      },

      // The dashed outlines of the cells that may carry LoRa at the shown
      // hour: one non-interactive multi-polygon (a ring per cell) on the
      // line canvas, replaced only when the set of cells or the colour
      // changes. O(cells). No --mc-duct-lora: no outline.
      _drawLora: function () {
        var group = this._group;
        if (!group || !L.polygon) return;
        var i = this._idx, color = cssVar(LORA_VAR), h = (Number(this._data && this._data.stepDeg) || 0.5) / 2;
        var sh = this._gridMode ? (this._winShift || 0) : 0;
        var cells = [], rings = [], ids = [];
        if (color) {
          for (var k = 0; k < this._cells.length; k++) {
            var c = this._cells[k];
            if (!isLora(c, i)) continue;
            cells.push(c);
            ids.push(k);
            var lo = c.lon + sh;
            rings.push([[[c.lat - h, lo - h], [c.lat - h, lo + h], [c.lat + h, lo + h], [c.lat + h, lo - h]]]);
          }
        }
        var key = color + '|' + ids.join(',');
        if (key === this._loraKey) return;
        this._loraKey = key;
        if (this._lora) { try { group.removeLayer(this._lora); } catch (_) {} this._lora = null; }
        if (!rings.length) return;
        try {
          var poly = L.polygon(rings, L.extend({ renderer: this._renderer, color: color }, LORA_STYLE));
          poly._mcLora = true;
          poly._mcCells = cells;
          group.addLayer(poly);
          this._lora = poly;
        } catch (_) { this._lora = null; }
      },

      _drawField: function () {
        var g = this._grid, map = this._map, d = this._data;
        if (!g || !map || !d) { this._dropField(); return; }
        var t0 = Date.now();
        var canvas = this._canvas;
        try {
          if (!canvas) canvas = this._canvas = document.createElement('canvas');
          if (canvas.width !== g.w) canvas.width = g.w;
          if (canvas.height !== g.h) canvas.height = g.h;
          var ctx = canvas.getContext('2d');
          // One pixel buffer per image size, cleared for each redraw.
          var img = this._img;
          if (!img || img.width !== g.w || img.height !== g.h) img = this._img = ctx.createImageData(g.w, g.h);
          else img.data.fill(0);
          renderField(g, ductGridValues(g, d.cells, this._idx), fieldLut(fieldStops(), fieldAlpha()), img.data);
          ctx.putImageData(img, 0, 0);
        } catch (_) { this._dropField(); return; }
        var bounds = gridBounds(g);
        if (!this._field) {
          this._field = newField(canvas, bounds, this._pane);
          if (this._field) { try { map.addLayer(this._field); } catch (_) { this._field = null; } }
        } else if (this._fieldKey !== String(bounds)) {
          try { this._field.setBounds(L.latLngBounds(bounds)); } catch (_) {}
        }
        this._fieldKey = String(bounds);
        this._fieldMs = Date.now() - t0;
      },

      _dropField: function () {
        if (this._field && this._map) { try { this._map.removeLayer(this._field); } catch (_) {} }
        this._field = null;
        this._fieldKey = '';
      }
    });
  }

  var LayerClass = null;
  window.MC_createDuctingLayer = function (opts) {
    if (typeof L === 'undefined' || !L.Layer) return null;
    if (!LayerClass) LayerClass = makeLayerClass();
    return new LayerClass(opts);
  };
  window.MC_ductTimeIndex = ductTimeIndex;
  window.MC_ductPopupContent = popupContent;
  window.MC_ductFieldColor = fieldColor;
  window.MC_ductFieldStops = fieldStops;
  window.MC_ductGrid = ductGrid;
  window.MC_ductEdges = ductEdges;
  window.MC_ductCellAt = ductCellAt;
  window.MC_ductGridValues = ductGridValues;
  window.MC_ductSample = sample;
  window.MC_ductFieldValues = fieldValues;
  window.MC_ductRowLat = rowLat;
  window.MC_ductWindowBox = windowBox;
  window.MC_ductWindowStep = windowStep;
  window.MC_ductBoxCovers = boxCovers;
  window.MC_ductLru = function (max) { return new Lru(max); };
  window.MC_ductTimeLabel = timeLabel;
  window.MC_ductHashTime = hashTime;
  window.MC_ductHashWith = hashWith;
  window.MC_ductLinksAt = linksAt;
  window.MC_ductLinkKind = linkKind;
  window.MC_ductSegDist = segDist;
  window.MC_ductLinksMethod = linksMethod;
  window.MC_ductLinksSummary = linksSummary;
  window.MC_ductLinkPopup = linkPopupContent;
})();
