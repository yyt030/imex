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

// resultSink is the channel model `list` and `show` share (docs/requirements.md
// 3.6). Both commands write their result through it instead of straight to
// stdout, and the sink decides where those bytes land once the run has produced
// them all:
//
//	--output               → that file, truncated (refused with --no-overwrite)
//	no --output, --messages → appended to the message file, after its log records
//	neither                 → stdout
//
// The result is buffered rather than streamed because the message file is written
// by the log sink while the command runs: interleaving the two would put table
// rows in the middle of log records. The buffer is bounded by what one command
// prints — measured on this repository's test server, the largest ordinary result
// is a whole-server `list tables` at 12,747 bytes — so the trade is size for
// ordering. Logs themselves are untouched by the sink and keep going to stderr,
// or to the message file when --messages names one.
//
// export and import do not use this: their --output is required, so they have
// never had a result to place anywhere else.
type resultSink struct {
	buf         bytes.Buffer
	output      string
	noOverwrite bool
	stdout      io.Writer
	messages    string
}

// newResultSink builds the sink for one run. stdout is the command's own output
// writer (cmd.OutOrStdout()), so tests that capture a command still see the
// result.
func newResultSink(output string, noOverwrite bool, stdout io.Writer) *resultSink {
	return &resultSink{
		output:      strings.TrimSpace(output),
		noOverwrite: noOverwrite,
		stdout:      stdout,
		messages:    strings.TrimSpace(messagesPath),
	}
}

// Write buffers the result bytes; nothing reaches any channel until Flush.
func (s *resultSink) Write(p []byte) (int, error) { return s.buf.Write(p) }

// Flush delivers the buffered result to the channel the flags select. It is only
// ever called after the command finished successfully, so a failed run leaves no
// half result behind in either file.
func (s *resultSink) Flush() error {
	data := s.buf.Bytes()
	switch {
	case s.output != "":
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
	case s.messages != "":
		if len(data) == 0 {
			return nil
		}
		// The message file is already open by the log sink in append mode, and a
		// second O_APPEND handle keeps that ordering: the log's own records come
		// first, the result last.
		f, err := os.OpenFile(s.messages, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			return errors.Wrapf(err, "failed to open message file %s", s.messages)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return errors.Wrapf(err, "failed to append result to message file %s", s.messages)
		}
		if err := f.Close(); err != nil {
			return errors.Wrapf(err, "failed to close message file %s", s.messages)
		}
		return nil
	default:
		if len(data) == 0 {
			return nil
		}
		if _, err := s.stdout.Write(data); err != nil {
			return errors.Wrap(err, "failed to write result to stdout")
		}
		return nil
	}
}

// validateDistinctPaths refuses the result file and the message file being the
// same path. The result is written truncated while the log is appended, so two
// writers on one path leave a file that is neither. The refusal is a usage error
// because the fix is a flag decision, not a data problem.
//
// It guards all four commands: export and import have always been able to collide
// here, and list and show do now that their result can name a file. When the
// result flag is optional the message also names the way out, because dropping it
// is a supported choice rather than a loss.
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
		return common.Usagef("--%s and --messages must name different files (omit --%s to append the result to the message file)", resultFlag, resultFlag)
	}
	return common.Usagef("--%s and --messages must name different files", resultFlag)
}
