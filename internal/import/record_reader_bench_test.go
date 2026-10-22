package _import

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// lenBytesOf is a helper for tests that need to know the byte length of a
// formatted string without allocating the final result separately.
func lenBytesOf(s string) int { return len(s) }

// generateLines produces n lines each of approximately lineSize bytes
// (including the trailing newline).
func generateLines(n, lineSize int) string {
	var sb strings.Builder
	sb.Grow(n * lineSize)
	for i := 0; i < n; i++ {
		payload := strings.Repeat("x", lineSize-1)
		sb.WriteString(payload)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func BenchmarkScannerLines(b *testing.B) {
	// 100 lines × 100 KB each = 10 MB total, representative of a medium export.
	const lineSize = 100 * 1024
	const numLines = 100
	data := generateLines(numLines, lineSize)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scanner := bufio.NewScanner(strings.NewReader(data))
		scanner.Buffer(make([]byte, 0, 1024*1024), 10<<20)
		count := 0
		for scanner.Scan() {
			_ = scanner.Bytes()
			count++
		}
		if scanner.Err() != nil {
			b.Fatalf("scanner error: %v", scanner.Err())
		}
		if count != numLines {
			b.Fatalf("expected %d lines, got %d", numLines, count)
		}
	}
}

func BenchmarkRecordReaderLines(b *testing.B) {
	const lineSize = 100 * 1024
	const numLines = 100
	data := generateLines(numLines, lineSize)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rd := newRecordReader(bufio.NewReader(strings.NewReader(data)), scanLines)
		count := 0
		for rd.Scan() {
			_ = rd.Bytes()
			count++
		}
		if rd.Err() != nil {
			b.Fatalf("recordReader error: %v", rd.Err())
		}
		if count != numLines {
			b.Fatalf("expected %d lines, got %d", numLines, count)
		}
	}
}

func BenchmarkRecordReaderLargeLine(b *testing.B) {
	// 8 MB single record — the scenario that motivated this reader.
	const lineSize = 8 << 20
	data := fmt.Sprintf("%s\n", strings.Repeat("y", lineSize-1))

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rd := newRecordReader(bufio.NewReader(strings.NewReader(data)), scanLines)
		if !rd.Scan() {
			b.Fatal("expected one token")
		}
		tok := rd.Bytes()
		if len(tok) != lineSize-1 {
			b.Fatalf("token length: got %d, want %d", len(tok), lineSize-1)
		}
		if rd.Scan() {
			b.Fatal("expected exactly one token")
		}
		if rd.Err() != nil {
			b.Fatalf("unexpected error: %v", rd.Err())
		}
	}
}

// Ensure lenBytesOf is used so the compiler doesn't drop it.
var _ = lenBytesOf

// Ensure bytes import is used.
var _ = bytes.Equal
