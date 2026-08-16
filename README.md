# origin-caster

Stream video to your physical Chromecast or Android TV from websites whose players completely lack native Google Cast support.

Many video streaming sites protect their media using strict `Referer`/`Origin` checks, cross-origin (CORS) blocks, session cookies, and tokenized HLS streams with AES-128 encryption keys. Physical Chromecast receivers cannot access browser cookies or pass these checks directly.

**origin-caster** detects the active stream URL and captures your browser's session headers, commanding the physical TV to stream through a local proxy that fetches video segments on the TV's behalf — the TV itself decrypts the AES-128 keys, which the proxy relays along with the video.


## Quickstart

### 1. Build & Run
```bash
# Build binary
make build

# Start origin-caster
./bin/origin-caster -tv-name "Living Room TV"
```

### 2. Cast from Any Browser Tab
1. Open the video on any streaming website.
2. Open Chrome DevTools (**F12** or **Cmd+Option+I**) and switch to the **Console** tab.
3. If the video player is embedded, select the player `<iframe>` from the Console dropdown (defaults to `top`).
4. Open **http://localhost:8888** in your browser, click **Copy Extraction Snippet**, paste it into the Console, and press **Enter**.
5. Playback starts on the TV with live remote controls (play, pause, seek, volume) on the web dashboard (launch takes a few seconds; Android TV can take up to ~20 s).

## CLI Options

Flags are grouped by what they control: the virtual device (what your browser casts to), the web server, and the physical TV.

### Virtual device (what your browser casts to)

| Flag | Default | Description |
|------|---------|-------------|
| `-device-name` | `origin-caster (Proxy)` | Name of the virtual Chromecast, shown in the Cast menu |
| `-cast-port` | `8009` | Port for Cast V2 control connections (TLS). Your browser casts to this port |
| `-dial-port` | `8008` | Port for the DIAL discovery service. TVs and apps use this to find the device |
| `-lan-ip` | auto-detected | LAN IP advertised to the browser and TV. Set it if auto-detection picks the wrong network |

### Web server (dashboard + media proxy)

| Flag | Default | Description |
|------|---------|-------------|
| `-http-addr` | `:8888` | Address (host:port) of the web dashboard and media proxy. You open the dashboard here; the TV fetches video here. `:8888` listens on all interfaces and advertises the detected LAN IP. To force an address, give a host, e.g. `-http-addr 192.168.1.50:8888` — the TV must be able to reach it |

### Physical TV (the device that plays the video)

| Flag | Default | Description |
|------|---------|-------------|
| `-tv-name` | `""` | Pick the physical TV by name (case-insensitive substring), e.g. `-tv-name "Living Room TV"` |
| `-tv-ip` | `""` | IP address of the physical TV. Auto-discovered if empty |
| `-tv-port` | `8009` | Port of the physical TV's Cast V2 service. Only needed if the TV uses a non-standard port |

### Other

| Flag | Default | Description |
|------|---------|-------------|
| `-list` | `false` | Scan the LAN for Chromecast devices, print them, and exit |
| `-scan-timeout` | `3s` | Timeout for the device scan |
| `-log-file` | `""` | Also write logs to this file (default: stderr only) |
| `-v` / `-verbose` | `false` | Verbose protocol logging |


## Ports

origin-caster uses three ports: 8008 (DIAL discovery), 8009 (Cast control), and 8888 (dashboard + media proxy). Their flags (`-dial-port`, `-cast-port`, `-tv-port`, `-http-addr`) are listed in [CLI Options](#cli-options) above. For the full ports table, discovery details, and protocol references, see [CONTRIBUTING.md](CONTRIBUTING.md).


## Development & Architecture

For architecture diagrams, REST API documentation, protocol internals, and contribution guidelines, see [CONTRIBUTING.md](CONTRIBUTING.md).
