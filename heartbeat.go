package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func heartbeatPath(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "heartbeat.last")
}

// reportBootGap logs how long the previous instance was offline (clean stop vs
// crash) before any DB is opened, so it's the first record of the boot.
func reportBootGap(cfg Config) {
	data, err := os.ReadFile(heartbeatPath(cfg))
	if err != nil {
		slog.Info("first boot (no prior heartbeat)")
		return
	}
	ns, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || ns == 0 {
		slog.Info("clean prior shutdown")
		return
	}
	gap := time.Since(time.Unix(0, ns))
	if gap > 2*time.Minute {
		slog.Warn("long downtime since last heartbeat (likely crash/restart)", "gap", gap.String())
	} else {
		slog.Info("restarted", "gap", gap.String())
	}
}

// heartbeatLoop writes a Unix-nano timestamp every interval; clean shutdown
// writes "0" so the next boot's reportBootGap can distinguish stop vs crash.
func heartbeatLoop(ctx context.Context, cfg Config, interval time.Duration) {
	path := heartbeatPath(cfg)
	write := func(v string) { _ = os.WriteFile(path, []byte(v), 0o600) }
	t := time.NewTicker(interval)
	defer t.Stop()
	write(strconv.FormatInt(time.Now().UnixNano(), 10))
	for {
		select {
		case <-ctx.Done():
			write("0")
			return
		case <-t.C:
			write(strconv.FormatInt(time.Now().UnixNano(), 10))
		}
	}
}
