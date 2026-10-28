package _import

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// captureLog runs fn with the command log pointed at a file instead of the
// terminal, and returns what was written. The boundary behaviours under test here
// are the reports the code makes about itself, so the record is the observable.
func captureLog(t *testing.T, fn func()) string {
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

// The driver will not interpolate a statement above its own 64 MiB ceiling
// (driverInterpolationCap): it returns driver.ErrSkip and database/sql silently
// rebuilds every statement as a server-side prepared statement. Measured on an
// 80 MiB import, that fallback cost 2.078s where 8 MiB COM_QUERY batches took
// 1.195s. A server advertising 128 MiB max_allowed_packet therefore yields a
// 120 MB budget that is strictly worse than the default, which is why
// batchCeilings clamps it and says so.
func TestBatchCeilingsClampsToDriverInterpolationCap(t *testing.T) {
	server128 := batchByteLimit(128 << 20) // 120,795,955 by the 9/10 margin, not a bed value
	if want := int64(120795955); server128 != want {
		t.Fatalf("formula assumption: batchByteLimit(128MiB) = %d, want %d", server128, want)
	}
	driverBudget := driverBatchBudget()

	cases := []struct {
		name          string
		packetBudget  int64
		statementSize int
		wantBudget    int64
		wantLimiting  string
		wantAbove     bool
	}{
		{"big server budget is clamped to the driver ceiling", server128, 0, driverBudget, "driver_packet_cap", false},
		{"server budget inside the ceiling is left alone", batchByteLimit(4 << 20), 0, batchByteLimit(4 << 20), "max_allowed_packet", false},
		{"explicit cap below the ceiling is honoured", server128, 8 << 20, 8 << 20, "statement-size", false},
		{"explicit cap above the ceiling is honoured and reported", server128, 100 << 20, 100 << 20, "statement-size", true},
		// A --statement-size wider than the server budget is not in force, so the
		// clamp still applies: the user asked for "up to 100 MB", not "at least".
		{"oversized explicit cap does not defeat the clamp", batchByteLimit(4 << 20), 100 << 20, batchByteLimit(4 << 20), "max_allowed_packet", false},
		{"zero keeps the clamped default", server128, 0, driverBudget, "driver_packet_cap", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			budget, limiting, above := batchCeilings(c.packetBudget, c.statementSize)
			if budget != c.wantBudget || limiting != c.wantLimiting || above != c.wantAbove {
				t.Fatalf("batchCeilings(%d, %d) = (%d, %q, %v), want (%d, %q, %v)",
					c.packetBudget, c.statementSize, budget, limiting, above,
					c.wantBudget, c.wantLimiting, c.wantAbove)
			}
		})
	}
}

// The clamped value must stay on the interpolated side of the ceiling with room
// for what estimateRowBytes does not count, otherwise the fix would be a no-op
// that still flips protocol on the very batches it is meant to prevent.
func TestDriverBatchBudgetStaysInsideTheCap(t *testing.T) {
	budget := driverBatchBudget()
	if budget >= driverInterpolationCap {
		t.Fatalf("driver budget %d is not below the interpolation ceiling %d", budget, driverInterpolationCap)
	}
	if want := batchByteLimit(driverInterpolationCap); budget != want {
		t.Fatalf("driver budget = %d, want the shared margin result %d", budget, want)
	}
}

// --pipeline-buffer counts batches and one batch can hold the whole byte budget, so
// the depth is also a memory statement. Measured on a 200,688,900-byte / 100,000-row
// import at --threads 32, three repeats each, on a server reporting the OceanBase V4.x
// default max_allowed_packet=16M (budget 15,099,494 B): 76,616-152,040 KB in
// 3.78-5.07s at depth 1024 and 122,740-149,412 KB in 4.45-4.83s at the default depth
// 10. The two ranges overlap, so no single figure is quoted as the price - and that is
// itself the finding: once a batch is only 15 MB, the reader cannot stay far enough
// ahead for depth to dominate peak RSS. On a 128 MB server the same file did separate
// (147,340-292,356 KB at depth 1024, 126,972-207,392 KB at depth 10), and the older
// 280,288 KB / 112,584 KB pair predates the removal of the depth clamp entirely.
// queuedBytes is that price, and it saturates rather than wrapping, because depth
// times an explicit --statement-size of any width is a user-scale product.
func TestQueuedBytesIsThePriceOfTheDepth(t *testing.T) {
	cases := []struct {
		name       string
		buffer     int
		batchBytes int64
		want       int64
	}{
		{"default depth at the clamped budget", 10, 60 << 20, 10 * int64(60<<20)},
		{"a deep queue scales linearly", 1024, 60 << 20, 1024 * int64(60<<20)},
		{"synchronous hand-off retains nothing", 0, 60 << 20, 0},
		{"a negative depth (library caller) is no queue", -5, 60 << 20, 0},
		{"an unset budget cannot be multiplied", 7, 0, 0},
		{"a zero depth cannot overflow", 0, math.MaxInt64, 0},
		{"an absurd product saturates instead of wrapping", 1024, math.MaxInt64, math.MaxInt64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := queuedBytes(c.buffer, c.batchBytes); got != c.want {
				t.Fatalf("queuedBytes(%d, %d) = %d, want %d", c.buffer, c.batchBytes, got, c.want)
			}
		})
	}
}

// Going past the envelope must be reported — that is the whole difference between a
// bound and a warning — but the depth itself is the operator's number and stays as
// written. The --threads a run uses is not part of this arithmetic: that knob is
// concurrency, this one is a queue, and the queue is shared by the workers.
func TestResolveQueueDepthHonoursTheDepthAndReportsItsPrice(t *testing.T) {
	defer common.InitLogger(config.LogLevelInfo)
	common.InitLogger(config.LogLevelInfo)

	cfg := config.NewImportConfig()
	cfg.PipelineBuffer = 1024
	imp := &Importer{cfg: cfg}

	logged := captureLog(t, func() { imp.resolveQueueDepth(60 << 20) })
	if imp.queueDepth != 1024 {
		t.Fatalf("queue depth = %d, want the configured 1024: the envelope reports, it does not convert", imp.queueDepth)
	}
	for _, want := range []string{
		"the hand-off queue may retain more than the memory envelope",
		"requested=1024",
		"batch_bytes=62914560",
		"envelope_bytes=1073741824",
		"lower --pipeline-buffer",
		"queued_bytes=" + strconv.FormatInt(queuedBytes(1024, 60<<20), 10),
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("envelope record lacks %q:\n%s", want, logged)
		}
	}

	// A depth inside the envelope stays silent, and a library caller's negative depth
	// becomes the synchronous hand-off instead of a negative channel capacity.
	cfg2 := config.NewImportConfig()
	imp2 := &Importer{cfg: cfg2}
	logged = captureLog(t, func() { imp2.resolveQueueDepth(64 << 10) })
	if imp2.queueDepth != config.DefaultPipelineBuffer {
		t.Fatalf("queue depth = %d, want the configured default %d", imp2.queueDepth, config.DefaultPipelineBuffer)
	}
	if strings.Contains(logged, "--pipeline-buffer") {
		t.Errorf("a depth inside the envelope was reported anyway:\n%s", logged)
	}

	cfg3 := config.NewImportConfig()
	cfg3.PipelineBuffer = -1
	imp3 := &Importer{cfg: cfg3}
	if got := imp3.resolveQueueDepth(60 << 20); got != 0 {
		t.Fatalf("negative depth resolved to %d, want 0 (a channel cannot be negative)", got)
	}
}

// replayBudget divides one budget across writers but keeps a per-writer floor, so
// past eight workers the retained total grows with --threads (256 x 8 MiB = 2 GiB).
// That is a real envelope, not the documented constant, and the run has to say so
// instead of letting the comment claim otherwise.
func TestWarnIfReplayWindowScalesReportsEnvelope(t *testing.T) {
	defer common.InitLogger(config.LogLevelInfo)
	common.InitLogger(config.LogLevelInfo)

	logged := captureLog(t, func() { warnIfReplayWindowScales(256) })
	total := int64(256) * int64(replayBudget(256))
	for _, want := range []string{"replay window scales with threads", "threads=256", "total_bytes="} {
		if !strings.Contains(logged, want) {
			t.Errorf("envelope record lacks %q:\n%s", want, logged)
		}
	}
	if total <= DefaultMaxPendingBytes {
		t.Fatalf("formula assumption: 256 workers retain %d, want more than %d", total, DefaultMaxPendingBytes)
	}

	// Four workers stay inside the documented budget and must be silent.
	logged = captureLog(t, func() { warnIfReplayWindowScales(4) })
	if strings.Contains(logged, "replay window") {
		t.Errorf("a within-budget worker count was reported anyway:\n%s", logged)
	}
}
