package common

import (
	"fmt"
	"github.com/pkg/errors"
	"os"
	"strings"
	"time"
)

// MessageWriter writes operation messages to a message file.
// The format follows the DB2-style message file convention.
type MessageWriter struct {
	file      *os.File
	opType    string // "Export" or "Import"
	startTime time.Time
}

// NewMessageWriter creates a new MessageWriter for the given operation type.
// If path is empty, the writer is a no-op.
func NewMessageWriter(path string, opType string) (*MessageWriter, error) {
	if path == "" {
		return &MessageWriter{opType: opType}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create message file %s", path)
	}
	return &MessageWriter{
		file:      f,
		opType:    opType,
		startTime: time.Now(),
	}, nil
}

// WriteHeader writes the message file header.
func (w *MessageWriter) WriteHeader(command string) error {
	if w.file == nil {
		return nil
	}
	w.startTime = time.Now()
	lines := []string{
		fmt.Sprintf("# %s Messages", w.opType),
		fmt.Sprintf("# Started: %s", w.startTime.Format("2006-01-02 15:04:05")),
		fmt.Sprintf("# Command: %s", command),
		"",
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w.file, line); err != nil {
			return err
		}
	}
	return nil
}

// WriteMessage writes a single message line.
func (w *MessageWriter) WriteMessage(msg string) error {
	if w.file == nil {
		return nil
	}
	_, err := fmt.Fprintln(w.file, msg)
	return err
}

// WriteProgress writes a progress message with row count.
func (w *MessageWriter) WriteProgress(rows int64) error {
	return w.WriteMessage(fmt.Sprintf("SQL3105N  The utility has %s %d rows.",
		w.progressVerb(), rows))
}

// WriteFooter writes the completion footer with statistics.
func (w *MessageWriter) WriteFooter(totalRows int64) error {
	if w.file == nil {
		return nil
	}
	endTime := time.Now()
	duration := endTime.Sub(w.startTime).Seconds()
	rate := float64(0)
	if duration > 0 {
		rate = float64(totalRows) / duration
	}

	lines := []string{
		fmt.Sprintf("SQL3110N  The utility has completed processing.  \"%d\" rows were %s.",
			totalRows, w.completedVerb()),
		fmt.Sprintf("# Completed: %s", endTime.Format("2006-01-02 15:04:05")),
		fmt.Sprintf("# Duration: %.0f seconds", duration),
		fmt.Sprintf("# Rate: %.2f rows/sec", rate),
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w.file, line); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the underlying file.
func (w *MessageWriter) Close() error {
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

func (w *MessageWriter) progressVerb() string {
	if w.opType == "Export" {
		return "exported"
	}
	return "imported"
}

func (w *MessageWriter) completedVerb() string {
	if w.opType == "Export" {
		return "exported"
	}
	return "imported"
}

// FormatCommand formats the command arguments into a single string for the message header.
func FormatCommand(args []string) string {
	return strings.Join(args, " ")
}
