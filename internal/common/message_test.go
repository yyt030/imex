package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readMessageFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestMessageWriterBeginAndWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.msg")
	w, err := NewMessageWriter(path, "Export")
	if err != nil {
		t.Fatalf("NewMessageWriter: %v", err)
	}
	if err := w.WriteHeader("export -o out.del -q \"SELECT * FROM users\""); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := w.WriteBegin("users"); err != nil {
		t.Fatalf("WriteBegin: %v", err)
	}
	if err := w.WriteWarning("hex-blob explicitly disabled"); err != nil {
		t.Fatalf("WriteWarning: %v", err)
	}
	if err := w.WriteProgress(7); err != nil {
		t.Fatalf("WriteProgress: %v", err)
	}
	if err := w.WriteFooter(7); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	text := readMessageFile(t, path)
	for _, want := range []string{
		`SQL3104N  The utility is beginning to export data from the table "users".`,
		"SQL3107N  hex-blob explicitly disabled",
		"SQL3107N  At least one warning message was encountered.",
		"SQL3105N  The utility has exported 7 rows.",
		`SQL3110N  The utility has completed processing.  "7" rows were exported.`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("message file missing %q:\n%s", want, text)
		}
	}
}

func TestMessageWriterImportBegin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "import.msg")
	w, err := NewMessageWriter(path, "Import")
	if err != nil {
		t.Fatalf("NewMessageWriter: %v", err)
	}
	if err := w.WriteHeader("import -i users.del -t users"); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := w.WriteBegin("users"); err != nil {
		t.Fatalf("WriteBegin: %v", err)
	}
	if err := w.WriteFooter(0); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	w.Close()

	text := readMessageFile(t, path)
	if !strings.Contains(text, `SQL3109N  The utility is beginning to import data into the table "users".`) {
		t.Errorf("missing import begin line:\n%s", text)
	}
	if strings.Contains(text, "SQL3107N") {
		t.Errorf("unexpected warning summary without warnings:\n%s", text)
	}
}

func TestMessageWriterBeginWithoutTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.msg")
	w, err := NewMessageWriter(path, "Export")
	if err != nil {
		t.Fatalf("NewMessageWriter: %v", err)
	}
	if err := w.WriteBegin(""); err != nil {
		t.Fatalf("WriteBegin: %v", err)
	}
	w.Close()

	if text := readMessageFile(t, path); !strings.Contains(text, "SQL3104N  The utility is beginning to export data.") {
		t.Errorf("missing table-less begin line:\n%s", text)
	}
}

func TestMessageWriterNoFileIsNoop(t *testing.T) {
	w, err := NewMessageWriter("", "Export")
	if err != nil {
		t.Fatalf("NewMessageWriter: %v", err)
	}
	if err := w.WriteHeader("x"); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := w.WriteBegin("users"); err != nil {
		t.Fatalf("WriteBegin: %v", err)
	}
	if err := w.WriteWarning("w"); err != nil {
		t.Fatalf("WriteWarning: %v", err)
	}
	if err := w.WriteProgress(1); err != nil {
		t.Fatalf("WriteProgress: %v", err)
	}
	if err := w.WriteFooter(1); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMessageWriterHeaderResetsWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reuse.msg")
	w, err := NewMessageWriter(path, "Import")
	if err != nil {
		t.Fatalf("NewMessageWriter: %v", err)
	}
	defer w.Close()

	if err := w.WriteWarning("first run"); err != nil {
		t.Fatalf("WriteWarning: %v", err)
	}
	if w.warnings != 1 {
		t.Fatalf("warnings = %d, want 1", w.warnings)
	}
	// A second run on the same writer starts with a clean warning count.
	if err := w.WriteHeader("import -i x -t y"); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if w.warnings != 0 {
		t.Fatalf("warnings = %d after WriteHeader, want 0", w.warnings)
	}
}
