package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"positional", []string{"stray", "--verbose"}, "unexpected argument"},
		{"after separator", []string{"--", "stray"}, "unexpected argument"},
		{"unknown", []string{"--typo"}, "flag provided but not defined"},
		{"missing value", []string{"--tv-ip"}, "flag needs an argument"},
		{"bad duration", []string{"--scan-timeout=soon"}, "invalid value"},
		{"zero timeout", []string{"--scan-timeout=0"}, "must be positive"},
		{"negative timeout", []string{"--scan-timeout=-1s"}, "must be positive"},
		{"conflict", []string{"--tv-ip=192.168.1.42", "--tv-name=TV"}, "cannot be used together"},
		{"port without IP", []string{"--tv-port=8009"}, "requires --tv-ip"},
		{"invalid IP", []string{"--tv-ip=invalid"}, "invalid IP address"},
		{"invalid LAN IP", []string{"--lan-ip=invalid"}, "invalid IP address"},
		{"invalid address", []string{"--http-addr=8888"}, "expected host:port"},
	}
	for _, port := range []string{"0", "-1", "65536", "abc"} {
		tests = append(tests, struct {
			name string
			args []string
			want string
		}{"http port " + port, []string{"--http-addr=:" + port}, "port must be between"})
	}
	for _, port := range []string{"0", "-1", "65536"} {
		tests = append(tests, struct {
			name string
			args []string
			want string
		}{"tv port " + port, []string{"--tv-ip=192.168.1.42", "--tv-port=" + port}, "must be between"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArgs(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want %q", err, tt.want)
			}
		})
	}
}

func TestParseArgsDefaultsAndSyntax(t *testing.T) {
	cfg, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.httpAddr != ":8888" || cfg.tvPort != 8009 || cfg.scanTimeout != 3*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	for _, args := range [][]string{
		{"-tv-name", "Living Room", "-v"},
		{"--tv-name=Living Room", "--verbose"},
	} {
		cfg, err := parseArgs(args)
		if err != nil || cfg.tvName != "Living Room" || !cfg.verbose {
			t.Fatalf("parse %v = %+v, %v", args, cfg, err)
		}
	}
	cfg, err = parseArgs([]string{"--tv-ip=::1", "--tv-port=65535", "--http-addr=[::]:1", "--lan-ip=::1"})
	if err != nil || cfg.tvPort != 65535 {
		t.Fatalf("IPv6 and boundary ports: %+v, %v", cfg, err)
	}
	// Each call must start with fresh defaults.
	cfg, err = parseArgs(nil)
	if err != nil || cfg.verbose || cfg.tvName != "" {
		t.Fatalf("state leaked between calls: %+v, %v", cfg, err)
	}
}

func TestParseArgsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		_, err := parseArgs([]string{arg})
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s: got %v", arg, err)
		}
	}
}
