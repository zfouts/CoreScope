/* === CoreScope — url-state.js ===
 *
 * Shared helpers for encoding/decoding view & filter state in the URL hash.
 * Pages use these so deep links restore the exact view (issue #749).
 *
 * Hash format: "#/<route>?key1=val1&key2=val2"
 *
 * Existing deep links remain intact:
 *   #/nodes/<pubkey>            (path segment after route)
 *   #/packets/<hash>            (path segment after route)
 *   #/packets?filter=...        (query after route)
 *
 * This module ONLY parses/serializes — it never mutates location.
 *
 * Ducting overlay link (#2459), on #/map, #/live and #/tools/planner:
 *   parseDuctLink(hash) -> { on, t }      duct=1 / duct_t=<hour, unix s>
 *   ductLinkHash(hash, on, t) -> hash     the same two set or removed
 */
'use strict';

(function (root) {
  /**
   * Parse a sort token "column[:direction]" into { column, direction }.
   * Direction defaults to 'desc'. Anything other than 'asc'/'desc' falls back to 'desc'.
   * Empty/null input returns null.
   */
  function parseSort(s) {
    if (s == null || s === '') return null;
    var str = String(s);
    var idx = str.indexOf(':');
    var column = idx >= 0 ? str.slice(0, idx) : str;
    var dir = idx >= 0 ? str.slice(idx + 1) : 'desc';
    if (dir !== 'asc' && dir !== 'desc') dir = 'desc';
    return { column: column, direction: dir };
  }

  /**
   * Serialize a sort state to a token. 'desc' is the default and omitted.
   * Empty/null column returns ''.
   */
  function serializeSort(column, direction) {
    if (!column) return '';
    if (direction === 'asc') return column + ':asc';
    return String(column);
  }

  /**
   * Parse a location.hash string into { route, params }.
   * - Strips leading '#' and '/'.
   * - Splits on first '?'; left = route (may include subpath like 'nodes/abc'),
   *   right = querystring parsed via URLSearchParams.
   */
  function parseHash(hash) {
    var h = String(hash || '');
    if (h.charAt(0) === '#') h = h.slice(1);
    if (h.charAt(0) === '/') h = h.slice(1);
    if (h === '') return { route: '', params: {} };
    var qi = h.indexOf('?');
    var route = qi >= 0 ? h.slice(0, qi) : h;
    var qs = qi >= 0 ? h.slice(qi + 1) : '';
    var params = {};
    if (qs) {
      var sp = new URLSearchParams(qs);
      sp.forEach(function (v, k) { params[k] = v; });
    }
    return { route: route, params: params };
  }

  /**
   * Build a hash string '#/<route>?k=v&...'. Skips keys with null/undefined/'' values.
   * 'route' may be passed as '#/foo', '/foo' or 'foo'.
   */
  function buildHash(route, params) {
    var r = String(route || '');
    if (r.charAt(0) === '#') r = r.slice(1);
    if (r.charAt(0) === '/') r = r.slice(1);
    var sp = new URLSearchParams();
    if (params && typeof params === 'object') {
      for (var k in params) {
        if (!Object.prototype.hasOwnProperty.call(params, k)) continue;
        var v = params[k];
        if (v == null || v === '') continue;
        sp.set(k, String(v));
      }
    }
    var qs = sp.toString();
    return '#/' + r + (qs ? '?' + qs : '');
  }

  /**
   * Apply a partial-update to the params of an existing hash, preserving the route
   * (including any subpath like 'nodes/<pubkey>'). Returns the new hash string —
   * caller decides whether to history.replaceState() it.
   *
   * Setting a key to '' / null / undefined removes it.
   */
  function updateHashParams(updates, currentHash) {
    var src = currentHash != null ? currentHash :
      (typeof location !== 'undefined' ? location.hash : '');
    var parsed = parseHash(src);
    var merged = {};
    var k;
    for (k in parsed.params) {
      if (Object.prototype.hasOwnProperty.call(parsed.params, k)) merged[k] = parsed.params[k];
    }
    if (updates && typeof updates === 'object') {
      for (k in updates) {
        if (!Object.prototype.hasOwnProperty.call(updates, k)) continue;
        var v = updates[k];
        if (v == null || v === '') delete merged[k];
        else merged[k] = v;
      }
    }
    return buildHash(parsed.route, merged);
  }

  // ── Tropospheric-ducting overlay link (#2459) ──
  // On the pages with a map layer picker:
  //   duct=1                  the ducting overlay is shown for this page view
  //   duct_t=<unix seconds>   the shown forecast hour (a whole hour); implies duct=1
  var DUCT_ROUTES = { 'map': true, 'live': true, 'tools/planner': true };
  var HOUR_S = 3600;

  // The route part of a hash ('#/map?x=1' -> 'map'), nothing parsed.
  function routeOf(hash) {
    var h = String(hash == null ? '' : hash);
    if (h.charAt(0) === '#') h = h.slice(1);
    if (h.charAt(0) === '/') h = h.slice(1);
    var qi = h.indexOf('?');
    return qi >= 0 ? h.slice(0, qi) : h;
  }

  /** True on the routes whose hash carries the ducting link parameters. */
  function isDuctRoute(hash) {
    return Object.prototype.hasOwnProperty.call(DUCT_ROUTES, routeOf(hash));
  }

  // A forecast hour as the link carries it: plain digits, a whole hour, > 0.
  // Anything else (a sign, a fraction, an exponent, spaces) is null.
  function ductHour(v) {
    var s = typeof v === 'number' ? String(v) : v;
    if (typeof s !== 'string' || !/^[0-9]{1,11}$/.test(s)) return null;
    var n = Number(s);
    return (n > 0 && n % HOUR_S === 0) ? n : null;
  }

  /**
   * Read the ducting link parameters of a hash: { on, t }.
   * on: the link asks for the overlay (duct=1, or a valid duct_t).
   * t:  the linked forecast hour (unix seconds) or null. A duct_t that is not
   *     a whole hour is ignored. Off the picker routes: { on: false, t: null }.
   */
  function parseDuctLink(hash) {
    if (!isDuctRoute(hash)) return { on: false, t: null };
    var p = parseHash(hash).params;
    var t = ductHour(p.duct_t);
    return { on: p.duct === '1' || t != null, t: t };
  }

  function queryKey(pair) {
    var eq = pair.indexOf('=');
    var k = eq >= 0 ? pair.slice(0, eq) : pair;
    try { return decodeURIComponent(k.replace(/\+/g, ' ')); } catch (_) { return k; }
  }

  /**
   * The hash with the ducting link parameters set (on: duct=1, plus duct_t
   * when t is a whole hour) or removed (off: both). Every other parameter
   * stays exactly as written, in place; nothing is re-encoded. Returns null
   * off the picker routes (nothing to write there). The caller decides
   * whether to history.replaceState() it.
   */
  function ductLinkHash(hash, on, t) {
    if (!isDuctRoute(hash)) return null;
    var h = String(hash);
    var qi = h.indexOf('?');
    var head = qi >= 0 ? h.slice(0, qi) : h;
    var keep = [];
    if (qi >= 0) {
      var parts = h.slice(qi + 1).split('&');
      for (var i = 0; i < parts.length; i++) {
        if (parts[i] === '') continue;
        var k = queryKey(parts[i]);
        if (k !== 'duct' && k !== 'duct_t') keep.push(parts[i]);
      }
    }
    if (on) {
      keep.push('duct=1');
      var hour = ductHour(t);
      if (hour != null) keep.push('duct_t=' + hour);
    }
    return head + (keep.length ? '?' + keep.join('&') : '');
  }

  var api = {
    isDuctRoute: isDuctRoute,
    parseDuctLink: parseDuctLink,
    ductLinkHash: ductLinkHash,
    parseSort: parseSort,
    serializeSort: serializeSort,
    parseHash: parseHash,
    buildHash: buildHash,
    updateHashParams: updateHashParams,
  };

  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  root.URLState = api;
})(typeof window !== 'undefined' ? window : globalThis);
