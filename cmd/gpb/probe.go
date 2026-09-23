package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/gphotos"
	"gpb/internal/store"
)

// runProbe asks Google for one item exactly as a run would and says what came back, without
// touching the pool or the database. It exists for questions a backup cannot answer on its own
// behalf: whether a motion photo arrives with its video, what an edited photo downloads as, how
// long a minted URL stays good. The signed URL is never printed — it is a credential for that
// file — only the host it names.
func runProbe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("probe", flag.ExitOnError)
	againAfter := flags.Duration("again-after", 0, "fetch the same signed URL again after this long, to see whether it still works")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("usage: gpb probe [--again-after 30m] <media key or pool file name>...")
	}

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	return withStore(cfg, func(db *store.Store) error {
		items, err := itemsNamed(db, flags.Args())
		if err != nil {
			return err
		}

		session, err := auth.NewManager(cfg.ProfileDir()).Warmup(ctx)
		if err != nil {
			return err
		}
		client, err := gphotos.NewClient(session)
		if err != nil {
			return err
		}

		prober := prober{client: client, db: db, againAfter: *againAfter}
		for _, item := range items {
			if err := prober.probe(ctx, item); err != nil {
				fmt.Fprintf(stdout, "  failed: %v\n", err)
			}
			fmt.Fprintln(stdout)
		}
		return nil
	})
}

// itemsNamed accepts either handle a person has: the media key the web UI and the logs use, or
// the name of the file in the pool.
func itemsNamed(db *store.Store, names []string) ([]store.MediaItem, error) {
	items := make([]store.MediaItem, 0, len(names))
	for _, name := range names {
		item, err := db.Item(name)
		if err != nil {
			item, err = db.ItemStoredAs(name)
		}
		if err != nil {
			return nil, fmt.Errorf("%s is neither a media key nor a file in the pool", name)
		}
		items = append(items, item)
	}
	return items, nil
}

type prober struct {
	client     *gphotos.Client
	db         *store.Store
	againAfter time.Duration
}

func (p prober) probe(ctx context.Context, item store.MediaItem) error {
	fmt.Fprintf(stdout, "%s  %s\n", item.MediaKey, item.Filename)
	fmt.Fprintf(stdout, "  backed up    %s\n", describeBackup(item))

	signed, err := p.client.DownloadURL(ctx, item.MediaKey, p.permissionAlbum(item))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  minted on    %s\n", hostOf(signed))

	body, err := p.fetchAndInspect(ctx, signed)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  served       %s\n", body.served)
	fmt.Fprintf(stdout, "  body         %s\n", describeBody(body.Body))
	for _, entry := range body.ZipEntries {
		fmt.Fprintf(stdout, "               %s (%d bytes)\n", entry.Name, entry.Size)
	}
	fmt.Fprintf(stdout, "  compared     %s\n", compareWithBackup(item, body.Body))

	if p.againAfter > 0 {
		fmt.Fprintf(stdout, "  waiting %s to fetch the same URL again\n", p.againAfter)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.againAfter):
		}
		_, err := p.fetchAndInspect(ctx, signed)
		fmt.Fprintf(stdout, "  again after %s: %s\n", p.againAfter, verdictOf(err))
	}
	return nil
}

// permissionAlbum names an album the item is followed through, because Google checks the item
// against it; an item reached only through the library timeline has none, and a null album is
// what mints for those.
func (p prober) permissionAlbum(item store.MediaItem) string {
	albumID, err := p.db.AlbumForItem(item.MediaKey)
	if err != nil || albumID == store.LibraryID {
		return ""
	}
	return albumID
}

type inspected struct {
	Body
	served string
}

func (p prober) fetchAndInspect(ctx context.Context, signed string) (inspected, error) {
	// Not the pool's staging directory: a run tidies that of every partial no item owns, and
	// would delete this one from under the inspection.
	scratch, err := os.CreateTemp("", "gpb-probe-*")
	if err != nil {
		return inspected{}, err
	}
	defer os.Remove(scratch.Name())
	defer scratch.Close()

	download, err := p.client.Fetch(ctx, signed, 0, scratch)
	if err != nil {
		return inspected{}, err
	}
	body, err := inspectFile(scratch.Name())
	if err != nil {
		return inspected{}, err
	}
	served := fmt.Sprintf("%s, %d bytes, named %q", orUnknown(download.ContentType), download.Size, download.Filename)
	return inspected{Body: body, served: served}, nil
}

func describeBackup(item store.MediaItem) string {
	if item.State != store.StateDone {
		return "not yet (" + string(item.State) + ")"
	}
	return fmt.Sprintf("%d bytes at %s", item.SizeBytes, item.LocalPath)
}

func describeBody(body Body) string {
	parts := []string{body.Kind}
	switch {
	case body.DeclaresMotion && body.CarriesVideo:
		parts = append(parts, "a motion photo carrying its video")
	case body.DeclaresMotion && body.DeclaredVideoLength > 0:
		parts = append(parts, fmt.Sprintf("says it is a motion photo with a %d-byte video, and carries none", body.DeclaredVideoLength))
	case body.DeclaresMotion:
		parts = append(parts, "says it is a motion photo, and carries no video")
	case body.CarriesVideo:
		parts = append(parts, "carries an embedded video it does not declare")
	}
	return strings.Join(parts, ", ")
}

func compareWithBackup(item store.MediaItem, body Body) string {
	switch {
	case item.SHA256 == "":
		return "nothing recorded to compare with"
	case strings.EqualFold(item.SHA256, body.SHA256):
		return "the same bytes as the backup"
	default:
		return fmt.Sprintf("different bytes from the backup: %d served against %d kept", body.Size, item.SizeBytes)
	}
}

func verdictOf(err error) string {
	if err == nil {
		return "still served"
	}
	return "refused: " + err.Error()
}

func hostOf(signed string) string {
	parsed, err := url.Parse(signed)
	if err != nil {
		return "an address that does not parse"
	}
	return parsed.Host
}

func orUnknown(text string) string {
	if text == "" {
		return "no content type"
	}
	return text
}
