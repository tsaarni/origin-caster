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
(() => {
  'use strict';

  const DEBUG = window.__OC_DEBUG__ === true;
  const log = (...a) => DEBUG && console.log('[OC]', ...a);

  // ── MIME & URL helpers ────────────────────────────────────────
  const HLS_MIME = 'application/x-mpegurl', DASH_MIME = 'application/dash+xml';
  const MIME_MAP = { m3u8: HLS_MIME, mpd: DASH_MIME, mp4: 'video/mp4', ts: 'video/mp2t', flv: 'video/x-flv' };
  const goodUrl = u => typeof u === 'string' && u.length > 0 && !u.startsWith('blob:') && !u.startsWith('data:');
  const resolveUrl = u => { try { return new URL(u, location.href).href; } catch { return ''; } };
  const isDisguisedPng = u => {
    if (!u || typeof u !== 'string') return false;
    const base = u.split(/[?#]/)[0], fn = base.substring(base.lastIndexOf('/') + 1);
    if (/x\d+\.png$/i.test(fn) || /icon|logo|favicon|splash|poster|banner|thumb/i.test(base)) return false;
    return /^(?:seg[-_]?|chunk[-_]?)?\d{3,}\.png$/i.test(fn);
  };
  const isManifest = u => /\.(m3u8|mpd)($|\?)/i.test(u);
  const isSegment = u => /\.(ts|m4s|aac|mp3)($|\?)/i.test(u) || /init\.mp4|\d{3,}\.(mp4|ts)(\?|$)/i.test(u) || isDisguisedPng(u);
  const typeFromUrl = u => { const m = u?.match(/\.(m3u8|mpd|mp4|ts|flv)($|\?)/i); return m ? MIME_MAP[m[1].toLowerCase()] : ''; };

  // ── Candidate factory + shared constants ──────────────────────
  const FLV_UNSUPPORTED = 'FLV is not supported by the Chromecast default receiver';
  const candidate = (url, type = '', time = 0, player = '', layer = '') => ({ url, type: type || '', time: time || 0, player, layer });
  // document.cookie throws SecurityError in cross-origin iframes; the snippet
  // is sometimes pasted into a player iframe where reads are blocked.
  const safeCookies = () => { try { return document.cookie; } catch { return ''; } };

  // ── Snapshot ──────────────────────────────────────────────────
  // Activity heuristic: playing > has a src > visible & large > long duration.
  // Ad players are usually muted, small, offscreen, or named ad/teaser.
  const videoScore = v => {
    try {
      const r = v.getBoundingClientRect?.(), c = `${v.className} ${v.id}`;
      return (!v.paused ? 100 : 0) +
        (goodUrl(v.currentSrc) ? 40 : 0) +
        (goodUrl(v.src) ? 20 : 0) +
        (v.readyState > 2 ? 10 : 0) +
        (isFinite(v.duration) ? Math.min(v.duration / 600, 10) : 0) +
        (r?.width >= 100 && r?.height >= 60 ? 30 : 0) -
        (v.muted ? 20 : 0) -
        (/ad|ads|teaser|trailer|banner/i.test(c) ? 50 : 0);
    } catch { return 0; }
  };

  const snapshot = extra => {
    let videos = [];
    try { videos = [...document.querySelectorAll('video')].sort((a, b) => videoScore(b) - videoScore(a)); } catch {}
    const resources = extra?.slice() || [], seen = new Set(resources);
    try {
      performance.getEntriesByType('resource')
        .map(r => r.name)
        .filter(u => isDisguisedPng(u) || (!/(\.css|\.js|\.json|\.woff2?|\.svg|\.jpe?g|\.webp|\.gif|\.png)(\?|$)/i.test(u) && /(\.m3u8|\.mpd|\.mp4|\.ts|\.flv|\.m4s)/i.test(u)))
        .reverse()
        .forEach(u => { if (!seen.has(u)) { seen.add(u); resources.push(u); } });
    } catch {}
    return { videos, active: videos[0] || null, resources, engines: [] };
  };

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
  const detectHls = v => {
    const u = [v.hlsPlayer, v._hlsPlayer, v._hls, v.hls].find(h => goodUrl(h?.url))?.url;
    return u ? candidate(u, HLS_MIME, v.currentTime, 'hls.js', 'engine') : null;
  };

  const detectDash = v => {
    const u = (v._dashjs_player || v.dashPlayer || v.dashjsPlayer)?.getSource?.();
    return goodUrl(u) ? candidate(u, DASH_MIME, v.currentTime, 'dash.js', 'engine') : null;
  };

  const detectShaka = v => {
    const u = v.shakaPlayer?.getAssetUri?.();
    return goodUrl(u) ? candidate(u, typeFromUrl(u), v.currentTime, 'shaka', 'engine') : null;
  };

  const detectFlv = v => {
    // FLV cannot play on the Chromecast default receiver - detect to warn only.
    const f = v.flvPlayer || v._flv;
    const u = f?.url || f?.mediaInfo?.url || f?.mediaDataSource?.url || f?._mediaInfo?.url;
    if (goodUrl(u)) {
      const c = candidate(u, 'video/x-flv', v.currentTime, 'flv.js', 'engine');
      c.unsupported = FLV_UNSUPPORTED;
      return c;
    }
    return null;
  };

  const fiberCandidate = (u, type, v) => goodUrl(u) ? candidate(u, type === 'hls' ? HLS_MIME : type === 'dash' ? DASH_MIME : (type || typeFromUrl(u)), v.currentTime, 'react-fiber', 'engine') : null;

  const detectReactFiber = v => {
    if (!v) return null;
    const fiberKey = Object.keys(v).find(k => k.startsWith('__reactFiber') || k.startsWith('__reactInternalInstance'));
    if (!fiberKey) return null;

    let fiber = v[fiberKey], depth = 0;
    while (fiber && depth < 40) {
      let state = fiber.memoizedState;
      while (state) {
        const s = state.memoizedState;
        if (s && typeof s === 'object') {
          const c = fiberCandidate(s.url, s.type, v) || fiberCandidate(s.current?.value?.url, s.current?.value?.type, v) || (Array.isArray(s.sources) && fiberCandidate(s.sources[0]?.url, s.sources[0]?.type, v));
          if (c) return c;
        }
        state = state.next;
      }
      if (fiber.memoizedProps) {
        const p = fiber.memoizedProps;
        const c = fiberCandidate(p.src, p.type, v) || fiberCandidate(p.stream?.url, p.stream?.type, v);
        if (c) return c;
      }
      fiber = fiber.return;
      depth++;
    }
    return null;
  };

  const markActive = (c, v, ctx) => {
    if (c) c.fromActive = (v === ctx.active);
    return c;
  };

  // ── Engine reference scan (generic layer) ─────────────────────
  // Covers hls.js / dash.js / shaka / flv.js / react-fiber under any brand wrapper
  // (Video.js, MediaElement.js, Plyr, Clappr, Flowplayer, Kaltura, ...).
  const engineScan = ctx => {
    for (const v of ctx.videos) {
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
    const g = window.hls || window.Hls?._instance;
    if (g && goodUrl(g.url)) {
      const v = ctx.videos.find(el => (el.currentSrc || el.src || '').startsWith('blob:'));
      const c = candidate(g.url, HLS_MIME, v?.currentTime, 'hls.js', 'engine');
      return v ? markActive(c, v, ctx) : c;
    }
    return null;
  };

  // ── Generic layer ─────────────────────────────────────────────
  const nativeOn = (v, player) => {
    const url = v.currentSrc || v.src;
    return goodUrl(url) ? candidate(url, typeFromUrl(url), v.currentTime, player, 'generic') : null;
  };

  const html5Video = ctx => {
    for (const v of ctx.videos) {
      const c = nativeOn(v, 'html5');   // blob: (MSE) -> null; real URL lives on the engine
      if (c) return markActive(c, v, ctx);
    }
    return null;
  };

  // ── Network layer ─────────────────────────────────────────────
  const networkScan = ctx => {
    const cleaned = ctx.resources.map(url => ({ url, base: url.split(/[?#]/)[0] }));
    // Pass 1: manifests - these are the real streams (exact URL).
    for (const c of cleaned) {
      if (/\.m3u8/i.test(c.base)) return candidate(c.url, HLS_MIME, 0, 'network', 'network');
      if (/\.mpd/i.test(c.base)) return candidate(c.url, DASH_MIME, 0, 'network', 'network');
    }
    // Pass 2: direct media files / disguised chunks.
    for (const { url, base } of cleaned) {
      if (/\.mp4/i.test(base) && !/init\.mp4|\d{3,}\.mp4/i.test(url)) return candidate(url, 'video/mp4', 0, 'network', 'network');
      if (/\.ts/i.test(base)) return candidate(url, 'video/mp2t', 0, 'network', 'network');
      if (isDisguisedPng(url)) return candidate(url.replace(/\/[^/]+$/, '/playlist.m3u8'), HLS_MIME, 0, 'network', 'network');
      if (/\.flv/i.test(base)) ctx.unsupported = ctx.unsupported || { url, type: 'video/x-flv', time: 0, player: 'network', layer: 'network', unsupported: FLV_UNSUPPORTED };
    }
    return null;
  };

  // ── Small helpers used by player detectors ────────────────────
  const videoIn = (ctx, sel) => ctx.videos.find(v => {
    try { if (v.closest?.(sel)) return true; } catch {}
    return v.parentClass?.includes(sel.slice(1));  // fake-DOM fallback
  }) || null;

  // Run fn over every matching container; return its first non-null result.
  const forEachContainer = (sel, fn) => {
    try {
      for (const el of document.querySelectorAll(sel)) {
        const r = fn(el);
        if (r) return r;
      }
    } catch {}
    return null;
  };

  // Brand players whose engine (hls.js / dash.js / shaka) lives on a <video>
  // inside a known container, with the native <video> as the final fallback.
  const containerPlayer = (ctx, sel, name, present) => {
    if (present && !present()) return null;
    const v = videoIn(ctx, sel);
    return v ? (detectHls(v) || detectDash(v) || detectShaka(v) || markActive(nativeOn(v, name), v, ctx)) : null;
  };

  // ── Player detectors (brands) ─────────────────────────────────
  // JW Player - legacy window.sources global.
  const jwSources = () => {
    const s = window.sources?.[0];
    const u = s?.file || s?.src;
    return goodUrl(u) ? candidate(u, s.type || '', 0, 'jw-sources', 'player') : null;
  };

  // Shared JW instance -> URL/type/time extraction.
  const jwCandidate = jw => {
    if (!jw) return null;
    const item = (typeof jw.getPlaylistItem === 'function' && jw.getPlaylistItem()) || (typeof jw.getConfig === 'function' && jw.getConfig()?.playlist?.[0]);
    const src = item?.sources?.[0] || item;
    const u = src?.file || src?.src;
    return goodUrl(u) ? candidate(u, src?.type || typeFromUrl(u), typeof jw.getPosition === 'function' ? jw.getPosition() : 0, 'jw-api', 'player') : null;
  };

  // JW Player - API. jwplayer(id) only queries existing instances.
  const jwAPI = () => {
    if (typeof window.jwplayer !== 'function') return null;
    const els = document.querySelectorAll('.jwplayer, [id*="jwplayer"], [class*="jwplayer"]');
    if (!els.length) return jwCandidate(window.jwplayer());
    for (const el of els) {
      if (el.id) {
        try {
          const c = jwCandidate(window.jwplayer(el.id));
          if (c) return c;
        } catch {}
      }
    }
    return null;
  };

  // Video.js - global registry of players.
  const videoJs = () => {
    for (const id in window.videojs?.getPlayers?.() || {}) {
      const p = window.videojs.getPlayers()[id];
      if (!p || typeof p !== 'object') continue;
      const src = p.currentSource?.();
      const u = src?.src || (typeof p.src === 'function' && p.src());
      if (goodUrl(u)) return candidate(u, src?.type || typeFromUrl(u), p.currentTime?.() || 0, 'video.js', 'player');
    }
    return null;
  };

  // MediaElement.js - mejs.players registry (mep_0 ...).
  const mediaElementJs = () => {
    for (const id in window.mejs?.players || {}) {
      const media = window.mejs.players[id]?.media;
      if (!media) continue;
      let url = '', type = '';
      if (goodUrl(media.hlsPlayer?.url)) {
        url = media.hlsPlayer.url; type = HLS_MIME;
      } else if (typeof media.dashPlayer?.getSource === 'function' && goodUrl(media.dashPlayer.getSource())) {
        url = media.dashPlayer.getSource(); type = DASH_MIME;
      } else {
        url = media.originalNode?.currentSrc || media.originalNode?.src || media.currentSrc || media.src || '';
      }
      if (goodUrl(url)) return candidate(url, type || typeFromUrl(url), media.currentTime || 0, 'mediaelement', 'player');
    }
    return null;
  };

  // Plyr - no global registry; the engine lives on the .plyr <video>.
  const plyr = ctx => containerPlayer(ctx, '.plyr', 'plyr');

  // Clappr - engine lives on the .clappr <video>.
  const clappr = ctx => containerPlayer(ctx, '.clappr', 'clappr', () => !!window.Clappr);

  // Flowplayer - only query existing containers (flowplayer() with no args can create).
  const flowPlayer = () => typeof window.flowplayer === 'function' ? forEachContainer('.flowplayer, [data-flowplayer], [class*="flowplayer"]', el => {
    try {
      const fp = window.flowplayer(el);
      const url = fp?.video?.src || fp?.video?.url;
      return goodUrl(url) ? candidate(url, fp.video.type || typeFromUrl(url), fp.video.time || 0, 'flowplayer', 'player') : null;
    } catch { return null; }
  }) : null;

  // Bitmovin - read-only rule: only touch elements already carrying the
  // Bitmovin UI marker; calling bitmovin.player(el) elsewhere creates a player.
  const bitmovin = () => (window.bitmovin && typeof window.bitmovin.player === 'function') ? forEachContainer('.bmpui-container, .bitmovinplayer-container, .bmpui-ui, [class*="bmpui"]', el => {
    try {
      const p = window.bitmovin.player(el);
      if (!p) return null;
      const src = typeof p.getSource === 'function' ? p.getSource() : null;
      const url = src && (src.hls || src.dash || src.progressive);
      if (!goodUrl(url)) return null;
      return candidate(url, src.hls ? HLS_MIME : src.dash ? DASH_MIME : typeFromUrl(url), typeof p.getCurrentTime === 'function' ? p.getCurrentTime() : 0, 'bitmovin', 'player');
    } catch { return null; }
  }) : null;

  // THEOplayer - v2-v6 register instances in THEOplayer.players / PlayerList.
  const theoPlayer = () => {
    const T = window.THEOplayer || window.THEOplayerChrome;
    if (!T) return null;
    const players = T.players || T.PlayerList?.getAll?.() || [];
    for (let p of players) {
      if (p?.uid && T.PlayerList?.getPlayerByUID) { try { p = T.PlayerList.getPlayerByUID(p.uid); } catch {} }
      const url = p?.source?.sources?.[0]?.src || p?.src;
      if (goodUrl(url)) return candidate(url, typeFromUrl(url), p.currentTime || 0, 'theoplayer', 'player');
    }
    return null;
  };

  // Kaltura - enterprise wrapper; no reachable global instance, so the engine
  // scan + network scan do the real work. Detector improves provenance only.
  const kaltura = ctx => containerPlayer(ctx, '.kaltura-player', 'kaltura', () => {
    try { return !!(window.kaltura || document.querySelector('.kaltura-player-container, .kaltura-player')); } catch { return false; }
  });

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
  const merge = candidates => {
    if (!candidates?.length) return null;
    for (const c of candidates) {
      let s = 50 +
        (c.layer === 'player' || c.layer === 'engine' ? 30 : 0) +   // exact API/manifest URL
        (isManifest(c.url) ? 10 : 0) -
        (c.layer === 'network' ? 10 : 0) -                          // inferred from traffic
        (isSegment(c.url) ? 30 : 0) +
        (c.time ? 5 : 0) +
        (c.type ? 5 : 0) +
        // Strong activity preference: candidates tied to the active <video> win;
        // candidates from other (ad/teaser) elements are penalized.
        (c.fromActive === true ? 25 : c.fromActive === false ? -25 : 0);
      c.score = Math.max(0, Math.min(100, s));
    }
    candidates.sort((a, b) => b.score - a.score);
    const b = candidates[0];
    return { url: b.url, type: b.type || typeFromUrl(b.url), time: b.time || 0, player: b.player, score: b.score };
  };

  // ── Pipeline ──────────────────────────────────────────────────
  const run = extraResources => {
    const ctx = snapshot(extraResources || []);
    const candidates = [];

    for (const layer of layers) {
      for (const d of layer.detectors) {
        try {
          const c = d(ctx);
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
      log('[summary] candidates:', candidates.map(c => `${c.player} (${c.score}) ${c.url}`).join(' | ') || '(none)');
      log('[summary] merged:', merged);
    }

    if (!merged) {
      if (ctx.unsupported) {
        return console.error(`[OC] ⚠ Detected ${ctx.unsupported.player} - ${ctx.unsupported.unsupported}`);
      }
      if (window.__OC_WATCH__ === true && !run._watched) {
        run._watched = true;
        watchAndRetry();
        return;
      }
      return console.error('[OC] ❌ No active video stream found. Start video playback first.');
    }

    // ── Cast to TV ──────────────────────────────────────────────
    const streamUrl = resolveUrl(merged.url);
    if (!streamUrl) return console.error('[OC] ❌ Could not resolve stream URL.');

    const params = new URLSearchParams({
      url: streamUrl,
      title: document.title || 'Video Stream',
      referer: location.href,
      origin: location.origin,
      cookies: safeCookies(),
      contentType: merged.type || (streamUrl.includes('.m3u8') || streamUrl.includes('streamsvr') ? HLS_MIME : ''),
      currentTime: Math.floor(merged.time || 0)
    });

    const w = window.open('__BASE__/api/cast?' + params.toString(), 'origincaster_cast', 'width=1,height=1,left=99999,top=99999');
    if (w) setTimeout(() => { try { w.close(); } catch {} }, 2000);

    console.log('[OC] ⚡ Casting initiated to TV!');
    console.log('Stream:', streamUrl);
    console.log('Detected by:', merged.player, '(score ' + merged.score + ')');
    if (ctx.active && !ctx.active.paused) { try { ctx.active.pause(); } catch {} }
  };

  // ── Watch mode (opt-in) ───────────────────────────────────────
  // Some players lazy-load the manifest after user interaction. When enabled,
  // hook XHR/fetch for ~3 s and retry once with the captured URLs.
  const watchAndRetry = () => {
    console.log('[OC] 👀 Watching network for ~3 s (lazy-loaded manifest)...');
    const captured = [], seen = new Set();
    const capture = u => {
      try {
        if (!u || typeof u !== 'string' || !/(\.m3u8|\.mpd|\.mp4|\.ts|\.flv)/i.test(u) || seen.has(u)) return;
        seen.add(u);
        captured.push(u);
      } catch {}
    };

    let origFetch = null;
    try {
      origFetch = window.fetch;
      if (typeof origFetch === 'function') {
        window.fetch = function(input, ...args) {
          try { capture(typeof input === 'string' ? input : input?.url); } catch {}
          return origFetch.apply(this, [input, ...args]);
        };
      }
    } catch {}

    let origOpen = null;
    try {
      origOpen = XMLHttpRequest.prototype.open;
      XMLHttpRequest.prototype.open = function(method, url, ...args) {
        try { capture(String(url)); } catch {}
        return origOpen.apply(this, [method, url, ...args]);
      };
    } catch {}

    setTimeout(() => {
      try { if (origFetch) window.fetch = origFetch; } catch {}
      try { if (origOpen) XMLHttpRequest.prototype.open = origOpen; } catch {}
      run(captured);
    }, 3000);
  };

  // ── Test hooks (never present on a real page) ─────────────────
  if (window.__OC_EXPOSE__) {
    window.__OC__ = {
      snapshot, run, merge, layers,
      utils: { goodUrl, resolveUrl, isManifest, isSegment, isDisguisedPng, typeFromUrl },
      detectors: { jwSources, jwAPI, videoJs, mediaElementJs, plyr, clappr, flowPlayer, bitmovin, theoPlayer, kaltura, engineScan, html5Video, networkScan, detectHls, detectDash, detectShaka, detectFlv, detectReactFiber }
    };
  }

  // ── One-shot run ──────────────────────────────────────────────
  if (window.__OC_AUTORUN__ !== false) {
    run();
  }
})();
