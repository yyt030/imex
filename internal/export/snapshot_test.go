package export

import (
	"testing"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// TestSnapshotStepsAreSentOnlyWhenAsked pins the default of the read-consistency
// option at the place the server-visible statements are decided: with
// --snapshot-consistency absent, no connection runs any isolation statement at
// all, and with it given, every engine gets exactly the steps it has always had.
// The step contents themselves belong to the dialect layer and are pinned by
// internal/backend; what matters here is the gate.
func TestSnapshotStepsAreSentOnlyWhenAsked(t *testing.T) {
	engines := []backend.Engine{backend.EngineMySQL, backend.EnginePG, backend.EngineDM, backend.EngineGauss}
	for _, e := range engines {
		be := backend.Must(e)
		if got := snapshotSteps(be, false); got != nil {
			t.Errorf("engine %s: default run sends %d snapshot steps (%v), want none", e, len(got), got)
		}
		on := snapshotSteps(be, true)
		want := be.SnapshotSteps()
		if len(on) != len(want) {
			t.Errorf("engine %s: opted-in run sends %d steps, want %d", e, len(on), len(want))
			continue
		}
		for i := range on {
			if on[i] != want[i] {
				t.Errorf("engine %s step %d = (%q, %q), want (%q, %q)",
					e, i, on[i].SQL, on[i].Warn, want[i].SQL, want[i].Warn)
			}
		}
	}
}

// TestSnapshotConsistencyConfigDefaultIsOff keeps the library face in step with
// the CLI face: a caller that builds an ExportConfig by hand gets the same
// default the CLI documents, and the zero value is the documented one rather
// than a value NewExportConfig has to remember to set.
func TestSnapshotConsistencyConfigDefaultIsOff(t *testing.T) {
	if config.NewExportConfig().SnapshotConsistency {
		t.Error("NewExportConfig() defaults SnapshotConsistency to true, want false")
	}
	var zero config.ExportConfig
	if zero.SnapshotConsistency {
		t.Error("the zero ExportConfig enables the snapshot statements, want false")
	}
}

// TestSnapshotConsistencyIsFormatIndependent: the option decides what the read
// connections send, which no writer format changes, so it must never be rejected
// for a format and must always appear in the configuration record.
func TestSnapshotConsistencyIsFormatIndependent(t *testing.T) {
	scopes := config.ExportOptionScopes()
	for _, format := range []string{config.FormatDel, config.FormatSQL, config.FormatParquet} {
		if !scopes.AppliesTo("snapshot-consistency", format) {
			t.Errorf("--format %s: snapshot-consistency reported as not applying", format)
		}
		cfg := config.NewExportConfig()
		cfg.Format = format
		keys := configKeys(t, exportConfigFields(cfg))
		v, ok := keys["snapshot_consistency"]
		if !ok {
			t.Errorf("--format %s: configuration record carries no snapshot_consistency", format)
			continue
		}
		if v != false {
			t.Errorf("--format %s: snapshot_consistency = %v, want false by default", format, v)
		}
	}
}
