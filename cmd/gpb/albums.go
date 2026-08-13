package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"gpb/internal/config"
	"gpb/internal/engine"
	"gpb/internal/store"
)

// albumIDDisplayLength keeps the listing readable. The full key is 50-odd characters of
// base64, which turns a table into a wall; the other commands accept any unambiguous prefix,
// so a truncated id is still usable as typed.
const albumIDDisplayLength = 12

func runAlbums(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("albums", flag.ExitOnError)
	local := flags.Bool("local", false, "list what the database already knows, without asking Google")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	if *local {
		return withStore(cfg, printKnownAlbums)
	}
	return refreshAndPrintAlbums(ctx, cfg)
}

func refreshAndPrintAlbums(ctx context.Context, cfg config.Config) error {
	stack, err := openSyncStack(ctx, cfg, engine.Options(cfg))
	if err != nil {
		return err
	}
	defer stack.Close()

	if err := stack.syncer.RefreshAlbums(ctx); err != nil {
		return fmt.Errorf("refreshing the album list: %w", err)
	}
	return printKnownAlbums(stack.store)
}

func printKnownAlbums(db *store.Store) error {
	albums, err := db.Albums()
	if err != nil {
		return err
	}
	if len(albums) == 0 {
		fmt.Fprintln(stdout, "No albums known yet. Run `gpb albums` without --local to fetch them.")
		return nil
	}

	printAlbums(albums)
	return nil
}

func printAlbums(albums []store.Album) {
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tMODE\tITEMS\tTITLE")

	followed := 0
	for _, album := range albums {
		if album.SyncMode != store.SyncNone {
			followed++
		}
		fmt.Fprintf(table, "%s\t%s\t%d\t%s\n",
			shortID(album.ID), album.SyncMode, album.ItemCount, titleOf(album))
	}
	table.Flush()

	fmt.Fprintf(stdout, "\n%d albums, %d followed\n", len(albums), followed)
}

func shortID(id string) string {
	if len(id) <= albumIDDisplayLength {
		return id
	}
	return id[:albumIDDisplayLength] + "…"
}

// titleOf names the untitled. Albums with no title are real — Google lets you create them —
// and printing an empty cell makes the row look broken rather than merely nameless.
func titleOf(album store.Album) string {
	if album.Title == "" {
		return "(untitled)"
	}
	return album.Title
}

func runFollow(args []string) error {
	flags := flag.NewFlagSet("follow", flag.ExitOnError)
	mode := flags.String("mode", string(store.SyncAll), "all: every item; picked: only items you select")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return setSyncMode(flags.Arg(0), store.SyncMode(*mode))
}

func runUnfollow(args []string) error {
	flags := flag.NewFlagSet("unfollow", flag.ExitOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	return setSyncMode(flags.Arg(0), store.SyncNone)
}

func setSyncMode(prefix string, mode store.SyncMode) error {
	if prefix == "" {
		return fmt.Errorf("give an album id (see `gpb albums`)")
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	return withStore(cfg, func(db *store.Store) error {
		album, err := resolveAlbum(db, prefix)
		if err != nil {
			return err
		}
		if err := db.SetAlbumSyncMode(album.ID, mode); err != nil {
			return err
		}
		relink(cfg, db)

		fmt.Fprintf(stdout, "%s: %s\n", titleOf(album), describeMode(mode))
		return nil
	})
}

func describeMode(mode store.SyncMode) string {
	switch mode {
	case store.SyncAll:
		return "now syncing all items"
	case store.SyncPicked:
		return "now syncing selected items only"
	default:
		return "no longer syncing"
	}
}

// resolveAlbum accepts any unambiguous prefix of an album id, because the full key is far
// too long to retype from a listing. An ambiguous prefix is refused rather than guessed:
// silently following the wrong album would be discovered only by the download traffic.
func resolveAlbum(db *store.Store, prefix string) (store.Album, error) {
	albums, err := db.Albums()
	if err != nil {
		return store.Album{}, err
	}

	prefix = strings.TrimSuffix(prefix, "…")
	var matches []store.Album
	for _, album := range albums {
		if strings.HasPrefix(album.ID, prefix) {
			matches = append(matches, album)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return store.Album{}, fmt.Errorf("no album id starts with %q — run `gpb albums` first", prefix)
	default:
		return store.Album{}, fmt.Errorf("%q matches %d albums; use more characters", prefix, len(matches))
	}
}
