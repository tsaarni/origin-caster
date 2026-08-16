let isDraggingSeek = false;
let isDraggingVol = false;
let currentPlaybackState = 'IDLE';
let currentDuration = 0;
let currentSeekTime = 0;
let isMuted = false;

// Browser snippet for the streaming site's console. It must be self-contained:
// the page cannot load code from localhost - Chrome's Private/Local Network
// Access gates every code-loading transport from public sites (fetch/XHR/
// subresources since 142, WebSockets since 147) - so cast.js is the single
// source of truth and the server injects the minified one-liner below at
// startup (from cast.js). The cast is sent with window.open() - a top-level
// navigation, which is exempt from CORS/PNA/LNA - to /api/cast, which answers
// with an auto-closing HTML page for navigations and JSON for API clients
// (content negotiation).
const ONELINER_SNIPPET_CORE = '/*__SNIPPET__*/';

// __BASE__ is replaced with the dashboard's own origin when the page loads.
const ONELINER_SNIPPET = ONELINER_SNIPPET_CORE.replace('__BASE__', location.origin);

function formatTime(totalSeconds) {
  if (isNaN(totalSeconds) || totalSeconds < 0) totalSeconds = 0;
  totalSeconds = Math.floor(totalSeconds);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  return (hours > 0 ? hours + ':' : '') + String(minutes).padStart(2, '0') + ':' + String(seconds).padStart(2, '0');
}

async function apiCmd(path, method = 'POST') {
  try {
    const res = await fetch(path, { method });
    return await res.json();
  } catch (e) {
    console.error('API command error:', e);
  }
}

async function togglePlayPause() {
  const icon = document.getElementById('icon-play-pause');
  if (currentPlaybackState === 'PLAYING') {
    if (icon) {
      icon.innerText = 'play_arrow';
      icon.className = 'material-symbols-outlined icon-play';
    }
    await apiCmd('/api/pause');
  } else {
    if (icon) {
      icon.innerText = 'pause';
      icon.className = 'material-symbols-outlined icon-pause';
    }
    await apiCmd('/api/play');
  }
  updateLiveState();
}

async function stopPlayback() {
  await apiCmd('/api/stop');
  updateLiveState();
}

async function stepTime(deltaSeconds) {
  await apiCmd('/api/seek?delta=' + deltaSeconds);
  updateLiveState();
}

async function toggleMute() {
  isMuted = !isMuted;
  const currentVol = parseFloat(document.getElementById('vol-slider').value) || 1.0;
  await apiCmd('/api/volume?level=' + currentVol + '&muted=' + isMuted);
  updateLiveState();
}

// Button Events
document.getElementById('btn-play-pause')?.addEventListener('click', togglePlayPause);
document.getElementById('btn-stop')?.addEventListener('click', stopPlayback);
document.getElementById('btn-rewind30')?.addEventListener('click', () => stepTime(-30));
document.getElementById('btn-rewind10')?.addEventListener('click', () => stepTime(-10));
document.getElementById('btn-forward10')?.addEventListener('click', () => stepTime(10));
document.getElementById('btn-forward30')?.addEventListener('click', () => stepTime(30));
document.getElementById('btn-mute')?.addEventListener('click', toggleMute);

// Seek Slider
const seekSlider = document.getElementById('seek-slider');
if (seekSlider) {
  seekSlider.addEventListener('input', (e) => {
    isDraggingSeek = true;
    document.getElementById('time-cur').innerText = formatTime(parseFloat(e.target.value));
  });
  seekSlider.addEventListener('change', async (e) => {
    isDraggingSeek = false;
    await apiCmd('/api/seek?seconds=' + e.target.value);
    updateLiveState();
  });
}

// Volume Slider
const volSlider = document.getElementById('vol-slider');
if (volSlider) {
  volSlider.addEventListener('input', (e) => {
    isDraggingVol = true;
    const v = parseFloat(e.target.value);
    document.getElementById('vol-text').innerText = Math.round(v * 100) + '%';
  });
  volSlider.addEventListener('change', async (e) => {
    isDraggingVol = false;
    await apiCmd('/api/volume?level=' + e.target.value + '&muted=false');
    updateLiveState();
  });
}

export async function updateLiveState() {
  try {
    const res = await fetch('/api/stats');
    if (!res.ok) return;
    const data = await res.json();

    // 1. Telemetry Stats
    const reqElem = document.getElementById('stat-requests');
    if (reqElem) reqElem.innerText = data.total_requests || 0;
    const stmElem = document.getElementById('stat-streams');
    if (stmElem) stmElem.innerText = data.active_streams || 0;
    const m3uElem = document.getElementById('stat-m3u8');
    if (m3uElem) m3uElem.innerText = data.m3u8_rewrites || 0;
    const mb = ((data.total_bytes || 0) / (1024 * 1024)).toFixed(2);
    const byteElem = document.getElementById('stat-bytes');
    if (byteElem) byteElem.innerText = mb + ' MB';

    // 2. Playback Details
    const pb = data.playback || {};
    currentPlaybackState = pb.playerState || 'IDLE';
    currentDuration = pb.duration || 0;
    currentSeekTime = pb.currentTime || 0;
    isMuted = !!pb.muted;

    const chip = document.getElementById('live-chip');
    const playIcon = document.getElementById('icon-play-pause');

    if (currentPlaybackState === 'PLAYING') {
      if (chip) { chip.className = 'm3-status-chip chip-playing'; chip.innerText = 'PLAYING'; }
      if (playIcon) { playIcon.innerText = 'pause'; playIcon.className = 'material-symbols-outlined icon-pause'; }
    } else if (currentPlaybackState === 'PAUSED') {
      if (chip) { chip.className = 'm3-status-chip chip-paused'; chip.innerText = 'PAUSED'; }
      if (playIcon) { playIcon.innerText = 'play_arrow'; playIcon.className = 'material-symbols-outlined icon-play'; }
    } else if (currentPlaybackState === 'BUFFERING') {
      if (chip) { chip.className = 'm3-status-chip chip-buffering'; chip.innerText = 'BUFFERING'; }
      if (playIcon) { playIcon.innerText = 'pause'; playIcon.className = 'material-symbols-outlined icon-pause'; }
    } else {
      if (chip) { chip.className = 'm3-status-chip chip-idle'; chip.innerText = 'STANDBY'; }
      if (playIcon) { playIcon.innerText = 'play_arrow'; playIcon.className = 'material-symbols-outlined icon-play'; }
    }

    // Title and Meta
    const nowTitle = document.getElementById('now-title');
    if (nowTitle) {
      if (pb.title) {
        nowTitle.innerText = pb.title;
      } else if (data.active_session && data.active_session.title) {
        nowTitle.innerText = data.active_session.title;
      } else if (pb.contentId) {
        nowTitle.innerText = pb.contentId;
      } else {
        nowTitle.innerText = 'No Active Stream';
      }
    }

    const nowSub = document.getElementById('now-sub');
    if (nowSub) {
      if (pb.activeApp) {
        nowSub.innerHTML = '<span class="material-symbols-outlined" style="font-size:16px;">tv</span><span>' + (pb.receiverName || 'TV') + ' - ' + pb.activeApp + '</span>';
      } else {
        nowSub.innerHTML = '<span class="material-symbols-outlined" style="font-size:16px;">tv</span><span>Ready - Waiting for stream playback</span>';
      }
    }

    const targetName = document.getElementById('target-name');
    if (targetName && (pb.receiverName || pb.receiverIP)) {
      targetName.innerText = (pb.receiverName || 'Receiver') + (pb.receiverIP ? ' - ' + pb.receiverIP : '');
    }

    // Timecode & Slider
    if (!isDraggingSeek && seekSlider) {
      document.getElementById('time-cur').innerText = formatTime(currentSeekTime);
      document.getElementById('time-dur').innerText = formatTime(currentDuration);
      seekSlider.max = currentDuration > 0 ? currentDuration : 100;
      seekSlider.value = currentSeekTime;
    }

    // Volume & Mute Icon
    if (!isDraggingVol && volSlider) {
      const vol = pb.volumeLevel !== undefined ? pb.volumeLevel : 1.0;
      volSlider.value = vol;
      document.getElementById('vol-text').innerText = Math.round(vol * 100) + '%';
      const muteIcon = document.getElementById('icon-mute');
      if (muteIcon) muteIcon.innerText = pb.muted ? 'volume_off' : (vol > 0.5 ? 'volume_up' : (vol > 0 ? 'volume_down' : 'volume_mute'));
    }

  } catch (err) {
    console.error('Live stats poll error:', err);
  }
}

setInterval(updateLiveState, 1000);
updateLiveState();

// Form Submit Handler
document.getElementById('castForm')?.addEventListener('submit', async (e) => {
  e.preventDefault();
  const toast = document.getElementById('toast');
  if (!toast) return;
  toast.style.display = 'block';
  toast.style.background = 'var(--md-sys-color-surface-container-high)';
  toast.style.color = 'var(--md-sys-color-primary)';
  toast.innerText = 'Dispatching stream to receiver...';

  const startTimeVal = parseFloat(document.getElementById('startTime')?.value) || 0;

  try {
    const res = await fetch('/api/cast', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        url: document.getElementById('url')?.value,
        title: document.getElementById('title')?.value,
        referer: document.getElementById('referer')?.value,
        origin: document.getElementById('origin')?.value,
        cookies: document.getElementById('cookies')?.value,
        currentTime: startTimeVal
      })
    });
    if (res.ok) {
      toast.style.background = 'var(--md-sys-color-tertiary-container)';
      toast.style.color = 'var(--md-sys-color-on-tertiary-container)';
      toast.innerText = '✓ Streaming initiated on TV!';
      updateLiveState();
    } else {
      const txt = await res.text();
      toast.style.background = 'var(--md-sys-color-error-container)';
      toast.style.color = 'var(--md-sys-color-on-error-container)';
      toast.innerText = 'Error: ' + txt;
    }
  } catch (err) {
    toast.style.background = 'var(--md-sys-color-error-container)';
    toast.style.color = 'var(--md-sys-color-on-error-container)';
    toast.innerText = 'Network error: ' + err.message;
  }
});

export function copyOneLiner() {
  navigator.clipboard.writeText(ONELINER_SNIPPET).then(() => {
    const btn = document.getElementById('btn-copy-snippet');
    if (btn) {
      const origText = btn.innerHTML;
      btn.innerHTML = '<span class="material-symbols-outlined" slot="icon">check</span>Copied to Clipboard!';
      setTimeout(() => { btn.innerHTML = origText; }, 2000);
    } else {
      alert('Copied extraction snippet to clipboard!');
    }
  });
}

window.copyOneLiner = copyOneLiner;
