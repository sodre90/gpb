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

// runDuplicates deals with the two ways one photograph ends up as two files. A written-off copy
// of a photo still backed up is listed, and with --delete removed — the only command that deletes
// anything, which is why it lists by default. A photo held under two live keys, which Google hands
// out for an album and the timeline alike, is counted, and with --link made one file with two
// names. Like verify it touches the database and the pool alone, so it can run while the daemon
// is up.
func runDuplicates(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("duplicates", flag.ExitOnError)
	remove := flags.Bool("delete", false, "remove the written-off copies rather than list them")
	link := flags.Bool("link", false, "make each photo held under several keys one file, hardlinked, rather than count them")
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

		linking, err := syncer.LinkCopies(ctx, db, *link)
		if err != nil {
			return err
		}
		for _, refusal := range linking.Refusals {
			fmt.Fprintln(stdout, "left alone: "+refusal)
		}
		fmt.Fprintln(stdout, linking)

		if cleanup.Refused > 0 || len(linking.Refusals) > 0 {
			return errors.New("a copy could not be verified, so it was left as it was")
		}
		return nil
	})
}
