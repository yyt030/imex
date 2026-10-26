//go:build clifuzz

package cmd

import (
	"strings"
	"testing"
)

// FuzzValidateBatchSize checks boundary consistency for validateBatchSize.
func FuzzValidateBatchSize(f *testing.F) {
	// Seed with boundary values
	f.Add(0)
	f.Add(1)
	f.Add(100000)
	f.Add(100001)
	f.Add(-1)
	f.Add(50000)

	f.Fuzz(func(t *testing.T, size int) {
		err := validateBatchSize(size)
		if size >= 1 && size <= 100000 {
			if err != nil {
				t.Errorf("validateBatchSize(%d) returned error for valid value: %v", size, err)
			}
		} else {
			if err == nil {
				t.Errorf("validateBatchSize(%d) should return error for invalid value", size)
			} else if !strings.Contains(err.Error(), "--batch-size") {
				t.Errorf("error message should contain '--batch-size', got: %v", err)
			}
		}
	})
}

// FuzzValidatePipelineBuffer checks boundary consistency for validatePipelineBuffer.
func FuzzValidatePipelineBuffer(f *testing.F) {
	// Seed with boundary values
	f.Add(-1)
	f.Add(0)
	f.Add(1)
	f.Add(1024)
	f.Add(1025)
	f.Add(512)

	f.Fuzz(func(t *testing.T, buffer int) {
		err := validatePipelineBuffer(buffer)
		if buffer >= 0 && buffer <= 1024 {
			if err != nil {
				t.Errorf("validatePipelineBuffer(%d) returned error for valid value: %v", buffer, err)
			}
		} else {
			if err == nil {
				t.Errorf("validatePipelineBuffer(%d) should return error for invalid value", buffer)
			} else if !strings.Contains(err.Error(), "--pipeline-buffer") {
				t.Errorf("error message should contain '--pipeline-buffer', got: %v", err)
			}
		}
	})
}

// FuzzValidateStatementSize checks boundary consistency for validateStatementSize.
func FuzzValidateStatementSize(f *testing.F) {
	// Seed with boundary values
	f.Add(0)
	f.Add(1)
	f.Add(268435456)
	f.Add(268435457)
	f.Add(-1)
	f.Add(134217728)

	f.Fuzz(func(t *testing.T, size int) {
		err := validateStatementSize(size)
		if size > 0 && size <= 268435456 {
			if err != nil {
				t.Errorf("validateStatementSize(%d) returned error for valid value: %v", size, err)
			}
		} else {
			if err == nil {
				t.Errorf("validateStatementSize(%d) should return error for invalid value", size)
			} else if !strings.Contains(err.Error(), "--statement-size") {
				t.Errorf("error message should contain '--statement-size', got: %v", err)
			}
		}
	})
}

// FuzzValidateImportStatementSize checks the import-side boundary of the same
// option name: 0 is legal here (no extra cap), which is the one case
// FuzzValidateStatementSize asserts the other way round.
func FuzzValidateImportStatementSize(f *testing.F) {
	f.Add(0)
	f.Add(1)
	f.Add(65536)
	f.Add(268435456)
	f.Add(268435457)
	f.Add(-1)
	f.Add(-268435457)

	f.Fuzz(func(t *testing.T, size int) {
		err := validateImportStatementSize(size)
		if size >= 0 && size <= 268435456 {
			if err != nil {
				t.Errorf("validateImportStatementSize(%d) returned error for valid value: %v", size, err)
			}
		} else {
			if err == nil {
				t.Errorf("validateImportStatementSize(%d) should return error for invalid value", size)
			} else if !strings.Contains(err.Error(), "--statement-size") {
				t.Errorf("error message should contain '--statement-size', got: %v", err)
			}
		}
	})
}

// FuzzValidateThreads checks boundary consistency for validateThreads.
func FuzzValidateThreads(f *testing.F) {
	// Seed with boundary values
	f.Add(0)
	f.Add(1)
	f.Add(256)
	f.Add(257)
	f.Add(-1)
	f.Add(128)

	f.Fuzz(func(t *testing.T, threads int) {
		err := validateThreads(threads)
		if threads >= 1 && threads <= 256 {
			if err != nil {
				t.Errorf("validateThreads(%d) returned error for valid value: %v", threads, err)
			}
		} else {
			if err == nil {
				t.Errorf("validateThreads(%d) should return error for invalid value", threads)
			} else if !strings.Contains(err.Error(), "--threads") {
				t.Errorf("error message should contain '--threads', got: %v", err)
			}
		}
	})
}
