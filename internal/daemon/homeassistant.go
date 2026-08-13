package daemon

import (
	"cmp"
	"fmt"
	"log"
	"math"
	"time"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/homeassistant"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

// mqttSettings is re-read from the file, the way the backup time is, so that a broker named on
// the settings page is dialled by the bridge's next attempt rather than at the next restart.
func (d *Daemon) mqttSettings() config.MQTT {
	cfg, err := config.Load(d.cfg.DataDir())
	if err != nil {
		log.Printf("home assistant: re-reading the config: %v", err)
		return d.cfg.MQTT
	}
	return cfg.MQTT
}

// Snapshot is the whole of what Home Assistant is told, gathered fresh each time. The counts come
// from the store rather than from the run in flight, because a dashboard is asking how the backup
// stands rather than what this particular pass has managed.
func (d *Daemon) Snapshot() homeassistant.State {
	activity := d.runner.Activity()
	state := homeassistant.State{
		Activity:       cmp.Or(activity, "idle"),
		Running:        activity != "",
		SessionProblem: sessionIsAProblem(d.auth.Status().State),
		Library:        libraryChoice(d.libraryMode()),
	}

	if set, err := d.store.BackupSet(); err != nil {
		log.Printf("home assistant: reading the backup set: %v", err)
	} else {
		state.BackedUp, state.Remaining, state.Failed = set.Done, set.Pending, set.Failed
		state.Progress = percentOf(set.Done, set.Known)
	}
	if free, err := syncer.FreeBytes(d.cfg.PhotosDir); err != nil {
		log.Printf("home assistant: measuring free space: %v", err)
	} else {
		state.FreeGB = math.Round(float64(free)/1e9*10) / 10
	}

	state.Downloading, state.Album = d.whatIsDownloading()
	state.LastRunAt, state.LastOutcome = d.howTheLastRunEnded()
	return state
}

// sessionIsAProblem covers both ways the Google half can be unwell: signed out, and unable to
// tell. A dashboard that reports "fine" while the daemon has not managed a check in a week is
// worse than one that reports nothing.
func sessionIsAProblem(state auth.State) bool {
	return state == auth.StateAuthRequired || state == auth.StateWarmupFailed
}

func (d *Daemon) whatIsDownloading() (filename, album string) {
	items := d.runner.Progress().Items
	if len(items) == 0 {
		return "", ""
	}

	first := items[0]
	return first.Filename, d.albumTitle(first.AlbumID)
}

func (d *Daemon) albumTitle(albumID string) string {
	if albumID == "" {
		return ""
	}
	if albumID == store.LibraryID {
		return "Whole library"
	}

	album, err := d.store.Album(albumID)
	if err != nil {
		return ""
	}
	return album.Title
}

func (d *Daemon) howTheLastRunEnded() (at, outcome string) {
	runs, err := d.store.RecentRuns(1)
	if err != nil || len(runs) == 0 {
		return "", ""
	}

	finished := runs[0].FinishedAt
	if finished.IsZero() {
		return "", string(runs[0].Outcome)
	}
	return finished.UTC().Format(time.RFC3339), string(runs[0].Outcome)
}

func (d *Daemon) libraryMode() store.SyncMode {
	library, err := d.store.Library()
	if err != nil {
		return store.SyncNone
	}
	return library.SyncMode
}

// libraryChoice turns the store's word into the one the selector offers, because a select entity
// whose state is not one of its own options shows as unknown in Home Assistant.
func libraryChoice(mode store.SyncMode) string {
	if mode == store.SyncAll {
		return "Everything"
	}
	return "No"
}

func percentOf(done, known int) int {
	if known <= 0 {
		return 0
	}
	return done * 100 / known
}

// StartSync, RefreshAlbums, SetLibraryMode and SetAlbumMode are what Home Assistant may ask for.
// They go through the same runner and store the web UI's own buttons use, so a run asked for from
// a dashboard is refused by the same one-run-at-a-time rule as one asked for from the page.
func (d *Daemon) StartSync(reason string) error {
	return d.runner.StartSync(reason)
}

func (d *Daemon) RefreshAlbums(reason string) error {
	return d.runner.StartRefresh(reason)
}

func (d *Daemon) SetLibraryMode(mode string) error {
	library, err := d.store.Library()
	if err != nil {
		return fmt.Errorf("reading the library setting: %w", err)
	}
	return d.store.SetLibrary(store.SyncMode(mode), library.Since)
}

func (d *Daemon) SetAlbumMode(albumID, mode string) error {
	return d.store.SetAlbumSyncMode(albumID, store.SyncMode(mode))
}
