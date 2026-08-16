package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tsaarni/origin-caster/internal/castrelay"
	"github.com/tsaarni/origin-caster/internal/dial"
	"github.com/tsaarni/origin-caster/internal/mdns"
	"github.com/tsaarni/origin-caster/internal/netutil"
	"github.com/tsaarni/origin-caster/internal/proxy"
	"github.com/tsaarni/origin-caster/internal/server"
)

func main() {
	// Flags are grouped by role; flag.Usage prints them with explanations.
	var (
		// Virtual device - what the browser casts to
		deviceName = flag.String("device-name", "origin-caster (Proxy)", "Name of the virtual Chromecast, shown in the Cast menu")
		castPort   = flag.Int("cast-port", 8009, "Port of the Cast V2 control channel (TLS). The browser casts to this port")
		dialPort   = flag.Int("dial-port", 8008, "Port of the DIAL discovery service. TVs and apps use this to find the device")
		lanIP      = flag.String("lan-ip", "", "LAN IP advertised to the browser and TV. Auto-detected if empty")

		// Web server - dashboard and media proxy
		httpAddr = flag.String("http-addr", ":8888", "Address (host:port) of the web dashboard and media proxy. The browser opens the dashboard here and the TV fetches video here; ':8888' listens on all interfaces and advertises the detected LAN IP")

		// Physical TV - the device that plays the video
		tvName = flag.String("tv-name", "", "Name of the physical TV or Chromecast to cast to. Case-insensitive substring match; if empty, the first device found is used")
		tvIP   = flag.String("tv-ip", "", "IP address of the physical TV or Chromecast. Auto-discovered if empty")
		tvPort = flag.Int("tv-port", 8009, "Port of the physical TV's Cast V2 service")

		// Discovery and diagnostics
		scanTimeout = flag.Duration("scan-timeout", 3*time.Second, "Timeout for the device scan")
		listOnly    = flag.Bool("list", false, "Scan the LAN for Chromecast devices, print them, and exit")

		// Logging
		verbose = flag.Bool("v", false, "Verbose protocol logging")
		logFile = flag.String("log-file", "", "Also write logs to this file (default: stderr only)")
	)
	flag.BoolVar(verbose, "verbose", false, "alias for -v")
	flag.Usage = usage
	flag.Parse()

	logHandle, err := initLogger(*logFile, *verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing logger: %v\n", err)
		os.Exit(1)
	}
	if logHandle != nil {
		defer logHandle.Close()
	}

	// Validate -http-addr (host:port) early; host may be empty (':8888').
	httpHost, httpPortStr, err := net.SplitHostPort(*httpAddr)
	if err != nil {
		slog.Error("Invalid -http-addr", "addr", *httpAddr, "error", err, "tip", "expected host:port, e.g. localhost:8888 or 192.168.1.50:8888")
		os.Exit(1)
	}
	if _, err := strconv.Atoi(httpPortStr); err != nil {
		slog.Error("Invalid port in -http-addr", "port", httpPortStr)
		os.Exit(1)
	}

	// 1. Detect LAN IP & interface for the Cast stack (relay, mDNS, DIAL).
	// This is independent of -http-addr, which controls the HTTP server binding.
	var localIP net.IP
	var localIface *net.Interface

	if *lanIP != "" {
		localIP = net.ParseIP(*lanIP)
		if localIP == nil {
			slog.Error("Invalid IP specified in -lan-ip", "ip", *lanIP)
			os.Exit(1)
		}
	} else {
		netInfo, err := netutil.DetectLANIP("")
		if err != nil {
			slog.Error("Failed to detect LAN address for Cast services", "error", err)
			os.Exit(1)
		}
		localIP = netInfo.IP
		localIface = netInfo.Interface
	}
	slog.Info("Cast services LAN address", "ip", localIP.String())

	// 2. Scan for physical Chromecast devices
	discoverer := mdns.NewDiscoverer("", *castPort)
	ctx := context.Background()

	if *listOnly {
		slog.Info("Scanning LAN for Chromecast devices", "timeout", *scanTimeout)
		devices, err := discoverer.Discover(ctx, *scanTimeout)
		if err != nil {
			slog.Error("Discovery failed", "error", err)
			os.Exit(1)
		}
		if len(devices) == 0 {
			slog.Info("No Chromecast devices found on the network.")
			return
		}
		fmt.Printf("\nDiscovered %d Chromecast device(s):\n", len(devices))
		fmt.Printf("%-30s %-16s %-6s %-18s\n", "NAME", "IP", "PORT", "MODEL")
		fmt.Println(strings.Repeat("-", 75))
		for _, dev := range devices {
			fmt.Printf("%-30s %-16s %-6d %-18s\n", dev.FriendlyName, dev.IP.String(), dev.Port, dev.ModelName)
		}
		return
	}

	// Determine Target Physical Device
	var targetDevice *mdns.DiscoveredDevice
	if *tvIP != "" {
		ip := net.ParseIP(*tvIP)
		if ip == nil {
			slog.Error("Invalid -tv-ip", "ip", *tvIP)
			os.Exit(1)
		}
		targetDevice = &mdns.DiscoveredDevice{
			ID:           "manual-" + *tvIP,
			FriendlyName: fmt.Sprintf("Physical TV (%s)", *tvIP),
			ModelName:    "Chromecast",
			Capabilities: "264709",
			IP:           ip,
			Port:         *tvPort,
		}
		slog.Info("Using manually specified physical receiver", "ip", targetDevice.IP.String(), "port", targetDevice.Port)
	} else {
		slog.Info("Discovering physical Chromecast on LAN", "timeout", *scanTimeout)
		target, err := discoverer.FindTarget(ctx, "", *tvName, *scanTimeout)
		if err != nil {
			slog.Error("Target Chromecast discovery failed", "error", err, "tip", "Specify physical Chromecast IP directly with -tv-ip <ip>")
			os.Exit(1)
		}
		targetDevice = target
		slog.Info("Bound to physical receiver", "name", targetDevice.FriendlyName, "model", targetDevice.ModelName, "ip", targetDevice.IP.String(), "port", targetDevice.Port)
	}

	// 3. Start Local HTTP Streaming Proxy Server
	// The public base URL for snippets and HLS rewrites: use the configured host,
	// or fall back to the detected LAN IP when no host was given (':8888').
	baseHost := httpHost
	if baseHost == "" {
		baseHost = localIP.String()
	}
	baseURL := (&url.URL{Scheme: "http", Host: net.JoinHostPort(baseHost, httpPortStr)}).String()
	proxyServer := proxy.NewServer(baseURL)

	// The browser snippet works with loopback, but a physical TV cannot reach
	// localhost - casting would silently fail (icon shows, no media).
	if isLoopbackHost(httpHost) {
		slog.Warn("HTTP server is loopback-only: the browser snippet works, but the TV cannot reach this address",
			"addr", *httpAddr,
			"tip", "run with a LAN-reachable address, e.g. -http-addr 192.168.1.50:8888 or -http-addr :8888")
	}

	// 4. Create Direct Device Controller and wire it into the web server
	controller := castrelay.NewDeviceController(targetDevice, proxyServer)
	webServer := server.NewServer(proxyServer)
	webServer.SetController(controller)

	httpServer := &http.Server{
		Addr:         *httpAddr,
		Handler:      webServer.Handler(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // Streaming media requires indefinite write time
	}

	go func() {
		slog.Info("Local media proxy listening", "url", baseURL+"/proxy")
		slog.Info("Web Dashboard & Cast Controller", "url", baseURL+"/")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	// 5. Start Cast V2 TLS MITM Relay Server
	listenAddr := fmt.Sprintf(":%d", *castPort)
	relayServer, err := castrelay.NewRelayServer(listenAddr, localIP, targetDevice, proxyServer, *verbose)
	if err != nil {
		slog.Error("Failed to initialize Cast relay server", "error", err)
		os.Exit(1)
	}

	if err := relayServer.Start(); err != nil {
		slog.Error("Failed to start Cast TLS listener", "listenAddr", listenAddr, "error", err)
		os.Exit(1)
	}

	// 6. Start mDNS Virtual Chromecast Advertiser
	var caps string
	if targetDevice != nil {
		caps = targetDevice.Capabilities
	}
	advertiser, err := mdns.NewAdvertiser(*deviceName, *castPort, localIP, localIface, caps)
	if err != nil {
		slog.Error("Failed to start mDNS advertiser", "error", err)
		os.Exit(1)
	}
	defer advertiser.Shutdown()

	// 7. Start DIAL & SSDP Service on port 8008
	dialServer := dial.NewServer(*deviceName, advertiser.DeviceID(), localIP, *dialPort, func(appName, payload string) error {
		slog.Info("Launching app via Cast Controller", "appName", appName)
		return controller.CastURL(payload, "", "", "")
	})
	_ = dialServer.Start()
	defer dialServer.Close()

	slog.Info("Advertising virtual Cast device on mDNS", "name", *deviceName, "port", *castPort)
	fmt.Println("==========================================================================")
	fmt.Printf(" Web Dashboard & Remote Controller: %s\n", baseURL)
	fmt.Printf(" Local Media Proxy Relay:           %s/proxy\n", baseURL)
	fmt.Printf(" Cast V2 MITM Relay:                %s\n", listenAddr)
	fmt.Printf(" DIAL / SSDP Service:               http://%s:%d/ssdp/device-desc.xml\n", localIP.String(), *dialPort)
	fmt.Printf(" Physical TV Receiver Target:       %s (%s)\n", targetDevice.FriendlyName, targetDevice.IP.String())
	fmt.Printf(" Virtual Cast Device (mDNS):        %s\n", *deviceName)
	if *logFile != "" {
		fmt.Printf(" File Logging Enabled:              %s\n", *logFile)
	}
	fmt.Println("==========================================================================")

	// 8. Wait for Shutdown Signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	slog.Info("Shutting down origin-caster gracefully...")
	dialServer.Close()
	advertiser.Shutdown()
	controller.Close()
	_ = relayServer.Close()

	ctxShut, cancelShut := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShut()
	_ = httpServer.Shutdown(ctxShut)

	slog.Info("origin-caster stopped.")
}

// usage prints the help text shown by -h/--help and on flag errors.
// Flags are grouped by role, and each group starts with a plain-English
// explanation so new users do not need to know the protocols in advance.
func usage() {
	fmt.Fprintf(os.Stderr, `origin-caster - play browser video on your TV

What it does
  origin-caster runs a virtual Chromecast on your computer. Your browser
  casts to this virtual device, and origin-caster forwards the commands to
  a physical TV (Chromecast or Android TV). The TV plays the video through
  the local media proxy, because it cannot pass the media site's cookie and
  login checks by itself.

Ports
  8008  DIAL discovery service - TVs and apps use this to find the device
  8009  Cast V2 control (TLS)  - the browser casts to this port
  8888  Dashboard and proxy    - you open the dashboard here; the TV fetches video here

Flags
`)
	groups := []struct {
		title string
		names []string
	}{
		{"Virtual device (what the browser casts to)", []string{"device-name", "cast-port", "dial-port", "lan-ip"}},
		{"Web server (dashboard and media proxy)", []string{"http-addr"}},
		{"Physical TV (the device that plays the video)", []string{"tv-name", "tv-ip", "tv-port"}},
		{"Discovery and diagnostics", []string{"list", "scan-timeout"}},
		{"Logging", []string{"v", "verbose", "log-file"}},
	}
	for _, g := range groups {
		fmt.Fprintf(os.Stderr, "%s:\n", g.title)
		for _, name := range g.names {
			printFlagHelp(flag.Lookup(name))
		}
		fmt.Fprintln(os.Stderr)
	}
}

// printFlagHelp prints one flag in the standard "go help" style:
// name and type on the first line, description (and default) indented below.
func printFlagHelp(f *flag.Flag) {
	if f == nil {
		return
	}
	typ, usageText := flag.UnquoteUsage(f)
	line := fmt.Sprintf("  -%s", f.Name)
	if typ != "" {
		line += " " + typ
	}
	fmt.Fprintln(os.Stderr, line)
	desc := "    \t" + usageText
	if f.DefValue != "" && f.DefValue != "false" {
		desc += fmt.Sprintf(" (default %s)", f.DefValue)
	}
	fmt.Fprintln(os.Stderr, desc)
}

// isLoopbackHost reports whether host refers to the local machine only
// (localhost, 127.x, ::1). An empty host (bind all interfaces) is not.
func isLoopbackHost(host string) bool {
	switch {
	case host == "":
		return false
	case host == "localhost":
		return true
	case strings.HasPrefix(host, "127."):
		return true
	case host == "::1" || host == "[::1]":
		return true
	default:
		return false
	}
}
