package _import

import (
	"strings"
	"testing"
)

func TestValidatePositionalColumns(t *testing.T) {
	if err := validatePositionalColumns([]string{"a", "b"}, []string{"x", "y"}); err != nil {
		t.Errorf("matching counts: err = %v", err)
	}
	// Header unknown (--no-col-names): only the names are checked.
	if err := validatePositionalColumns([]string{"a", "b"}, nil); err != nil {
		t.Errorf("no file header: err = %v", err)
	}
	if err := validatePositionalColumns([]string{"a", ""}, []string{"x", "y"}); err == nil {
		t.Error("empty entry accepted")
	} else if !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("error = %v, want entry 2", err)
	}
	if err := validatePositionalColumns([]string{"a"}, []string{"x", "y", "z"}); err == nil {
		t.Error("count mismatch accepted")
	} else if !strings.Contains(err.Error(), "1 target columns") || !strings.Contains(err.Error(), "3 columns") {
		t.Errorf("error = %v, want counts 1 and 3", err)
	}
}
