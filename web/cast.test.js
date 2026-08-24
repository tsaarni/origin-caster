'use strict';
/**
 * Unit tests for web/cast.js - the browser extraction snippet.
 *
 * Runs with Node's built-in test runner: `node --test cast.test.js`
 * (via the Makefile `test-js` target - separate from the Go test suite). The
 * snippet is evaluated in a VM context with a fake window/document stub, so
 * the full pipeline (snapshot -> detectors -> merge -> popup URL) is exercised
 * without a browser.
 *
 * Every fixture also guards the read-only rule: detectors must never create or
 * initialize a player (verified explicitly for Bitmovin/Flowplayer below).
 */
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const SOURCE = fs.readFileSync(path.join(__dirname, 'cast.js'), 'utf8');

// ── Fake DOM ────────────────────────────────────────────────────
function makeVideo(overrides = {}) {
  return Object.assign({
    tagName: 'VIDEO',
    className: '',
    id: '',
    parentClass: '',          // container class for closest()-based lookup
    paused: true,
    muted: false,
    duration: NaN,
    readyState: 0,
    currentTime: 0,
    currentSrc: '',
    src: '',
    dataset: {},
    pauseCalls: 0,
    getBoundingClientRect() { return { width: 640, height: 360 }; },
    pause() { this.paused = true; this.pauseCalls++; },
    closest(sel) {
      const cls = sel.replace(/^\./, '');
      return String(this.parentClass).split(/\s+/).indexOf(cls) !== -1 ? this : null;
    },
  }, overrides);
}

function makeElement(overrides = {}) {
  return Object.assign({ tagName: 'DIV', className: '', id: '', dataset: {} }, overrides);
}

function makeDom({ videos = [], containers = [] } = {}) {
  const all = [...videos, ...containers];
  const hasClass = (el, cls) => String(el.className || '').split(/\s+/).indexOf(cls) !== -1;

  const matches = (el, sel) => sel.split(',').map(s => s.trim()).some((p) => {
    if (p === 'video') return el.tagName === 'VIDEO';
    const cls = p.match(/^\.([\w-]+)/);
    if (cls) return hasClass(el, cls[1]);
    const attr = p.match(/^\[(\w+)\*?="([^"]*)"\]/);
    if (attr) return String(el[attr[1]] || el.dataset[attr[1]] || '').includes(attr[2]);
    return false;
  });

  return {
    querySelectorAll(sel) { return all.filter(el => matches(el, sel)); },
    querySelector(sel) { return all.find(el => matches(el, sel)) || null; },
  };
}

// ── Environment builder ─────────────────────────────────────────
function makeEnv({ videos = [], containers = [], resources = [], globals = {}, url = 'https://site.example/watch/123' } = {}) {
  const dom = makeDom({ videos, containers });
  const calls = { open: [], logs: [], timeouts: [] };
  const loc = { href: url, origin: new URL(url).origin };

  function XHRStub() {}
  XHRStub.prototype.open = function() {};

  const w = {
    __OC_AUTORUN__: false,
    __OC_EXPOSE__: true,
    location: loc,
    document: {
      title: 'Test Page',
      cookie: 'session=abc',
      querySelectorAll: dom.querySelectorAll,
      querySelector: dom.querySelector,
    },
    performance: { getEntriesByType: () => resources.map(name => ({ name })) },
    console: {
      log: (...a) => calls.logs.push('LOG: ' + a.join(' ')),
      error: (...a) => calls.logs.push('ERR: ' + a.join(' ')),
    },
    setTimeout: (fn) => { calls.timeouts.push(fn); return 1; },
    clearTimeout: () => {},
    fetch: () => Promise.resolve(),
    URL,
    URLSearchParams,
    XMLHttpRequest: XHRStub,
    open: (u) => { calls.open.push(u); return null; },
  };
  w.window = w;
  w.window = w;
  Object.assign(w, globals);

  const ctx = vm.createContext(w);
  vm.runInContext(SOURCE, ctx);
  w.__OC__.calls = calls;
  return w;
}

function castUrl(w) {
  return w.__OC__.calls.open[0] || '';
}
function param(w, key) {
  const q = castUrl(w).split('?')[1] || '';
  return new URLSearchParams(q).get(key);
}
function runWith(w, extra) {
  w.__OC__.run(extra || []);
}

// ── Snapshot ────────────────────────────────────────────────────
test('snapshot sorts videos by activity (playing, with src, visible)', () => {
  const ads = makeVideo({ className: 'ad-container', muted: true, paused: true, currentSrc: 'https://ads.example.com/ad.mp4', readyState: 4 });
  const main = makeVideo({ paused: false, currentSrc: 'https://cdn.example.com/main.mp4', readyState: 4 });
  const w = makeEnv({ videos: [ads, main] });
  const snap = w.__OC__.snapshot();
  assert.equal(snap.videos[0], main, 'playing main video must rank first');
  assert.equal(snap.active, main);
});

test('snapshot filters non-stream resources but keeps disguised png chunks', () => {
  const resources = [
    'https://site.example/app.js',
    'https://site.example/style.css',
    'https://site.example/thumb.png',
    'https://site.example/live/12345.png',          // disguised TS chunk
    'https://cdn.example.com/seg-0001.ts',
    'https://cdn.example.com/master.m3u8',
  ];
  const w = makeEnv({ resources });
  const snap = w.__OC__.snapshot();
  const names = snap.resources.join(' ');
  assert.ok(!names.includes('app.js'), 'css/js excluded');
  assert.ok(!names.includes('style.css'), 'css excluded');
  assert.ok(!names.includes('thumb.png'), 'plain png excluded');
  assert.ok(names.includes('12345.png'), 'disguised chunk kept');
  assert.ok(names.includes('seg-0001.ts'));
  assert.ok(names.includes('master.m3u8'));
});

// ── Generic / engine layers ─────────────────────────────────────
test('html5 layer extracts currentSrc and rejects blob:', () => {
  const v = makeVideo({ currentSrc: 'https://cdn.example.com/movie.mp4', currentTime: 61 });
  const w = makeEnv({ videos: [v] });
  const c = w.__OC__.detectors.html5Video(w.__OC__.snapshot());
  assert.equal(c.url, 'https://cdn.example.com/movie.mp4');
  assert.equal(c.type, 'video/mp4');
  assert.equal(c.time, 61);

  const blobV = makeVideo({ currentSrc: 'blob:https://site.example/uuid' });
  const w2 = makeEnv({ videos: [blobV] });
  assert.equal(w2.__OC__.detectors.html5Video(w2.__OC__.snapshot()), null, 'blob: never accepted');
});

test('engine scan: hls.js attached to video (hlsPlayer/_hlsPlayer/_hls/hls)', () => {
  for (const prop of ['hlsPlayer', '_hlsPlayer', '_hls', 'hls']) {
    const v = makeVideo({ currentSrc: 'blob:https://site.example/x', [prop]: { url: 'https://cdn.example.com/live/master.m3u8' } });
    const w = makeEnv({ videos: [v] });
    const c = w.__OC__.detectors.engineScan(w.__OC__.snapshot());
    assert.equal(c.player, 'hls.js', prop);
    assert.equal(c.url, 'https://cdn.example.com/live/master.m3u8');
    assert.equal(c.type, 'application/x-mpegurl');
  }
});

test('engine scan: hls.js globals (window.hls / window.Hls._instance)', () => {
  const v = makeVideo({ currentSrc: 'blob:https://site.example/x' });
  for (const globals of [{ hls: { url: 'https://cdn.example.com/live.m3u8' } },
                         { Hls: { _instance: { url: 'https://cdn.example.com/live2.m3u8' } } }]) {
    const w = makeEnv({ videos: [v], globals });
    const c = w.__OC__.detectors.engineScan(w.__OC__.snapshot());
    assert.ok(c && /live2?\.m3u8$/.test(c.url));
    assert.equal(c.type, 'application/x-mpegurl');
  }
});

test('engine scan: dash.js via _dashjs_player / dashPlayer / data-dashjs-player', () => {
  for (const prop of ['_dashjs_player', 'dashPlayer', 'dashjsPlayer']) {
    const v = makeVideo({ [prop]: { getSource: () => 'https://cdn.example.com/vod/manifest.mpd' } });
    const w = makeEnv({ videos: [v] });
    const c = w.__OC__.detectors.engineScan(w.__OC__.snapshot());
    assert.equal(c.player, 'dash.js', prop);
    assert.equal(c.url, 'https://cdn.example.com/vod/manifest.mpd');
    assert.equal(c.type, 'application/dash+xml');
  }
});

test('engine scan: shaka getAssetUri with extension-based type', () => {
  const v = makeVideo({ shakaPlayer: { getAssetUri: () => 'https://cdn.example.com/dash/manifest.mpd' } });
  const w = makeEnv({ videos: [v] });
  const c = w.__OC__.detectors.engineScan(w.__OC__.snapshot());
  assert.equal(c.player, 'shaka');
  assert.equal(c.type, 'application/dash+xml');
});

test('engine scan: flv.js is detected but flagged unsupported', () => {
  const v = makeVideo({ flvPlayer: { mediaInfo: { url: 'https://cdn.example.com/live.flv' } } });
  const w = makeEnv({ videos: [v] });
  const snap = w.__OC__.snapshot();
  const c = w.__OC__.detectors.engineScan(snap);
  assert.ok(!c, 'unsupported candidate is not returned as castable');
  assert.ok(snap.unsupported && snap.unsupported.player === 'flv.js');
});

test('engine scan: React Fiber internal state / ref extraction', () => {
  const v = makeVideo({
    currentTime: 42,
    __reactFiber$test: {
      memoizedState: {
        memoizedState: {
          current: {
            value: {
              type: 'hls',
              url: 'https://workers.dev/?payload=test12345&headers=test'
            }
          }
        },
        next: null
      }
    }
  });
  const w = makeEnv({ videos: [v] });
  const c = w.__OC__.detectors.engineScan(w.__OC__.snapshot());
  assert.equal(c.player, 'react-fiber');
  assert.equal(c.url, 'https://workers.dev/?payload=test12345&headers=test');
  assert.equal(c.type, 'application/x-mpegurl');
  assert.equal(c.time, 42);
});

test('snapshot and network scan reject dimensioned icons/splash and keep true disguised chunks', () => {
  const w = makeEnv();
  assert.ok(!w.__OC__.utils.isDisguisedPng('https://site.example/android-chrome-192x192.png'));
  assert.ok(!w.__OC__.utils.isDisguisedPng('https://site.example/favicon-32x32.png'));
  assert.ok(!w.__OC__.utils.isDisguisedPng('https://site.example/splash_screens/1920x1080.png'));
  assert.ok(!w.__OC__.utils.isDisguisedPng('https://site.example/assets/icon-512.png'));
  assert.ok(w.__OC__.utils.isDisguisedPng('https://site.example/live/12345.png'));
  assert.ok(w.__OC__.utils.isDisguisedPng('https://site.example/hls/seg-001.png'));
});

// ── Player layer ────────────────────────────────────────────────
test('JW Player: window.sources global', () => {
  const w = makeEnv({ globals: { sources: [{ file: 'https://cdn.example.com/jw.m3u8', type: 'application/x-mpegurl' }] } });
  runWith(w);
  assert.match(castUrl(w), /\/api\/cast\?/);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/jw.m3u8');
  assert.equal(param(w, 'contentType'), 'application/x-mpegurl');
});

test('JW Player: API via getPlaylistItem + getPosition', () => {
  const jw = {
    getPosition: () => 37,
    getPlaylistItem: () => ({ file: 'https://cdn.example.com/movie.m3u8', type: 'application/x-mpegurl' }),
    getConfig: () => ({}),
  };
  const w = makeEnv({ globals: { jwplayer: () => jw } });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/movie.m3u8');
  assert.equal(param(w, 'currentTime'), '37');
});

test('JW Player: API by container id, getConfig fallback', () => {
  let calledWith = null;
  const jw = {
    getPosition: () => 0,
    getPlaylistItem: () => null,
    getConfig: () => ({ playlist: [{ sources: [{ file: 'https://cdn.example.com/cfg.m3u8', type: 'application/x-mpegurl' }] }] }),
  };
  const w = makeEnv({
    containers: [makeElement({ id: 'jwplayer-1', className: 'jwplayer' })],
    globals: { jwplayer: (id) => { calledWith = id; return jw; } },
  });
  runWith(w);
  assert.equal(calledWith, 'jwplayer-1');
  assert.equal(param(w, 'url'), 'https://cdn.example.com/cfg.m3u8');
});

test('Video.js: getPlayers registry, blob: source falls through to engine', () => {
  const v = makeVideo({ hlsPlayer: { url: 'https://cdn.example.com/vjs.m3u8' }, currentSrc: 'blob:https://site.example/x' });
  const players = {
    v1: { currentSource: () => ({ src: 'blob:https://site.example/x', type: '' }), currentTime: () => 12, src: () => 'blob:https://site.example/x' },
    v2: { currentSource: () => ({ src: 'https://cdn.example.com/direct.mp4', type: 'video/mp4' }), currentTime: () => 5, src: () => 'https://cdn.example.com/direct.mp4' },
  };
  const w = makeEnv({ videos: [v], globals: { videojs: { getPlayers: () => players } } });
  runWith(w);
  // video.js detector skips blob:, engine scan finds the hls.js manifest.
  assert.equal(param(w, 'url'), 'https://cdn.example.com/vjs.m3u8');
});

test('MediaElement.js: hlsPlayer on wrapped media', () => {
  const media = {
    hlsPlayer: { url: 'https://cdn.example.com/mejs.m3u8' },
    dashPlayer: null,
    originalNode: { currentSrc: 'blob:https://site.example/x', src: 'blob:https://site.example/x' },
    currentTime: 9,
  };
  const w = makeEnv({ globals: { mejs: { players: { mep_0: { media } } } } });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/mejs.m3u8');
  assert.equal(param(w, 'currentTime'), '9');
});

test('MediaElement.js: dashPlayer.getSource and originalNode.currentSrc', () => {
  const media = {
    hlsPlayer: null,
    dashPlayer: { getSource: () => 'https://cdn.example.com/mejs/manifest.mpd' },
    currentTime: 3,
  };
  const w = makeEnv({ globals: { mejs: { players: { mep_0: { media } } } } });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/mejs/manifest.mpd');
  assert.equal(param(w, 'contentType'), 'application/dash+xml');
});

test('Plyr: engine found on video inside .plyr container', () => {
  const v = makeVideo({ parentClass: 'plyr', hlsPlayer: { url: 'https://cdn.example.com/plyr.m3u8' } });
  const w = makeEnv({ videos: [v] });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/plyr.m3u8');
});

test('Clappr: engine found on video inside .clappr container', () => {
  const v = makeVideo({ parentClass: 'clappr', hlsPlayer: { url: 'https://cdn.example.com/clappr.m3u8' } });
  const w = makeEnv({ videos: [v], globals: { Clappr: {} } });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/clappr.m3u8');
});

test('Flowplayer: only queried via existing container (read-only)', () => {
  let calls = 0;
  const fp = { video: { src: 'https://cdn.example.com/fp.mp4', type: 'video/mp4', time: 15 } };
  const w = makeEnv({
    containers: [makeElement({ className: 'flowplayer' })],
    globals: { flowplayer: () => { calls++; return fp; } },
  });
  runWith(w);
  assert.equal(calls, 1, 'flowplayer() must only be called for a real container');
  assert.equal(param(w, 'url'), 'https://cdn.example.com/fp.mp4');
  assert.equal(param(w, 'currentTime'), '15');
});

test('Flowplayer: never invoked when no container exists', () => {
  let calls = 0;
  const w = makeEnv({ globals: { flowplayer: () => { calls++; return { video: { src: 'https://x.example/a.mp4' } }; } } });
  runWith(w);
  assert.equal(calls, 0, 'read-only rule: no-arg flowplayer() must never be called');
});

test('Bitmovin: read-only - not invoked without a bmpui container', () => {
  let playerCalls = 0;
  const w = makeEnv({
    globals: { bitmovin: { player: () => { playerCalls++; return {}; } } },
  });
  runWith(w);
  assert.equal(playerCalls, 0, 'bitmovin.player() must never be called on a plain page');
});

test('Bitmovin: source extraction on bmpui container (hls preferred)', () => {
  const src = { hls: 'https://cdn.example.com/bitmovin/master.m3u8', dash: 'https://cdn.example.com/bitmovin/manifest.mpd' };
  const w = makeEnv({
    containers: [makeElement({ className: 'bmpui-container' })],
    globals: { bitmovin: { player: () => ({ getSource: () => src, getCurrentTime: () => 22 }) } },
  });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/bitmovin/master.m3u8');
  assert.equal(param(w, 'currentTime'), '22');
});

test('THEOplayer: registry via THEOplayer.players', () => {
  const p = { uid: 'p1', source: { sources: [{ src: 'https://cdn.example.com/theo/master.m3u8' }] }, currentTime: 40 };
  const w = makeEnv({
    globals: { THEOplayer: { players: [p], PlayerList: { getPlayerByUID: () => p } } },
  });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/theo/master.m3u8');
  assert.equal(param(w, 'currentTime'), '40');
});

test('Kaltura: container present, engine scan inside it', () => {
  const v = makeVideo({ parentClass: 'kaltura-player', hlsPlayer: { url: 'https://cdn.example.com/kaltura.m3u8' } });
  const w = makeEnv({ videos: [v], containers: [makeElement({ className: 'kaltura-player' })] });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/kaltura.m3u8');
});

// ── Network layer ───────────────────────────────────────────────
test('network scan prefers manifests over segments', () => {
  const resources = [
    'https://cdn.example.com/seg-0001.ts',
    'https://cdn.example.com/live/master.m3u8',   // older request, but a manifest
    'https://site.example/page.css',
  ];
  const w = makeEnv({ resources });
  const c = w.__OC__.detectors.networkScan(w.__OC__.snapshot());
  assert.equal(c.url, 'https://cdn.example.com/live/master.m3u8');
  assert.equal(c.type, 'application/x-mpegurl');
});

test('network scan: disguised png chunk derives playlist URL', () => {
  const w = makeEnv({ resources: ['https://site.example/streamsvr/12345.png'] });
  const c = w.__OC__.detectors.networkScan(w.__OC__.snapshot());
  assert.equal(c.url, 'https://site.example/streamsvr/playlist.m3u8');
  assert.equal(c.type, 'application/x-mpegurl');
});

test('network scan: flv flagged unsupported', () => {
  const w = makeEnv({ resources: ['https://cdn.example.com/live/stream.flv'] });
  const snap = w.__OC__.snapshot();
  const c = w.__OC__.detectors.networkScan(snap);
  assert.ok(!c, 'unsupported candidate not returned as castable');
  assert.ok(snap.unsupported && /FLV/.test(snap.unsupported.unsupported));
});

// ── Merger ──────────────────────────────────────────────────────
test('merge: player/engine API URL beats network-scanned URL', () => {
  const w = makeEnv();
  const merged = w.__OC__.merge([
    { url: 'https://cdn.example.com/seg-0001.ts', type: 'video/mp2t', layer: 'network', player: 'network' },
    { url: 'https://cdn.example.com/live.m3u8', type: 'application/x-mpegurl', layer: 'engine', player: 'hls.js' },
  ]);
  assert.equal(merged.url, 'https://cdn.example.com/live.m3u8');
  assert.equal(merged.player, 'hls.js');
});

test('merge: explicit mime beats extension inference; time passes through', () => {
  const w = makeEnv();
  const merged = w.__OC__.merge([
    { url: 'https://cdn.example.com/movie.m3u8', type: '', time: 0, layer: 'generic', player: 'html5' },
    { url: 'https://cdn.example.com/movie.m3u8', type: 'application/x-mpegurl', time: 12, layer: 'player', player: 'mejs' },
  ]);
  assert.equal(merged.type, 'application/x-mpegurl');
  assert.equal(merged.time, 12);
});

// ── Multi-video: ad avoidance ───────────────────────────────────
test('multi-video: muted offscreen ad player loses to active main player', () => {
  const ad = makeVideo({ className: 'ad-banner', muted: true, paused: false,
                         getBoundingClientRect: () => ({ width: 0, height: 0 }),
                         hlsPlayer: { url: 'https://ads.example.com/ad.m3u8' } });
  const main = makeVideo({ paused: false, readyState: 4,
                           hlsPlayer: { url: 'https://cdn.example.com/main.m3u8' } });
  const w = makeEnv({ videos: [ad, main] });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/main.m3u8', 'active main player must win over ad');
});

test('multi-video: active direct mp4 beats non-active hls.js ad', () => {
  const ad = makeVideo({ className: 'ad-banner', muted: true, paused: false,
                         getBoundingClientRect: () => ({ width: 0, height: 0 }),
                         hlsPlayer: { url: 'https://ads.example.com/ad.m3u8' } });
  const main = makeVideo({ paused: false, currentSrc: 'https://cdn.example.com/main.mp4', readyState: 4 });
  const w = makeEnv({ videos: [ad, main] });
  runWith(w);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/main.mp4', 'active main video must win over ad engine');
});

// ── Watch mode ──────────────────────────────────────────────────
test('watch mode: lazy-loaded manifest is caught via fetch hook', () => {
  const w = makeEnv({ globals: { __OC_WATCH__: true } }); // no videos, no resources -> first run fails
  runWith(w);
  assert.equal(castUrl(w), '', 'no cast on first run');
  assert.equal(w.__OC__.calls.timeouts.length, 1, 'watch retry scheduled');

  // Simulate the page lazy-loading the manifest during the watch window.
  w.fetch('https://cdn.example.com/lazy/live.m3u8');

  w.__OC__.calls.timeouts[0](); // retry after the 3 s capture window
  assert.match(castUrl(w), /\/api\/cast\?/);
  assert.equal(param(w, 'url'), 'https://cdn.example.com/lazy/live.m3u8');
});

test('no cast attempt when nothing is found (one-shot error)', () => {
  const w = makeEnv({ globals: { bitmovin: { player: () => ({}) } } });
  runWith(w);
  assert.equal(castUrl(w), '');
  assert.ok(w.__OC__.calls.logs.some(l => l.includes('No active video stream found')));
});

// ── Full pipeline sanity ────────────────────────────────────────
test('end-to-end: hls.js site casts with referer/origin/cookies', () => {
  const v = makeVideo({ paused: false, hlsPlayer: { url: '/live/master.m3u8' } });
  const w = makeEnv({ videos: [v], url: 'https://site.example/watch/abc?t=1' });
  runWith(w);
  assert.match(castUrl(w), /^__BASE__\/api\/cast\?/);
  assert.equal(param(w, 'url'), 'https://site.example/live/master.m3u8', 'relative URL resolved against page');
  assert.equal(param(w, 'referer'), 'https://site.example/watch/abc?t=1');
  assert.equal(param(w, 'origin'), 'https://site.example');
  assert.equal(param(w, 'cookies'), 'session=abc');
  assert.equal(param(w, 'title'), 'Test Page');
});

// ── Dashboard app.js & snippet injection sanity ─────────────────
test('app.js: snippet core injection, origin replacement, and clipboard copy', async () => {
  const APP_SOURCE = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  // Simulate Go server-side json.Marshal injection
  const injectedApp = APP_SOURCE.replace('/*__SNIPPET__*/', JSON.stringify(SOURCE));

  let copiedText = '';
  const appSandbox = {
    location: { origin: 'http://127.0.0.1:8888', href: 'http://127.0.0.1:8888/' },
    document: { getElementById: () => null, title: 'origin-caster' },
    navigator: {
      clipboard: {
        writeText: (txt) => { copiedText = txt; return Promise.resolve(); }
      }
    },
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    setInterval: () => {},
    alert: () => {},
  };
  appSandbox.window = appSandbox;

  const ctx = vm.createContext(appSandbox);
  vm.runInContext(injectedApp.replace(/export\s+/g, ''), ctx);

  assert.ok(typeof appSandbox.copyOneLiner === 'function', 'copyOneLiner must be defined');
  await appSandbox.copyOneLiner();
  assert.ok(copiedText.includes('http://127.0.0.1:8888/api/cast?'), 'origin interpolated in copied snippet');

  // Test fallback when navigator.clipboard is undefined (e.g. insecure HTTP / LAN IP)
  delete appSandbox.navigator.clipboard;
  let fallbackCopiedText = '';
  let execCommandCalled = false;
  appSandbox.document.createElement = (tag) => {
    if (tag === 'textarea') {
      const el = {
        style: {},
        value: '',
        select: () => {},
        remove: () => { appendedChild = null; }
      };
      return el;
    }
    return {};
  };
  let appendedChild = null;
  appSandbox.document.body = {
    appendChild: (el) => { appendedChild = el; },
    removeChild: () => { appendedChild = null; }
  };
  appSandbox.document.execCommand = (cmd) => {
    if (cmd === 'copy' && appendedChild) {
      fallbackCopiedText = appendedChild.value;
      execCommandCalled = true;
      return true;
    }
    return false;
  };

  await appSandbox.copyOneLiner();
  assert.ok(execCommandCalled, 'document.execCommand fallback called');
  assert.ok(fallbackCopiedText.includes('http://127.0.0.1:8888/api/cast?'), 'origin interpolated in fallback copied snippet');
});
