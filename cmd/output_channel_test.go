package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imex/internal/common"
)

// withMessages sets the package-level --messages path for one test and puts the
// previous value back afterwards. Every channel decision reads it, so the tests
// that exercise a cell of the matrix have to own it.
func withMessages(t *testing.T, path string) {
	t.Helper()
	prev := messagesPath
	messagesPath = path
	t.Cleanup(func() { messagesPath = prev })
}

func TestResultSinkWritesNothingBeforeFlush(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "result.txt")
	var stdout bytes.Buffer

	withMessages(t, "")
	sink := newResultSink(out, false, &stdout)
	if _, err := sink.Write([]byte("body\n")); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout received bytes before Flush: %q", stdout.String())
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output file created before Flush")
	}

	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "body\n" {
		t.Errorf("output = %q", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("--output must keep stdout empty, got %q", stdout.String())
	}
}

func TestResultSinkChannelMatrix(t *testing.T) {
	const payload = "row one\nrow two\n"

	// One result destination per run: whichever cell the flags select holds the
	// bytes, the others stay empty.
	cases := []struct {
		name   string
		output bool
		msgs   bool
		// want is the channel holding the payload: "stdout", "output" or "messages".
		want string
	}{
		{name: "output only", output: true, want: "output"},
		{name: "output and messages", output: true, msgs: true, want: "output"},
		{name: "neither", want: "stdout"},
		{name: "messages only", msgs: true, want: "messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "result.txt")
			msg := filepath.Join(dir, "messages.log")
			var stdout bytes.Buffer

			var output, messages string
			if tc.output {
				output = out
			}
			if tc.msgs {
				messages = msg
			}
			withMessages(t, messages)

			sink := newResultSink(output, false, &stdout)
			if _, err := sink.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			if err := sink.Flush(); err != nil {
				t.Fatal(err)
			}

			fileChannels := map[string]string{"output": out, "messages": msg}
			if got := stdout.String(); tc.want == "stdout" {
				if got != payload {
					t.Errorf("stdout = %q, want %q", got, payload)
				}
			} else if got != "" {
				t.Errorf("stdout must stay empty, got %q", got)
			}

			for channel, path := range fileChannels {
				data, err := os.ReadFile(path)
				switch {
				case tc.want == channel:
					if err != nil {
						t.Fatalf("read %s: %v", path, err)
					}
					if !strings.HasSuffix(string(data), payload) {
						t.Errorf("%s = %q, want it to end with %q", channel, data, payload)
					}
				case err == nil:
					t.Errorf("%s must stay empty, got %q", channel, data)
				}
			}
		})
	}
}

func TestResultSinkAppendsAfterExistingLogRecords(t *testing.T) {
	dir := t.TempDir()
	msg := filepath.Join(dir, "messages.log")
	// Stand in for the log sink, which opens the file in append mode before the
	// command body runs.
	if err := os.WriteFile(msg, []byte("INFO imex starting\nINFO list config\n"), 0666); err != nil {
		t.Fatal(err)
	}

	withMessages(t, msg)
	var stdout bytes.Buffer
	sink := newResultSink("", false, &stdout)
	if _, err := sink.Write([]byte("header\nrow\n")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := "INFO imex starting\nINFO list config\nheader\nrow\n"
	if string(got) != want {
		t.Errorf("message file = %q, want %q", got, want)
	}
	// The message file must not be truncated: the log records are still there.
	if !strings.Contains(string(got), "imex starting") {
		t.Error("log records lost")
	}
}

func TestResultSinkTruncatesExistingOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(out, []byte("stale bytes that must not survive\n"), 0666); err != nil {
		t.Fatal(err)
	}

	withMessages(t, "")
	sink := newResultSink(out, false, &bytes.Buffer{})
	if _, err := sink.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Errorf("output = %q, want the truncated result", got)
	}
}

func TestResultSinkNoOverwriteRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(out, []byte("keep me\n"), 0666); err != nil {
		t.Fatal(err)
	}

	withMessages(t, "")
	sink := newResultSink(out, true, &bytes.Buffer{})
	if _, err := sink.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	err := sink.Flush()
	if err == nil {
		t.Fatal("--no-overwrite accepted an existing file")
	}
	// Same wording as export's guard, so one message explains both commands.
	if !strings.Contains(err.Error(), "already exists, remove --no-overwrite to overwrite") {
		t.Errorf("error = %v", err)
	}
	if common.IsUsageError(err) {
		t.Error("--no-overwrite is a runtime refusal (exit 1), not a usage error")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep me\n" {
		t.Errorf("existing file changed: %q", got)
	}
}

func TestResultSinkNoOverwriteCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sub", "result.txt")
	// The directory exists; only the file is missing.
	if err := os.MkdirAll(filepath.Dir(out), 0777); err != nil {
		t.Fatal(err)
	}

	withMessages(t, "")
	sink := newResultSink(out, true, &bytes.Buffer{})
	if _, err := sink.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestResultSinkEmptyResultIsSilent(t *testing.T) {
	// A run with nothing to print must not create an empty message file or write a
	// stray byte to stdout.
	dir := t.TempDir()
	msg := filepath.Join(dir, "messages.log")

	for _, tc := range []struct{ name, messages string }{
		{"stdout", ""},
		{"messages", msg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withMessages(t, tc.messages)
			var stdout bytes.Buffer
			sink := newResultSink("", false, &stdout)
			if err := sink.Flush(); err != nil {
				t.Fatal(err)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q", stdout.String())
			}
			if _, err := os.Stat(msg); tc.messages != "" && err == nil {
				t.Errorf("empty result created %s", msg)
			}
		})
	}
}

func TestResultSinkUnwritableOutputIsRuntimeError(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "missing-dir", "result.txt")

	withMessages(t, "")
	sink := newResultSink(out, false, &bytes.Buffer{})
	if _, err := sink.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	err := sink.Flush()
	if err == nil {
		t.Fatal("unwritable output accepted")
	}
	if !strings.Contains(err.Error(), "failed to create output file") {
		t.Errorf("error = %v", err)
	}
	if common.IsUsageError(err) {
		t.Error("a filesystem failure is exit 1, not a usage error")
	}
}

func TestValidateDistinctPathsRefusesSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.log")

	for _, other := range []string{
		path,
		filepath.Join(dir, ".", "one.log"),
		filepath.Join(dir, "..", filepath.Base(dir), "one.log"),
		dir + "/./one.log",
	} {
		withMessages(t, path)
		err := validateDistinctPaths("output", other, true)
		if err == nil {
			t.Errorf("--output %q and --messages %q were accepted", other, path)
			continue
		}
		if !common.IsUsageError(err) {
			t.Errorf("%q vs %q: must be a usage error, got %T", other, path, err)
		}
		if !strings.Contains(err.Error(), "--output and --messages must name different files") {
			t.Errorf("error = %v", err)
		}
		if !strings.Contains(err.Error(), "omit --output to append the result") {
			t.Errorf("optional result flag must name the way out: %v", err)
		}
	}

	// cwd-relative comparison is what a user actually types.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	withMessages(t, "one.log")
	if err := validateDistinctPaths("output", filepath.Join(dir, "one.log"), false); err == nil {
		t.Error("relative and absolute spellings of one file were accepted")
	}
}

func TestValidateDistinctPathsAllowsDifferentFiles(t *testing.T) {
	dir := t.TempDir()
	withMessages(t, filepath.Join(dir, "messages.log"))
	for _, result := range []string{"", filepath.Join(dir, "result.txt")} {
		if err := validateDistinctPaths("output", result, true); err != nil {
			t.Errorf("result %q refused: %v", result, err)
		}
	}
}

func TestValidateDistinctPathsRequiredFlagHasNoEscapeHatch(t *testing.T) {
	// export and import always name a result file, so the message cannot offer to
	// drop it.
	dir := t.TempDir()
	path := filepath.Join(dir, "one.log")
	withMessages(t, path)
	err := validateDistinctPaths("output", path, false)
	if err == nil {
		t.Fatal("same path accepted")
	}
	if strings.Contains(err.Error(), "omit") {
		t.Errorf("required flag must not suggest omitting it: %v", err)
	}
}
