package main

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestConsoleGroupsAndConcurrentChildren(t *testing.T) {
	var buf bytes.Buffer
	root := slog.New(newConsoleHandler(&buf, slog.LevelDebug))
	logger := root.With("root", "value").WithGroup("outer").With("bound", 1).WithGroup("inner")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { logger.Info("event", slog.Group("nested", "key", "value")) })
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 20 {
		t.Fatalf("got %d records", len(lines))
	}
	for _, line := range lines {
		for _, want := range []string{"root=value", "outer.bound=1", "outer.inner.nested.key=value"} {
			if !strings.Contains(line, want) {
				t.Errorf("missing %q in %q", want, line)
			}
		}
	}
}
