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

// runDuplicates lists the written-off files whose photograph is still backed up under another
// key, and with --delete removes them. It is the only command that deletes anything, which is
// why it lists by default and acts only when asked. Like verify it touches the database and
// the pool alone, so it can run while the daemon is up.
func runDuplicates(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("duplicates", flag.ExitOnError)
	remove := flags.Bool("delete", false, "remove the written-off copies rather than list them")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	return withStore(cfg, func(db *store.Store) error {
		cleanup, err := syncer.RemoveTwins(ctx, db, *remove)
		if err != nil {
			return err
		}

		for _, removal := range cleanup.Removals {
			fmt.Fprintln(stdout, removal)
		}
		fmt.Fprintln(stdout, cleanup)

		if cleanup.Refused > 0 {
			return errors.New("a kept copy could not be verified, so its twin was left alone")
		}
		return nil
	})
}
