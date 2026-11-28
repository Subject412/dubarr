package watch

import (
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

// TestDefaultIntervalMatchesSettingsDefault pins the two places the watch
// cadence is stated to one number. DefaultInterval is returned by
// cmd/dubarr's OptionsProvider when a stored scan_interval_sec is <= 0 (a
// hand-written or pre-heal row: settings validation rejects 0), so if the
// settings default moves and this constant does not, those installs silently
// run at the old cadence while every surface advertises the new one.
func TestDefaultIntervalMatchesSettingsDefault(t *testing.T) {
	raw, ok := settings.Defaults("/data")[settings.KeyWatchOptions]
	if !ok {
		t.Fatalf("settings.Defaults() has no %s entry", settings.KeyWatchOptions)
	}
	opts, ok := raw.(settings.WatchOptions)
	if !ok {
		t.Fatalf("settings.Defaults()[%s] is %T, want settings.WatchOptions", settings.KeyWatchOptions, raw)
	}
	if opts.ScanIntervalSec <= 0 {
		t.Fatalf("settings default scan_interval_sec = %d, want a positive cadence", opts.ScanIntervalSec)
	}
	want := time.Duration(opts.ScanIntervalSec) * time.Second
	if DefaultInterval != want {
		t.Errorf("watch.DefaultInterval = %v, but the settings default is %v: "+
			"the <= 0 fallback and the seeded value must agree", DefaultInterval, want)
	}
}
