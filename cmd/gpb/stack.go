package main

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/engine"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

const databaseFileName = "state.db"

// stdout is a variable so tests can read what a command printed.
var stdout io.Writer = os.Stdout

func openStore(cfg config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir(), 0o700); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(cfg.DataDir(), databaseFileName))
}

// withStore runs a command that touches only the database. Keeping it separate from the sync
// stack matters: opening that one starts a browser, and following an album should not.
func withStore(cfg config.Config, do func(*store.Store) error) error {
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	return do(db)
}

type syncStack struct {
	store  *store.Store
	syncer *syncer.Syncer
}

func openSyncStack(ctx context.Context, cfg config.Config, options syncer.Options) (*syncStack, error) {
	db, err := openStore(cfg)
	if err != nil {
		return nil, err
	}

	engine, err := engine.Connect(ctx, auth.NewManager(cfg.ProfileDir()), db, options)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &syncStack{store: db, syncer: engine}, nil
}

func (s *syncStack) Close() error { return s.store.Close() }
