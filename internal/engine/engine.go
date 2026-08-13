// Package engine assembles a sync run from configuration: it warms the browser profile,
// turns the harvested session into an HTTP client, and wires that client to a syncer. It
// exists because both the CLI and the daemon need exactly this assembly, and the pieces are
// interdependent enough that duplicating the wiring would eventually let them drift apart.
package engine

import (
	"context"
	"path/filepath"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/gphotos"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

const (
	stagingDirName = ".tmp"
	runLockName    = "sync.lock"
)

func Options(cfg config.Config) syncer.Options {
	options := syncer.DefaultOptions(
		cfg.PoolDir(),
		filepath.Join(cfg.PhotosDir, stagingDirName),
	)
	options.Workers = cfg.Limits.DownloadWorkers
	options.RequestsPerSecond = cfg.Limits.RequestsPerSecond
	options.MinFreeBytes = cfg.Limits.MinFreeBytes
	return options
}

// Connect warms the profile and returns a syncer ready to run. The warmup is the only part
// of gpb that needs Chrome; everything past it is plain HTTP carrying the cookies the
// browser just rotated.
//
// The client draws from the syncer's own token bucket, which is the point of assembling the
// two together: separately throttled, listing and downloading would each get a full rate
// budget and the account would see twice the traffic the config asked for.
//
// The caller supplies the manager rather than a profile path so a run inside the daemon
// refreshes the same session the status page reports, instead of quietly rotating cookies
// behind a second manager's back.
func Connect(ctx context.Context, manager *auth.Manager, db *store.Store, options syncer.Options) (*syncer.Syncer, error) {
	session, err := manager.Warmup(ctx)
	if err != nil {
		return nil, err
	}

	client, err := gphotos.NewClient(session)
	if err != nil {
		return nil, err
	}

	engine := syncer.New(client, db, options)
	client.Throttle(engine.Limiter())
	return engine, nil
}

// LockRun serialises whole runs across processes: the daemon's own guard cannot see
// `podman exec gpb gpb sync`, and two runs sharing one pool would race on the same .part
// files and double the request rate.
func LockRun(cfg config.Config) (release func(), err error) {
	return syncer.AcquireRunLock(filepath.Join(cfg.DataDir(), runLockName))
}
