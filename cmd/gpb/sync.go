package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"gpb/internal/config"
	"gpb/internal/engine"
	"gpb/internal/syncer"
)

func runSync(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sync", flag.ExitOnError)
	limit := flags.Int("limit", 0, "stop after this many items (0 means no limit)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	unlock, err := engine.LockRun(cfg)
	if err != nil {
		return err
	}
	defer unlock()

	options := engine.Options(cfg)
	if *limit > 0 {
		options.MaxItemsPerRun = *limit
	}

	stack, err := openSyncStack(ctx, cfg, options)
	if err != nil {
		return err
	}
	defer stack.Close()

	startedAt := time.Now()
	report, err := stack.syncer.Run(ctx)
	relink(cfg, stack.store)
	printReport(report, time.Since(startedAt))
	return err
}

func printReport(report syncer.Report, elapsed time.Duration) {
	fmt.Fprintf(stdout, "%s: %d listed, %d downloaded, %d failed, %d skipped, %s in %s\n",
		report.Outcome, report.Listed, report.Downloaded, report.Failed, report.Skipped,
		humanBytes(report.Bytes), elapsed.Round(time.Second))
}

func humanBytes(bytes int64) string {
	const unit = 1000
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	value, exponent := float64(bytes)/unit, 0
	for value >= unit && exponent < 3 {
		value /= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %cB", value, "kMGT"[exponent])
}
