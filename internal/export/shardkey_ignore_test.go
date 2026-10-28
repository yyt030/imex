package export

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// captureExportLog runs fn with the command log pointed at a file instead of the
// terminal, and returns what was written. What is under test here is the report the
// code makes about itself, so the record is the observable.
func captureExportLog(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create capture file: %v", err)
	}
	orig := os.Stderr
	os.Stderr = f
	fn()
	os.Stderr = orig
	if err := f.Close(); err != nil {
		t.Fatalf("close capture file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	return string(data)
}

// exporterWithConfig builds the smallest Exporter that can answer a question about
// its own configuration: the shard-key warning reads nothing but the config.
func exporterWithConfig(cfg *config.ExportConfig) *Exporter {
	return &Exporter{config: cfg}
}

func shardKeyWarnCases() []struct {
	name     string
	threads  int
	shardKey string
	want     bool
} {
	// warnIfShardKeyIgnored is the single-threaded path's own report, so every case
	// here is a run that already got there; whether a parallel run reaches it at all
	// is the live export tests' question.
	return []struct {
		name     string
		threads  int
		shardKey string
		want     bool
	}{
		{"--threads 1 with a key", 1, "id", true},
		{"--threads 1 with an unknown key", 1, "nosuchcol", true},
		{"fallback from --threads 4", 4, "id", true},
		{"fallback from --threads 16", 16, "id", true},
		{"no key asked for", 1, "", false},
	}
}

// TestWarnIfShardKeyIgnoredReportsEverySingleThreadedRoute pins the fix itself: a
// requested shard key that the run will never read has to be named on the log, on
// both routes into the single-threaded path — an explicit --threads 1 and a parallel
// request that fell back.
func TestWarnIfShardKeyIgnoredReportsEverySingleThreadedRoute(t *testing.T) {
	for _, tc := range shardKeyWarnCases() {
		cfg := config.NewExportConfig()
		cfg.Format = config.FormatDel
		cfg.Threads = tc.threads
		cfg.ShardKey = tc.shardKey

		out := captureExportLog(t, func() { exporterWithConfig(cfg).warnIfShardKeyIgnored() })

		if !tc.want {
			if strings.Contains(out, "single-threaded export ignores --shard-key") {
				t.Errorf("%s: record printed, want silence:\n%s", tc.name, out)
			}
			continue
		}
		for _, want := range []string{"single-threaded export ignores --shard-key", "shard_key=" + tc.shardKey} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: record is missing %q:\n%s", tc.name, want, out)
			}
		}
		// threads carries the requested value either way, so the operator can see
		// whether the run asked for parallelism and lost it.
		if !strings.Contains(out, "threads="+strconv.Itoa(tc.threads)) {
			t.Errorf("%s: record does not name threads=%d:\n%s", tc.name, tc.threads, out)
		}
		wantFallback := map[bool]string{true: "fallback=true", false: "fallback=false"}[tc.threads > 1]
		if !strings.Contains(out, wantFallback) {
			t.Errorf("%s: record does not carry %s:\n%s", tc.name, wantFallback, out)
		}
	}
}

// TestExportConfigRecordMarksAnUnusableShardKey is the printing half of the same
// defect: a bare shard_key=nosuchcol in the configuration record read as a setting
// the run applies, on the one path that never looks at it.
func TestExportConfigRecordMarksAnUnusableShardKey(t *testing.T) {
	cases := []struct {
		name     string
		threads  int
		shardKey string
		want     bool
	}{
		{"single-threaded with a key", 1, "id", true},
		{"single-threaded without a key", 1, "", false},
		{"parallel with a key", 3, "id", false},
	}
	for _, format := range []string{config.FormatDel, config.FormatSQL, config.FormatParquet} {
		for _, tc := range cases {
			cfg := config.NewExportConfig()
			cfg.Format = format
			cfg.Threads = tc.threads
			cfg.ShardKey = tc.shardKey

			keys := configKeys(t, exportConfigFields(cfg))
			if keys["shard_key"] != tc.shardKey {
				t.Errorf("--format %s %s: shard_key=%v, want %q", format, tc.name, keys["shard_key"], tc.shardKey)
			}
			ignored, present := keys["shard_key_ignored"]
			if present != tc.want {
				t.Errorf("--format %s %s: shard_key_ignored present=%v value=%v, want present=%v",
					format, tc.name, present, ignored, tc.want)
			}
			if present && ignored != true {
				t.Errorf("--format %s %s: shard_key_ignored=%v, want true", format, tc.name, ignored)
			}
		}
	}
}
