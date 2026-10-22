package _import

import (
	"bufio"
	"bytes"
	"io"
)

// recordReader is a scanner-like reader driven by a bufio.SplitFunc, but with
// no upper bound on token size. It exists because the exporter emits single-row
// INSERT statements that can be tens of megabytes long (measured: 15 MB); the
// standard bufio.Scanner caps at 10 MB even after Buffer() and would return
// "token too long" on such input.
//
// The buffer is unbounded by design. Process-level memory is bounded by
// GOMEMLIMIT instead.
//
// The internal layout mirrors bufio.Scanner's cursor design: [start, end) is
// the unconsumed window inside buf. When a split returns advance > 0, start
// moves forward without copying. When start == end the window resets to the
// buffer head so the full capacity is reusable. Compaction (copy to head) only
// happens inside refill(), and the buffer grows geometrically (doubling) only
// when refill cannot fit the incoming read — so amortised cost stays O(1) per
// byte and no O(n²) memmove occurs.
type recordReader struct {
	br      *bufio.Reader
	split   bufio.SplitFunc
	buf     []byte
	start   int    // unconsumed data start
	end     int    // unconsumed data end
	token   []byte // alias into buf, not a copy
	err     error
	done    bool // EOF reached
	initCap int
}

// newRecordReader creates a recordReader driven by split. r may be nil: several
// tests call pure-parse helpers that never read data; in that case Scan returns
// false immediately and Err is nil.
func newRecordReader(r *bufio.Reader, split bufio.SplitFunc) *recordReader {
	if r == nil {
		return &recordReader{done: true}
	}
	const initCap = 64 * 1024
	return &recordReader{
		br:      r,
		split:   split,
		buf:     make([]byte, initCap),
		initCap: initCap,
	}
}

// Scan advances to the next token. Returns true when a token is available via
// Bytes/Text; returns false at EOF or on error (check Err).
func (r *recordReader) Scan() bool {
	if r.err != nil {
		return false
	}
	for {
		if r.br == nil {
			// nil reader: nothing to produce.
			return false
		}
		// Try to split from the current window.
		advance, token, err := r.split(r.buf[r.start:r.end], r.done)
		if err != nil {
			r.err = err
			return false
		}
		if advance > 0 {
			r.token = token
			r.start += advance
			// Reclaim buffer space when fully consumed.
			if r.start == r.end {
				r.start = 0
				r.end = 0
			}
			return true
		}
		// advance == 0: split needs more data (or at EOF with nothing left).
		if r.done {
			// EOF already seen and split produced nothing: we are done.
			return false
		}
		if !r.refill() {
			// refill failed — either EOF (which sets done and retries split
			// above) or a real error.
			if r.err != nil {
				return false
			}
			// EOF path: loop once more so split can emit a final token.
			continue
		}
	}
}

// refill compacts unconsumed bytes to the buffer head, grows if needed, and
// reads more data. Returns true if more data was appended (or EOF was reached
// with bytes available); returns false only on a non-EOF read error.
func (r *recordReader) refill() bool {
	// Only compact if we're running out of space at the end. If there's plenty
	// of room to read more data, skip the compaction to avoid O(n) copies.
	spaceAtEnd := len(r.buf) - r.end
	if spaceAtEnd < 4096 && r.start > 0 {
		// Compact: move [start, end) to the front.
		n := copy(r.buf, r.buf[r.start:r.end])
		r.end = n
		r.start = 0
	}
	// Ensure there is room to read at least one byte. If the buffer is full
	// (end == len(buf)), grow geometrically.
	if r.end == len(r.buf) {
		newCap := len(r.buf) * 2
		if newCap < r.end+4096 {
			newCap = r.end + 4096
		}
		newBuf := make([]byte, newCap)
		copy(newBuf, r.buf[:r.end])
		r.buf = newBuf
	}
	n, err := r.br.Read(r.buf[r.end:])
	r.end += n
	if err != nil {
		if err == io.EOF {
			r.done = true
			// EOF is not an error; the caller's next split invocation will
			// see atEOF=true and can emit a final token.
			return n > 0
		}
		r.err = err
		return false
	}
	return true
}

// Bytes returns the most recent token as a byte slice. The slice is valid
// until the next call to Scan.
func (r *recordReader) Bytes() []byte {
	return r.token
}

// Text returns the most recent token as a string.
func (r *recordReader) Text() string {
	return string(r.token)
}

// Err returns the first non-EOF error encountered. Returns nil at clean EOF.
func (r *recordReader) Err() error {
	return r.err
}

// scanLines is a bufio.SplitFunc that splits on '\n', stripping a trailing '\r'
// (matching bufio.ScanLines semantics). At EOF it returns any remaining
// non-empty tail.
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// We have a full line.
		line := data[:i]
		line = dropCR(line)
		return i + 1, line, nil
	}
	// No newline found.
	if atEOF {
		// Return whatever is left (drop trailing CR if present).
		line := dropCR(data)
		return len(data), line, nil
	}
	// Request more data.
	return 0, nil, nil
}

// dropCR removes a trailing '\r' from data.
func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[:len(data)-1]
	}
	return data
}
