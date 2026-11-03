package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tsaarni/origin-caster/internal/controller"
	"github.com/tsaarni/origin-caster/internal/mdns"
	"github.com/tsaarni/origin-caster/internal/netutil"
	"github.com/tsaarni/origin-caster/internal/proxy"
	"github.com/tsaarni/origin-caster/internal/server"
)

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		usage(os.Stdout)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "origin-caster: %v\nRun with --help for usage.\n", err)
		os.Exit(2)
	}

	logHandle, err := initLogger(cfg.logFile, cfg.verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing logger: %v\n", err)
		os.Exit(1)
	}
	if logHandle != nil {
		defer logHandle.Close()
	}

	// parseArgs has already validated the address.
	httpHost, httpPortStr, _ := net.SplitHostPort(cfg.httpAddr)

	// Scan for physical Chromecast devices
	discoverer := mdns.NewDiscoverer()
	ctx := context.Background()

	if cfg.listOnly {
		slog.Info("Scanning LAN for Chromecast devices", "timeout", cfg.scanTimeout)
		devices, err := discoverer.Discover(ctx, cfg.scanTimeout)
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

	// Determine the physical device to control
	var targetDevice *mdns.DiscoveredDevice
	if cfg.tvIP != "" {
		ip := net.ParseIP(cfg.tvIP)
		if ip == nil {
			slog.Error("Invalid -tv-ip", "ip", cfg.tvIP)
			os.Exit(1)
		}
		targetDevice = &mdns.DiscoveredDevice{
			ID:           "manual-" + cfg.tvIP,
			FriendlyName: fmt.Sprintf("Physical TV (%s)", cfg.tvIP),
			ModelName:    "Chromecast",
			IP:           ip,
			Port:         cfg.tvPort,
		}
		slog.Info("Using manually specified physical receiver", "ip", targetDevice.IP.String(), "port", targetDevice.Port)
	} else {
		slog.Info("Discovering physical Chromecast on LAN", "timeout", cfg.scanTimeout)
		target, err := discoverer.FindTarget(ctx, cfg.tvName, cfg.scanTimeout)
		if err != nil {
			slog.Error("Target Chromecast discovery failed", "error", err, "tip", "Specify physical Chromecast IP directly with -tv-ip <ip>")
			os.Exit(1)
		}
		targetDevice = target
		slog.Info("Selected physical receiver", "name", targetDevice.FriendlyName, "model", targetDevice.ModelName, "ip", targetDevice.IP.String(), "port", targetDevice.Port)
	}

	// Detect the LAN IP used in the proxy URL the TV is told to fetch.
	var localIP net.IP
	if cfg.lanIP != "" {
		localIP = net.ParseIP(cfg.lanIP)
		if localIP == nil {
			slog.Error("Invalid IP specified in -lan-ip", "ip", cfg.lanIP)
			os.Exit(1)
		}
	} else {
		localIP, err = netutil.DetectIPForTarget(targetDevice.IP)
		if err != nil {
			slog.Error("Failed to detect LAN address for target TV", "target", targetDevice.IP.String(), "error", err)
			os.Exit(1)
		}
	}
	slog.Info("LAN address", "ip", localIP.String())

	// Build the base URL advertised to the TV: use the configured host, or
	// fall back to the detected LAN IP when no host was given (':8888').
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
			"addr", cfg.httpAddr,
			"tip", "run with a LAN-reachable address, e.g. -http-addr 192.168.1.50:8888 or -http-addr :8888")
	}

	// Wire the device controller into the web server
	ctrl := controller.NewDeviceController(targetDevice, proxyServer)
	webServer := server.NewServer(proxyServer)
	webServer.SetController(ctrl)

	httpServer := &http.Server{
		Addr:         cfg.httpAddr,
		Handler:      webServer.Handler(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // Streaming media requires indefinite write time
	}

	fmt.Println("\norigin-caster")
	fmt.Printf("  %-11s %s\n", "Dashboard", baseURL+"/")
	fmt.Printf("  %-11s %s/proxy\n", "Media proxy", baseURL)
	fmt.Printf("  %-11s %s (%s)\n", "TV", targetDevice.FriendlyName, targetDevice.IP.String())
	if cfg.logFile != "" {
		fmt.Printf("  %-11s %s\n", "Log file", cfg.logFile)
	}
	fmt.Println("\nPress Ctrl+C to stop.")
	fmt.Println()

	go func() {
		slog.Debug("Local media proxy listening", "url", baseURL+"/proxy")
		slog.Debug("Web Dashboard & Cast Controller", "url", baseURL+"/")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for Shutdown Signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	slog.Info("Shutting down origin-caster gracefully...")
	ctrl.Close()

	ctxShut, cancelShut := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShut()
	_ = httpServer.Shutdown(ctxShut)

	slog.Info("origin-caster stopped.")
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
