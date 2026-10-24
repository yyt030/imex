package config

import (
	"testing"
	"time"
)

// The progress ticker takes its interval from the resolved config value, so the
// shipped default must be the documented 5 s and an unset or nonsensical value
// must still resolve to a positive duration: time.NewTicker panics on <= 0, and
// a library caller that built a Config by hand must not be able to crash a run.
func TestEffectiveProgressInterval(t *testing.T) {
	tests := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"CLI default", DefaultProgressInterval, 5 * time.Second},
		{"explicit sub-second", 200 * time.Millisecond, 200 * time.Millisecond},
		{"explicit minute", time.Minute, time.Minute},
		{"zero falls back", 0, DefaultProgressInterval},
		{"negative falls back", -time.Second, DefaultProgressInterval},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewExportConfig()
			cfg.ProgressInterval = tt.set
			if got := cfg.EffectiveProgressInterval(); got != tt.want {
				t.Errorf("EffectiveProgressInterval() with %v = %v, want %v", tt.set, got, tt.want)
			}

			// Import shares the field and the resolution, being on the embedded
			// Config rather than on either command's own struct.
			imp := NewImportConfig()
			imp.ProgressInterval = tt.set
			if got := imp.EffectiveProgressInterval(); got != tt.want {
				t.Errorf("import EffectiveProgressInterval() with %v = %v, want %v", tt.set, got, tt.want)
			}
		})
	}
}

// NewExportConfig and NewImportConfig must both start a run on the default
// cadence; a zero here would silently disable progress reporting.
func TestConfigsDefaultToTheProgressInterval(t *testing.T) {
	if got := NewExportConfig().ProgressInterval; got != DefaultProgressInterval {
		t.Errorf("export default ProgressInterval = %v, want %v", got, DefaultProgressInterval)
	}
	if got := NewImportConfig().ProgressInterval; got != DefaultProgressInterval {
		t.Errorf("import default ProgressInterval = %v, want %v", got, DefaultProgressInterval)
	}
}
