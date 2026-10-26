package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStreams redirects os.Stdout and os.Stderr into temp files for the
// duration of fn. Both are resolved at write time by fmt and by the java log
// handler, so the swap catches everything the command prints.
func captureStreams(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	outPath := filepath.Join(t.TempDir(), "stdout")
	errPath := filepath.Join(t.TempDir(), "stderr")
	outF, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	errF, err := os.Create(errPath)
	if err != nil {
		_ = outF.Close()
		t.Fatalf("create stderr file: %v", err)
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outF, errF
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	fn()

	if err := outF.Close(); err != nil {
		t.Fatalf("close stdout file: %v", err)
	}
	if err := errF.Close(); err != nil {
		t.Fatalf("close stderr file: %v", err)
	}
	outData, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read stdout capture: %v", err)
	}
	errData, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatalf("read stderr capture: %v", err)
	}
	return string(outData), string(errData)
}

// TestCLIVersionPrintsNoLogRecord locks in that `imex version` is pure
// stdout text: it prints the same fields the run banner carries, so a log
// record ahead of it would only duplicate them and break `version | grep`.
func TestCLIVersionPrintsNoLogRecord(t *testing.T) {
	logFormat = "java"
	resetChangedFlags(exportCmd, importCmd, rootCmd)
	defer resetChangedFlags(exportCmd, importCmd, rootCmd)

	stdout, stderr := captureStreams(t, func() {
		rootCmd.SetArgs([]string{"version"})
		if err := rootCmd.Execute(); err != nil {
			t.Errorf("version failed: %v", err)
		}
	})

	if strings.TrimSpace(stderr) != "" {
		t.Errorf("version must not log anything, stderr = %q", stderr)
	}
	if strings.Contains(stdout, "imex starting") || strings.Contains(stdout, "level=INFO") {
		t.Errorf("version output must not contain log records: %q", stdout)
	}
	for _, want := range []string{"imex version ", "Git commit:", "Build time:", "Go version:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version output missing %q, got %q", want, stdout)
		}
	}
}

// TestCLIRunBannerStillLogged guards the other side of the exclusion: real
// work commands still start with the banner, so this stays the first log line
// of any run that touches a database or writes a file.
func TestCLIRunBannerStillLogged(t *testing.T) {
	*exportCfg = *newExportConfigForTest()
	logFormat = "java"
	resetChangedFlags(exportCmd, importCmd, rootCmd)
	defer resetChangedFlags(exportCmd, importCmd, rootCmd)

	t.Setenv("IMEX_KEY", "")
	_, stderr := captureStreams(t, func() {
		rootCmd.SetArgs([]string{"encrypt", "secret"})
		_ = rootCmd.Execute() // fails on the missing key; the banner is earlier
	})

	if !strings.Contains(stderr, "imex starting") {
		t.Fatalf("encrypt run must emit the version banner, stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "command=encrypt") {
		t.Errorf("banner must carry the command name, stderr = %q", stderr)
	}
	if first := strings.Split(strings.TrimSpace(stderr), "\n")[0]; !strings.Contains(first, "imex starting") {
		t.Errorf("banner must be the first log line, got %q", first)
	}
}
