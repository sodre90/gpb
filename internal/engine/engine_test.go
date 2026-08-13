package engine

import (
	"testing"

	"gpb/internal/config"
	"gpb/internal/syncer"
)

// Options is the only place the file's limits become the run's limits, and nothing else would
// notice if a setting stopped being copied: the daemon would go on running happily on the
// defaults, and the settings page would go on showing a number that meant nothing.
func TestTheConfiguredLimitsReachTheSyncer(t *testing.T) {
	cfg := config.Defaults()
	cfg.Limits.DownloadWorkers = 7
	cfg.Limits.RequestsPerSecond = 4.5
	cfg.Limits.MinFreeBytes = 50 << 30

	options := Options(cfg)
	if options.Workers != 7 {
		t.Errorf("the syncer runs %d workers, want the configured 7", options.Workers)
	}
	if options.RequestsPerSecond != 4.5 {
		t.Errorf("the syncer asks %v times a second, want the configured 4.5", options.RequestsPerSecond)
	}
	if options.MinFreeBytes != 50<<30 {
		t.Errorf("the syncer keeps %d bytes free, want the configured %d", options.MinFreeBytes, 50<<30)
	}
}

// The free-space floor is named in two places: config writes it into config.toml, and the syncer
// falls back to it when nothing configures one. Configuration must not import the sync engine to
// borrow the number, so they hold it separately — and this is where both are in scope to check
// that they still agree.
func TestTheConfiguredFloorAndTheSyncersOwnFallbackAgree(t *testing.T) {
	if written, enforced := config.Defaults().Limits.MinFreeBytes, int64(syncer.DiskFloor); written != enforced {
		t.Errorf("config defaults the floor to %d bytes and the syncer to %d", written, enforced)
	}
}

// Zero is the off switch, and it is the one value a copy that only takes non-empty settings would
// silently drop — leaving the default floor in place under someone who turned it off.
func TestAFloorTurnedOffInTheFileReachesTheSyncerAsOff(t *testing.T) {
	cfg := config.Defaults()
	cfg.Limits.MinFreeBytes = 0

	if floor := Options(cfg).MinFreeBytes; floor != 0 {
		t.Errorf("the syncer keeps %d bytes free despite the check being turned off", floor)
	}
}
