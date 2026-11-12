package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/common"
)

// resultSink is the channel model `list`, `show` and `exec` share
// (docs/requirements.md 3.6). All three write their result through it instead of
// straight to stdout, and the sink decides where those bytes land once the run
// has produced them all:
//
//	--output → that file, truncated (refused with --no-overwrite)
//	neither  → stdout
//
// --messages is deliberately NOT a result channel. It takes the command log and
// the errors (cmd/root.go hands them over before the command body runs), and the
// result still goes to stdout or --output. 3.6's original ruling folded the
// result into the message file when no --output was given; the revision of
// 2026-11-12 (变更记录 158) dropped that cell, because a file that carries both
// log records and table rows is neither a log nor a data file — `-m log -o out`
// already expresses both destinations separately.
//
// The result is still buffered rather than streamed: a failed run must not leave
// half a table in the file (the sink only flushes after the command returns
// nil). The buffer is bounded by what one command prints — measured on this
// repository's test server, the largest ordinary result is a whole-server
// `list tables` at 12,747 bytes.
//
// export and import do not use this: their --output is required, so they have
// never had a result to place anywhere else.
type resultSink struct {
	buf         bytes.Buffer
	output      string
	noOverwrite bool
	stdout      io.Writer
}

// newResultSink builds the sink for one run. stdout is the command's own output
// writer (cmd.OutOrStdout()), so tests that capture a command still see the
// result.
func newResultSink(output string, noOverwrite bool, stdout io.Writer) *resultSink {
	return &resultSink{
		output:      strings.TrimSpace(output),
		noOverwrite: noOverwrite,
		stdout:      stdout,
	}
}

// Write buffers the result bytes; nothing reaches any channel until Flush.
func (s *resultSink) Write(p []byte) (int, error) { return s.buf.Write(p) }

// Flush delivers the buffered result to the channel the flags select. It is only
// ever called after the command finished successfully, so a failed run leaves no
// half result behind in either file.
func (s *resultSink) Flush() error {
	data := s.buf.Bytes()

	// No --output: stdout is the destination, and --messages has no say here.
	if s.output == "" {
		if len(data) == 0 {
			return nil
		}
		if _, err := s.stdout.Write(data); err != nil {
			return errors.Wrap(err, "failed to write result to stdout")
		}
		return nil
	}

	if s.noOverwrite {
		if _, err := os.Stat(s.output); err == nil {
			return errors.Errorf("output file %s already exists, remove --no-overwrite to overwrite", s.output)
		}
	}
	f, err := os.Create(s.output)
	if err != nil {
		return errors.Wrapf(err, "failed to create output file %s", s.output)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return errors.Wrapf(err, "failed to write output file %s", s.output)
	}
	if err := f.Close(); err != nil {
		return errors.Wrapf(err, "failed to close output file %s", s.output)
	}
	return nil
}

// validateDistinctPaths refuses the result file and the message file being the
// same path. The result is written truncated while the log is appended, so two
// writers on one path leave a file that is neither. The refusal is a usage error
// because the fix is a flag decision, not a data problem.
//
// It guards all four commands: export and import have always been able to collide
// here, and list, show and exec do too. When the result flag is optional the
// message also names the way out: dropping --output sends the result to stdout,
// which is a supported choice rather than a loss.
func validateDistinctPaths(resultFlag, path string, optional bool) error {
	messages := strings.TrimSpace(messagesPath)
	path = strings.TrimSpace(path)
	if path == "" || messages == "" {
		return nil
	}
	a, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	b, err := filepath.Abs(messages)
	if err != nil {
		return nil
	}
	if filepath.Clean(a) != filepath.Clean(b) {
		return nil
	}
	if optional {
		return common.Usagef("--%s and --messages must name different files (omit --%s to send the result to stdout)", resultFlag, resultFlag)
	}
	return common.Usagef("--%s and --messages must name different files", resultFlag)
}
