package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"gpb/internal/config"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

// runVerify exits non-zero when the sweep found something wrong, so a cron entry that runs it
// monthly needs no output parsing to notice. It touches only the database and the pool — no
// browser, no Google session — so it can run while the daemon is up.
func runVerify(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	repair := flags.Bool("repair", false, "queue the damaged files to be downloaded again")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	return withStore(cfg, func(db *store.Store) error {
		report, err := syncer.Verify(ctx, db, *repair)
		if err != nil {
			return err
		}

		for _, problem := range report.Problems {
			fmt.Fprintln(stdout, problem)
		}
		fmt.Fprintln(stdout, report)

		if len(report.Problems) > 0 {
			return errors.New("the backup is not what it was when it was made")
		}
		return nil
	})
}
