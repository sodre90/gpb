package main

import (
	"flag"
	"fmt"

	"gpb/internal/config"
	"gpb/internal/links"
	"gpb/internal/store"
)

func runLinks(args []string) error {
	flags := flag.NewFlagSet("links", flag.ExitOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	return withStore(cfg, func(db *store.Store) error {
		report, err := links.Rebuild(db, cfg.PhotosDir)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", report)
		return nil
	})
}

// relink keeps the album view in step with a change the user just made. It never fails the
// operation that triggered it: the backup and the database are the product, and a stale
// convenience tree is a smaller problem than an apparently failed command.
func relink(cfg config.Config, db *store.Store) {
	if _, err := links.Rebuild(db, cfg.PhotosDir); err != nil {
		fmt.Fprintf(stdout, "warning: the album view could not be rebuilt: %v\n", err)
	}
}
