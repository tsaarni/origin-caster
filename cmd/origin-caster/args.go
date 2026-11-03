package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"text/tabwriter"
	"time"
)

type config struct {
	httpAddr, lanIP, tvName, tvIP, logFile string
	tvPort                                 int
	scanTimeout                            time.Duration
	listOnly, verbose                      bool
}

// parseArgs has no startup side effects; the caller handles help and errors.
func parseArgs(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("origin-caster", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&cfg.httpAddr, "http-addr", ":8888", "Dashboard and proxy address")
	fs.StringVar(&cfg.lanIP, "lan-ip", "", "Advertised LAN IP")
	fs.StringVar(&cfg.tvName, "tv-name", "", "Device name substring")
	fs.StringVar(&cfg.tvIP, "tv-ip", "", "Device IP")
	fs.IntVar(&cfg.tvPort, "tv-port", 8009, "Cast port")
	fs.DurationVar(&cfg.scanTimeout, "scan-timeout", 3*time.Second, "Discovery timeout")
	fs.BoolVar(&cfg.listOnly, "list", false, "List devices")
	fs.BoolVar(&cfg.verbose, "v", false, "Verbose logging")
	fs.BoolVar(&cfg.verbose, "verbose", false, "Verbose logging")
	fs.StringVar(&cfg.logFile, "log-file", "", "Log file")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if cfg.scanTimeout <= 0 {
		return cfg, fmt.Errorf("--scan-timeout must be positive")
	}
	if cfg.tvIP != "" && cfg.tvName != "" {
		return cfg, fmt.Errorf("--tv-ip and --tv-name cannot be used together")
	}
	explicitlySet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicitlySet[f.Name] = true })
	if explicitlySet["tv-port"] && cfg.tvIP == "" {
		return cfg, fmt.Errorf("--tv-port requires --tv-ip")
	}
	if cfg.tvPort < 1 || cfg.tvPort > 65535 {
		return cfg, fmt.Errorf("--tv-port must be between 1 and 65535")
	}
	for _, value := range []struct{ name, value string }{{"tv-ip", cfg.tvIP}, {"lan-ip", cfg.lanIP}} {
		if value.value != "" && net.ParseIP(value.value) == nil {
			return cfg, fmt.Errorf("--%s: invalid IP address %q", value.name, value.value)
		}
	}
	_, port, err := net.SplitHostPort(cfg.httpAddr)
	if err != nil {
		return cfg, fmt.Errorf("--http-addr: expected host:port (for example :8888 or 192.168.1.50:8888)")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return cfg, fmt.Errorf("--http-addr: port must be between 1 and 65535")
	}
	return cfg, nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `origin-caster — play browser video on your TV

Usage:
  origin-caster [options]

Examples:
  origin-caster --tv-name "Living Room"
  origin-caster --tv-ip 192.168.1.42
  origin-caster --list

`)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprint(tw, `Select a TV:
  --tv-name NAME	Match device name (case-insensitive substring)
  --tv-ip IP	Connect directly, skipping discovery
  --tv-port PORT	Cast port for --tv-ip (default: 8009)
  --list	List discovered devices and exit
  --scan-timeout TIME	Discovery timeout (default: 3s)

Web server:
  --http-addr HOST:PORT	Dashboard and proxy address (default: :8888)
  --lan-ip IP	Advertised LAN IP when --http-addr has no host (default: auto-detect)

Logging:
  -v, --verbose	Enable protocol logging
  --log-file PATH	Also write logs to a file

  -h, --help	Show help
`)
	tw.Flush()
	fmt.Fprint(w, `
With no TV selection, the first discovered device is used.
Open the dashboard on port 8888 by default to get started.
Single-dash options (such as -tv-name) are also accepted.
`)
}
