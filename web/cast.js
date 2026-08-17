/**
 * origin-caster - Universal Cast Trigger & Player Extractor
 *
 * Runs inside the streaming site's page (or the player iframe) and opens
 * `/api/cast?url=...&cookies=...&referer=...` in a hidden popup.
 *
 * Pipeline:
 *   1. Snapshot the page once: every <video> sorted by activity, the
 *      performance resource list, and engine instances attached to elements.
 *   2. Run layered detectors - player brands, then engines (hls.js / dash.js /
 *      shaka / flv.js), then generic HTML5, then the network scan.
 *   3. Collect every candidate (no first-wins), score them, and merge the best.
 *   4. Open the cast popup with url / cookies / referer / currentTime.
 *
 * Rules for contributors:
 *   - Detectors MUST be read-only: never call player constructors or
 *     create/initialize a player (e.g. never `dashjs.MediaPlayer().create()`,
 *     never `bitmovin.player(el)` on a plain element). Instance access is only
 *     via element-attached refs or globals that already exist.
 *   - Never accept `blob:` URLs - if the <video> plays via MSE, the real
 *     manifest lives on the engine (`hls.url`, `getSource()`, ...).
 *   - Return a MediaCandidate `{ url, type?, time?, player?, layer? }` or null.
 *
 * Debugging: set `window.__OC_DEBUG__ = true` in the page console for
 * per-detector output; `window.__OC_WATCH__ = true` retries once with a 3 s
 * live network capture (XHR/fetch hooks) to catch lazy-loaded manifests.
 */
(function() {
  'use strict';

  const DEBUG = window.__OC_DEBUG__ === true;
  const log = DEBUG ? function() {
    const args = Array.prototype.slice.call(arguments);
    args.unshift('%c[OC]', 'color:#4fc3f7; font-weight:bold;');
    console.log.apply(console, args);
  } : function() {};

  // ── URL helpers ───────────────────────────────────────────────
  function goodUrl(u) {
    return !!u && typeof u === 'string' && u.indexOf('blob:') !== 0 && u.indexOf('data:') !== 0;
  }
  function resolveUrl(u) {
    try { return new URL(u, location.href).href; } catch (e) { return ''; }
  }
  function isDisguisedPng(u) {
    if (!u || typeof u !== 'string') return false;
    const base = u.split('#')[0].split('?')[0];
    const filename = base.substring(base.lastIndexOf('/') + 1);
    if (/x\d+\.png$/i.test(filename) || /icon|logo|favicon|splash|poster|banner|thumb/i.test(base)) return false;
    return /^(?:seg[-_]?|chunk[-_]?)?\d{3,}\.png$/i.test(filename);
  }
  function isManifest(u) {
    return /\.m3u8($|\?)/i.test(u) || /\.mpd($|\?)/i.test(u);
  }
  function isSegment(u) {
    return /\.ts($|\?)/i.test(u) || /\.m4s($|\?)/i.test(u) || /\.aac($|\?)/i.test(u)
        || /\.mp3($|\?)/i.test(u) || /init\.mp4/i.test(u) || isDisguisedPng(u) || /\d{3,}\.(mp4|ts)(\?|$)/i.test(u);
  }
  function typeFromUrl(u) {
    if (/\.m3u8/i.test(u)) return 'application/x-mpegurl';
    if (/\.mpd/i.test(u)) return 'application/dash+xml';
    if (/\.mp4/i.test(u)) return 'video/mp4';
    if (/\.ts/i.test(u)) return 'video/mp2t';
    return '';
  }

  // ── Candidate factory + shared constants ──────────────────────
  const FLV_UNSUPPORTED = 'FLV is not supported by the Chromecast default receiver';
  function candidate(url, type, time, player, layer) {
    return { url: url, type: type || '', time: time || 0, player: player, layer: layer };
  }
  // document.cookie throws SecurityError in cross-origin iframes; the snippet
  // is sometimes pasted into a player iframe where reads are blocked.
  function safeCookies() {
    try { return document.cookie; } catch (e) { return ''; }
  }

  // ── Snapshot ──────────────────────────────────────────────────
  // Activity heuristic: playing > has a src > visible & large > long duration.
  // Ad players are usually muted, small, offscreen, or named ad/teaser.
  function videoScore(v) {
    let s = 0;
    try {
      if (!v.paused) s += 100;
      if (v.currentSrc && v.currentSrc.indexOf('blob:') !== 0) s += 40;
      if (v.src && v.src.indexOf('blob:') !== 0) s += 20;
      if (v.readyState > 2) s += 10;
      if (v.duration && isFinite(v.duration)) s += Math.min(v.duration / 600, 10);
      const r = v.getBoundingClientRect();
      if (r && r.width >= 100 && r.height >= 60) s += 30;
      if (v.muted) s -= 20;
      const cls = String(v.className || '') + ' ' + String(v.id || '');
      if (/ad|ads|teaser|trailer|banner/i.test(cls)) s -= 50;
    } catch (e) {}
    return s;
  }

  function snapshot(extraResources) {
    let videos = [];
    try {
      videos = Array.prototype.slice.call(document.querySelectorAll('video'));
      videos.sort(function(a, b) { return videoScore(b) - videoScore(a); });
    } catch (e) {}

    const resources = (extraResources && extraResources.slice()) || [];
    try {
      const seen = {};
      for (let i = 0; i < resources.length; i++) seen[resources[i]] = true;
      const mine = performance.getEntriesByType('resource')
        .map(function(r) { return r.name; })
        .filter(function(u) {
          if (isDisguisedPng(u)) return true;                           // disguised TS chunk
          if (/(\.css|\.js|\.json|\.woff2?|\.svg|\.jpe?g|\.webp|\.gif|\.png)(\?|$)/i.test(u)) return false;
          if (!/(\.m3u8|\.mpd|\.mp4|\.ts|\.flv|\.m4s)/i.test(u)) return false;
          return true;
        })
        .reverse();                                                      // newest first
      for (let i = 0; i < mine.length; i++) {
        if (!seen[mine[i]]) { seen[mine[i]] = true; resources.push(mine[i]); }
      }
    } catch (e) {}

    return {
      videos: videos,
      active: videos[0] || null,
      resources: resources,
      engines: []          // filled by the engine scan layer (provenance)
    };
  }

  // ── Detector recipes (find the player -> read its URL) ────────
  //   hls.js        window.Hls / refs on <video> (hlsPlayer, _hls, ...) -> hls.url
  //   dash.js       video._dashjs_player (attached ref only) -> getSource()
  //   shaka         video.shakaPlayer -> getAssetUri()
  //   Video.js      window.videojs.getPlayers() -> p.currentSource().src
  //   JW Player     window.jwplayer -> getPlaylistItem()?.file
  //   Flowplayer    window.flowplayer(el) on existing containers -> fp.video.src
  //   MediaElement  window.mejs.players -> hlsPlayer?.url / dashPlayer?.getSource() / originalNode.currentSrc
  //   Plyr / Clappr .plyr / .clappr container -> engine scan on its <video>
  //   Bitmovin      window.bitmovin.player(el) on .bmpui-* elements only -> getSource()
  //   THEOplayer    window.THEOplayer / THEOplayerChrome -> player.source.sources[0].src
  //   Kaltura       .kaltura-player container -> engine + network scan
  //   generic       any <video> -> video.currentSrc
  //   network       performance resource list -> filtered stream URLs
  //   flv.js        warn only - FLV cannot play on the Chromecast receiver

  // ── Engine detectors (operate on one <video>) ─────────────────
  // Element-attached refs only; window globals are handled once in engineScan
  // so they are never attributed to an arbitrary <video>.
  function detectHls(v) {
    const refs = [v.hlsPlayer, v._hlsPlayer, v._hls, v.hls];
    for (let i = 0; i < refs.length; i++) {
      const hls = refs[i];
      if (hls && goodUrl(hls.url)) {
        return candidate(hls.url, 'application/x-mpegurl', v.currentTime || 0, 'hls.js', 'engine');
      }
    }
    return null;
  }

  function detectDash(v) {
    const p = v._dashjs_player || v.dashPlayer || v.dashjsPlayer;
    if (!p || typeof p.getSource !== 'function') return null;
    let url = null;
    try { url = p.getSource(); } catch (e) {}
    if (!goodUrl(url)) return null;
    return candidate(url, 'application/dash+xml', v.currentTime || 0, 'dash.js', 'engine');
  }

  function detectShaka(v) {
    if (!v.shakaPlayer || typeof v.shakaPlayer.getAssetUri !== 'function') return null;
    let uri = null;
    try { uri = v.shakaPlayer.getAssetUri(); } catch (e) {}
    if (!goodUrl(uri)) return null;
    return candidate(uri, typeFromUrl(uri), v.currentTime || 0, 'shaka', 'engine');
  }

  function detectFlv(v) {
    // FLV cannot play on the Chromecast default receiver - detect to warn only.
    const refs = [v.flvPlayer, v._flv];
    for (let i = 0; i < refs.length; i++) {
      const flv = refs[i];
      if (!flv) continue;
      const url = flv.url || (flv.mediaInfo && flv.mediaInfo.url)
               || (flv.mediaDataSource && flv.mediaDataSource.url)
               || (flv._mediaInfo && flv._mediaInfo.url);
      if (!goodUrl(url)) continue;
      const c = candidate(url, 'video/x-flv', v.currentTime || 0, 'flv.js', 'engine');
      c.unsupported = FLV_UNSUPPORTED;
      return c;
    }
    return null;
  }

  function detectReactFiber(v) {
    if (!v) return null;
    let fiberKey = '';
    try {
      fiberKey = Object.keys(v).find(function(k) {
        return k.indexOf('__reactFiber') === 0 || k.indexOf('__reactInternalInstance') === 0;
      }) || '';
    } catch (e) {}
    if (!fiberKey) return null;

    let fiber = v[fiberKey];
    let depth = 0;
    while (fiber && depth < 40) {
      let state = fiber.memoizedState;
      while (state) {
        const s = state.memoizedState;
        if (s && typeof s === 'object') {
          if (goodUrl(s.url) && (s.type === 'hls' || s.type === 'mp4' || s.type === 'dash' || isManifest(s.url))) {
            const t = s.type === 'hls' ? 'application/x-mpegurl' : (s.type === 'dash' ? 'application/dash+xml' : typeFromUrl(s.url));
            return candidate(s.url, t, v.currentTime || 0, 'react-fiber', 'engine');
          }
          if (s.current && s.current.value && goodUrl(s.current.value.url)) {
            const val = s.current.value;
            const t = val.type === 'hls' ? 'application/x-mpegurl' : (val.type === 'dash' ? 'application/dash+xml' : typeFromUrl(val.url));
            return candidate(val.url, t, v.currentTime || 0, 'react-fiber', 'engine');
          }
          if (Array.isArray(s.sources) && s.sources[0] && goodUrl(s.sources[0].url)) {
            return candidate(s.sources[0].url, s.sources[0].type || typeFromUrl(s.sources[0].url), v.currentTime || 0, 'react-fiber', 'engine');
          }
        }
        state = state.next;
      }
      if (fiber.memoizedProps) {
        const p = fiber.memoizedProps;
        if (goodUrl(p.src)) {
          return candidate(p.src, p.type || typeFromUrl(p.src), v.currentTime || 0, 'react-fiber', 'engine');
        }
        if (p.stream && goodUrl(p.stream.url)) {
          return candidate(p.stream.url, p.stream.type || typeFromUrl(p.stream.url), v.currentTime || 0, 'react-fiber', 'engine');
        }
      }
      fiber = fiber.return;
      depth++;
    }
    return null;
  }

  function markActive(c, v, ctx) {
    if (c) c.fromActive = (v === ctx.active);
    return c;
  }

  // ── Engine reference scan (generic layer) ─────────────────────
  // Covers hls.js / dash.js / shaka / flv.js / react-fiber under any brand wrapper
  // (Video.js, MediaElement.js, Plyr, Clappr, Flowplayer, Kaltura, ...).
  function engineScan(ctx) {
    for (let i = 0; i < ctx.videos.length; i++) {
      const v = ctx.videos[i];
      const c = detectHls(v) || detectDash(v) || detectShaka(v) || detectFlv(v) || detectReactFiber(v);
      if (!c) continue;
      ctx.engines.push({ video: v, player: c.player, url: c.url });
      markActive(c, v, ctx);
      if (c.unsupported) { ctx.unsupported = ctx.unsupported || c; continue; }
      return c;
    }
    // Global references (some setups keep the instance off the element).
    // Attribute to the MSE video if any, so the merger still rewards the
    // element the instance belongs to.
    const g = window.hls || (window.Hls && window.Hls._instance);
    if (g && goodUrl(g.url)) {
      let v = null;
      for (let i = 0; i < ctx.videos.length; i++) {
        const src = ctx.videos[i].currentSrc || ctx.videos[i].src;
        if (src && src.indexOf('blob:') === 0) { v = ctx.videos[i]; break; }
      }
      const c = candidate(g.url, 'application/x-mpegurl', v ? v.currentTime || 0 : 0, 'hls.js', 'engine');
      return v ? markActive(c, v, ctx) : c;
    }
    return null;
  }

  // ── Generic layer ─────────────────────────────────────────────
  function html5Video(ctx) {
    for (let i = 0; i < ctx.videos.length; i++) {
      const v = ctx.videos[i];
      const c = nativeOn(v, 'html5');   // blob: (MSE) -> null; real URL lives on the engine
      if (!c) continue;
      return markActive(c, v, ctx);
    }
    return null;
  }

  // ── Network layer ─────────────────────────────────────────────
  function networkScan(ctx) {
    // Strip query/fragment once; the original URL (with tokens) is what we cast.
    const cleaned = [];
    for (let i = 0; i < ctx.resources.length; i++) {
      cleaned.push({ url: ctx.resources[i], base: ctx.resources[i].split('#')[0].split('?')[0] });
    }
    // Pass 1: manifests - these are the real streams (exact URL).
    for (let i = 0; i < cleaned.length; i++) {
      if (/\.m3u8/i.test(cleaned[i].base)) return candidate(cleaned[i].url, 'application/x-mpegurl', 0, 'network', 'network');
      if (/\.mpd/i.test(cleaned[i].base))  return candidate(cleaned[i].url, 'application/dash+xml', 0, 'network', 'network');
    }
    // Pass 2: direct media files / disguised chunks.
    for (let i = 0; i < cleaned.length; i++) {
      const u = cleaned[i].url, base = cleaned[i].base;
      if (/\.mp4/i.test(base) && !/init\.mp4/i.test(u) && !/\d{3,}\.mp4/i.test(u)) {
        return candidate(u, 'video/mp4', 0, 'network', 'network');
      }
      if (/\.ts/i.test(base)) return candidate(u, 'video/mp2t', 0, 'network', 'network');
      if (isDisguisedPng(u)) {
        // Disguised TS chunks (streamsvr-style) -> derive the playlist URL.
        return candidate(u.replace(/\/[^/]+$/, '/playlist.m3u8'), 'application/x-mpegurl', 0, 'network', 'network');
      }
      if (/\.flv/i.test(base)) {
        ctx.unsupported = ctx.unsupported || { url: u, type: 'video/x-flv', time: 0, player: 'network', layer: 'network',
          unsupported: FLV_UNSUPPORTED };
      }
    }
    return null;
  }

  // ── Player detectors (brands) ─────────────────────────────────
  // JW Player - legacy window.sources global.
  function jwSources(ctx) {
    const s = window.sources;
    if (!Array.isArray(s) || !s[0]) return null;
    const it = s[0];
    const url = it.file || it.src;
    if (!goodUrl(url)) return null;
    return candidate(url, it.type || '', 0, 'jw-sources', 'player');
  }

  // JW Player - API. jwplayer(id) only queries existing instances.
  function jwAPI(ctx) {
    if (typeof window.jwplayer !== 'function') return null;
    let els = [];
    try {
      els = Array.prototype.slice.call(document.querySelectorAll('.jwplayer, [id*="jwplayer"], [class*="jwplayer"]'));
    } catch (e) {}
    if (!els.length) {
      // No DOM anchors: query the default (first) instance - read-only.
      return jwCandidate(window.jwplayer());
    }
    for (let i = 0; i < els.length; i++) {
      if (!els[i].id) continue;
      let jw = null;
      try { jw = window.jwplayer(els[i].id); } catch (e) { continue; }
      const c = jwCandidate(jw);
      if (c) return c;
    }
    return null;
  }

  // Shared JW instance -> URL/type/time extraction.
  function jwCandidate(jw) {
    if (!jw) return null;
    const time = typeof jw.getPosition === 'function' ? jw.getPosition() : 0;
    let item = null;
    try { item = typeof jw.getPlaylistItem === 'function' ? jw.getPlaylistItem() : null; } catch (e) {}
    let url = '', type = '';
    if (item) {
      url = item.file || (item.sources && item.sources[0] && item.sources[0].file) || '';
      type = item.type || (item.sources && item.sources[0] && item.sources[0].type) || '';
    }
    if (!url && typeof jw.getConfig === 'function') {
      try {
        const cfg = jw.getConfig();
        const src = cfg && cfg.playlist && cfg.playlist[0] && cfg.playlist[0].sources && cfg.playlist[0].sources[0];
        if (src) { url = src.file || ''; type = src.type || ''; }
      } catch (e) {}
    }
    if (!goodUrl(url)) return null;
    return candidate(url, type, time, 'jw-api', 'player');
  }

  // Video.js - global registry of players.
  function videoJs(ctx) {
    if (typeof window.videojs !== 'function' || !window.videojs.getPlayers) return null;
    let players = {};
    try { players = window.videojs.getPlayers(); } catch (e) {}
    for (const id in players) {
      const p = players[id];
      if (!p || typeof p !== 'object') continue;
      let src = null;
      try { src = typeof p.currentSource === 'function' ? p.currentSource() : null; } catch (e) {}
      let url = src && src.src ? src.src : '';
      if (!url && typeof p.src === 'function') {
        try { url = p.src(); } catch (e) {}
      }
      if (!goodUrl(url)) continue;               // blob: -> engine scan will find the manifest
      let time = 0;
      try { time = typeof p.currentTime === 'function' ? p.currentTime() : 0; } catch (e) {}
      return candidate(url, (src && src.type) || '', time, 'video.js', 'player');
    }
    return null;
  }

  // MediaElement.js - mejs.players registry (mep_0 ...).
  function mediaElementJs(ctx) {
    const players = window.mejs && window.mejs.players;
    if (!players) return null;
    for (const id in players) {
      const p = players[id];
      if (!p || !p.media) continue;
      const media = p.media;
      let url = '', type = '';
      if (media.hlsPlayer && goodUrl(media.hlsPlayer.url)) {
        url = media.hlsPlayer.url; type = 'application/x-mpegurl';          // hls.js instance MEJS keeps on the media element
      } else if (media.dashPlayer && typeof media.dashPlayer.getSource === 'function') {
        try { url = media.dashPlayer.getSource(); } catch (e) {}
        if (goodUrl(url)) type = 'application/dash+xml';
      } else if (media.originalNode) {
        url = media.originalNode.currentSrc || media.originalNode.src || '';
      } else {
        url = media.currentSrc || media.src || '';
      }
      if (!goodUrl(url)) continue;
      let time = 0;
      try { time = media.currentTime || 0; } catch (e) {}
      return candidate(url, type, time, 'mediaelement', 'player');
    }
    return null;
  }

  // Plyr - no global registry; the engine lives on the .plyr <video>.
  function plyr(ctx) {
    return containerPlayer(ctx, '.plyr', 'plyr');
  }

  // Clappr - engine lives on the .clappr <video>.
  function clappr(ctx) {
    return containerPlayer(ctx, '.clappr', 'clappr', function() { return !!window.Clappr; });
  }

  // Flowplayer - only query existing containers (flowplayer() with no args can create).
  function flowPlayer(ctx) {
    if (typeof window.flowplayer !== 'function') return null;
    return forEachContainer('.flowplayer, [data-flowplayer], [class*="flowplayer"]', function(el) {
      let fp = null;
      try { fp = window.flowplayer(el); } catch (e) { return null; }
      if (!fp || !fp.video) return null;
      const url = fp.video.src || fp.video.url;
      if (!goodUrl(url)) return null;
      return candidate(url, fp.video.type || '', fp.video.time || 0, 'flowplayer', 'player');
    });
  }

  // Bitmovin - read-only rule: only touch elements already carrying the
  // Bitmovin UI marker; calling bitmovin.player(el) elsewhere creates a player.
  function bitmovin(ctx) {
    if (!window.bitmovin || typeof window.bitmovin.player !== 'function') return null;
    return forEachContainer('.bmpui-container, .bitmovinplayer-container, .bmpui-ui, [class*="bmpui"]', function(el) {
      let p = null;
      try { p = window.bitmovin.player(el); } catch (e) { return null; }
      if (!p) return null;
      let src = null, time = 0;
      try {
        if (typeof p.getSource === 'function') src = p.getSource();
        if (typeof p.getCurrentTime === 'function') time = p.getCurrentTime();
      } catch (e) {}
      const url = src && (src.hls || src.dash || src.progressive);
      if (!goodUrl(url)) return null;
      const type = src.hls ? 'application/x-mpegurl' : src.dash ? 'application/dash+xml' : typeFromUrl(url);
      return candidate(url, type, time || 0, 'bitmovin', 'player');
    });
  }

  // THEOplayer - v2-v6 register instances in THEOplayer.players / PlayerList.
  function theoPlayer(ctx) {
    const T = window.THEOplayer || window.THEOplayerChrome;
    if (!T) return null;
    let list = [];
    try {
      const players = T.players || (T.PlayerList && T.PlayerList.getAll && T.PlayerList.getAll()) || null;
      if (players) list = typeof players.length === 'number' ? players : [];
    } catch (e) {}
    for (let i = 0; i < list.length; i++) {
      let p = list[i];
      const uid = p && p.uid;
      if (uid && T.PlayerList && typeof T.PlayerList.getPlayerByUID === 'function') {
        try { p = T.PlayerList.getPlayerByUID(uid); } catch (e) {}
      }
      if (!p) continue;
      const src = p.source && p.source.sources && p.source.sources[0];
      const url = (src && src.src) || p.src;
      if (!goodUrl(url)) continue;
      const time = typeof p.currentTime === 'number' ? p.currentTime : 0;
      return candidate(url, typeFromUrl(url), time, 'theoplayer', 'player');
    }
    return null;
  }

  // Kaltura - enterprise wrapper; no reachable global instance, so the engine
  // scan + network scan do the real work. Detector improves provenance only.
  function kaltura(ctx) {
    return containerPlayer(ctx, '.kaltura-player', 'kaltura', function() {
      try { return !!window.kaltura || !!document.querySelector('.kaltura-player-container, .kaltura-player'); } catch (e) { return false; }
    });
  }

  // ── Small helpers used by player detectors ────────────────────
  function videoIn(ctx, sel) {
    for (let i = 0; i < ctx.videos.length; i++) {
      const v = ctx.videos[i];
      try { if (v.closest && v.closest(sel)) return v; } catch (e) {}
      if (v.parentClass && v.parentClass.indexOf(sel.slice(1)) !== -1) return v;  // fake-DOM fallback
    }
    return null;
  }

  function nativeOn(v, player) {
    const url = v.currentSrc || v.src;
    if (!goodUrl(url)) return null;
    return candidate(url, typeFromUrl(url), v.currentTime || 0, player, 'generic');
  }

  // Run fn over every matching container; return its first non-null result.
  function forEachContainer(sel, fn) {
    let els = [];
    try { els = Array.prototype.slice.call(document.querySelectorAll(sel)); } catch (e) {}
    for (let i = 0; i < els.length; i++) {
      const r = fn(els[i]);
      if (r) return r;
    }
    return null;
  }

  // Brand players whose engine (hls.js / dash.js / shaka) lives on a <video>
  // inside a known container, with the native <video> as the final fallback.
  function containerPlayer(ctx, sel, name, present) {
    if (present && !present()) return null;
    const v = videoIn(ctx, sel);
    if (!v) return null;
    return detectHls(v) || detectDash(v) || detectShaka(v) || markActive(nativeOn(v, name), v, ctx);
  }

  // ── Layer registry ────────────────────────────────────────────
  const layers = [
    { name: 'player',  detectors: [jwSources, jwAPI, videoJs, mediaElementJs, plyr, clappr, flowPlayer, bitmovin, theoPlayer, kaltura] },
    { name: 'engine',  detectors: [engineScan] },
    { name: 'generic', detectors: [html5Video] },
    { name: 'network', detectors: [networkScan] }
  ];

  // ── Candidate merger ──────────────────────────────────────────
  // Collect, don't first-win: every detector contributes; the best per-field
  // wins. Player/engine API URLs (exact manifests) beat network-scanned URLs;
  // segment URLs score lowest; blob: never reaches the merger.
  function merge(candidates) {
    if (!candidates || !candidates.length) return null;
    for (let i = 0; i < candidates.length; i++) {
      const c = candidates[i];
      let s = 50;
      if (c.layer === 'player' || c.layer === 'engine') s += 30;   // exact API/manifest URL
      if (isManifest(c.url)) s += 10;
      if (c.layer === 'network') s -= 10;                          // inferred from traffic
      if (isSegment(c.url)) s -= 30;
      if (c.time) s += 5;
      if (c.type) s += 5;
      // Strong activity preference: candidates tied to the active <video> win;
      // candidates from other (ad/teaser) elements are penalized.
      if (c.fromActive === true) s += 25;
      else if (c.fromActive === false) s -= 25;
      c.score = s < 0 ? 0 : (s > 100 ? 100 : s);
    }
    candidates.sort(function(a, b) { return b.score - a.score; });
    const best = candidates[0];
    const out = {
      url: best.url,
      type: best.type || typeFromUrl(best.url),
      time: best.time || 0,
      player: best.player,
      score: best.score
    };
    return out;
  }

  // ── Pipeline ──────────────────────────────────────────────────
  function run(extraResources) {
    const ctx = snapshot(extraResources || []);
    const candidates = [];

    for (let l = 0; l < layers.length; l++) {
      const layer = layers[l];
      for (let d = 0; d < layer.detectors.length; d++) {
        try {
          const c = layer.detectors[d](ctx);
          if (!c) continue;
          if (c.unsupported) { ctx.unsupported = ctx.unsupported || c; continue; }
          candidates.push(c);
          log('[detect]', layer.name, c.player, c.url, 'type=' + (c.type || '?'), 'time=' + (c.time || 0));
        } catch (e) {
          log('[detect:error]', layer.name, String(e));
        }
      }
    }

    const merged = merge(candidates);

    if (DEBUG) {
      log('[summary] candidates:', candidates.map(function(c) { return c.player + ' (' + c.score + ') ' + c.url; }).join(' | ') || '(none)');
      log('[summary] merged:', merged);
    }

    if (!merged) {
      if (ctx.unsupported) {
        return console.error('%c[OC] ⚠ Detected ' + ctx.unsupported.player + ' - ' + ctx.unsupported.unsupported, 'color:#ffb74d; font-weight:bold;');
      }
      if (window.__OC_WATCH__ === true && !run._watched) {
        run._watched = true;
        watchAndRetry();
        return;
      }
      return console.error('%c[OC] ❌ No active video stream found. Start video playback first.', 'color:#ff5252; font-weight:bold;');
    }

    // ── Cast to TV ──────────────────────────────────────────────
    const streamUrl = resolveUrl(merged.url);
    if (!streamUrl) {
      return console.error('%c[OC] ❌ Could not resolve stream URL.', 'color:#ff5252; font-weight:bold;');
    }

    const params = new URLSearchParams({
      url: streamUrl,
      title: document.title || 'Video Stream',
      referer: location.href,
      origin: location.origin,
      cookies: safeCookies(),
      contentType: merged.type || (streamUrl.indexOf('.m3u8') !== -1 || streamUrl.indexOf('streamsvr') !== -1 ? 'application/x-mpegurl' : ''),
      currentTime: Math.floor(merged.time || 0)
    });

    const w = window.open('__BASE__/api/cast?' + params.toString(), 'origincaster_cast', 'width=1,height=1,left=99999,top=99999');
    if (w) { setTimeout(function() { try { w.close(); } catch (e) {} }, 2000); }

    console.log('%c[OC] ⚡ Casting initiated to TV!', 'color:#00e676; font-weight:bold; font-size:13px;');
    console.log('Stream:', streamUrl);
    console.log('Detected by:', merged.player, '(score ' + merged.score + ')');
    if (ctx.active && !ctx.active.paused) { try { ctx.active.pause(); } catch (e) {} }
  }

  // ── Watch mode (opt-in) ───────────────────────────────────────
  // Some players lazy-load the manifest after user interaction. When enabled,
  // hook XHR/fetch for ~3 s and retry once with the captured URLs.
  function watchAndRetry() {
    console.log('%c[OC] 👀 Nothing found yet - watching network for ~3 s (lazy-loaded manifest)...', 'color:#4fc3f7; font-weight:bold;');
    const captured = [];
    const seen = {};
    const capture = function(u) {
      try {
        if (!u || typeof u !== 'string' || !/(\.m3u8|\.mpd|\.mp4|\.ts|\.flv)/i.test(u)) return;
        if (seen[u]) return;
        seen[u] = true;
        captured.push(u);
      } catch (e) {}
    };

    let origFetch = null;
    try {
      origFetch = window.fetch;
      if (typeof origFetch === 'function') {
        window.fetch = function(input) {
          try { capture(typeof input === 'string' ? input : (input && input.url)); } catch (e) {}
          return origFetch.apply(this, arguments);
        };
      }
    } catch (e) {}

    let origOpen = null;
    try {
      origOpen = XMLHttpRequest.prototype.open;
      XMLHttpRequest.prototype.open = function(method, url) {
        try { capture(String(url)); } catch (e) {}
        return origOpen.apply(this, arguments);
      };
    } catch (e) {}

    setTimeout(function() {
      try { if (origFetch) window.fetch = origFetch; } catch (e) {}
      try { if (origOpen) XMLHttpRequest.prototype.open = origOpen; } catch (e) {}
      run(captured);
    }, 3000);
  }

  // ── Test hooks (never present on a real page) ─────────────────
  if (window.__OC_EXPOSE__) {
    window.__OC__ = {
      snapshot: snapshot,
      run: run,
      merge: merge,
      layers: layers,
      utils: { goodUrl: goodUrl, resolveUrl: resolveUrl, isManifest: isManifest, isSegment: isSegment, isDisguisedPng: isDisguisedPng, typeFromUrl: typeFromUrl },
      detectors: { jwSources: jwSources, jwAPI: jwAPI, videoJs: videoJs, mediaElementJs: mediaElementJs, plyr: plyr, clappr: clappr, flowPlayer: flowPlayer, bitmovin: bitmovin, theoPlayer: theoPlayer, kaltura: kaltura, engineScan: engineScan, html5Video: html5Video, networkScan: networkScan, detectHls: detectHls, detectDash: detectDash, detectShaka: detectShaka, detectFlv: detectFlv, detectReactFiber: detectReactFiber }
    };
  }

  // ── One-shot run ──────────────────────────────────────────────
  if (window.__OC_AUTORUN__ !== false) {
    run();
  }
})();
