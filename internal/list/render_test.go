package list

import (
	"bytes"
	"strings"
	"testing"
)

func ptr(s string) *string { return &s }

func TestRenderTableAlignsAndEscapes(t *testing.T) {
	var out bytes.Buffer
	rows := [][]*string{
		{ptr("a"), ptr("long value"), nil},
		{ptr("bb"), ptr("x"), ptr("tab\there")},
	}
	if err := Render(&out, []string{"k", "value", "note"}, true, rows); err != nil {
		t.Fatal(err)
	}
	want := "k   value       note\n" +
		"a   long value  NULL\n" +
		"bb  x           tab\\there\n"
	if out.String() != want {
		t.Fatalf("table output = %q, want %q", out.String(), want)
	}
}

func TestRenderTableWithoutHeaderOrRows(t *testing.T) {
	var out bytes.Buffer
	if err := Render(&out, []string{"k"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("empty table output = %q", out.String())
	}
}

func TestRenderTableWithoutHeader(t *testing.T) {
	var out bytes.Buffer
	if err := Render(&out, []string{"one"}, false, [][]*string{{ptr("x")}}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "x\n" {
		t.Fatalf("output = %q", out.String())
	}
}

// A carriage return in the data must come out as the two visible characters \r
// and never as a raw CR: otherwise one logical row would span two physical
// lines, and the width pass would have measured the wrong text.
func TestRenderTableEscapesCarriageReturn(t *testing.T) {
	var out bytes.Buffer
	rows := [][]*string{
		{ptr("a"), ptr("plain")},
		{ptr("bb"), ptr("cr\rhere")},
	}
	if err := Render(&out, []string{"k", "value"}, true, rows); err != nil {
		t.Fatal(err)
	}
	want := "k   value\n" +
		"a   plain\n" +
		"bb  cr\\rhere\n"
	if out.String() != want {
		t.Fatalf("table output = %q, want %q", out.String(), want)
	}
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("physical lines = %d, want 3 (header plus two data rows)", n)
	}
	if strings.ContainsRune(out.String(), '\r') {
		t.Fatalf("output still contains a raw carriage return: %q", out.String())
	}
}

// Line feeds follow the same rule, and a backslash already in the data is
// doubled while the one the escape introduces is not: the input `a\b` + LF must
// come out as a\\b\nc, which pins both halves of that rule in one cell.
func TestRenderTableEscapesLineFeed(t *testing.T) {
	var out bytes.Buffer
	rows := [][]*string{
		{ptr("x"), ptr("a\\b\nc")},
		{ptr("yy"), ptr("plain")},
	}
	if err := Render(&out, []string{"k", "v"}, true, rows); err != nil {
		t.Fatal(err)
	}
	want := "k   v\n" +
		"x   a\\\\b\\nc\n" +
		"yy  plain\n"
	if out.String() != want {
		t.Fatalf("table output = %q, want %q", out.String(), want)
	}
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("physical lines = %d, want 3 (header plus two data rows)", n)
	}
	if strings.Count(out.String(), "a\\\\b") != 1 {
		t.Fatalf("a data backslash must be doubled, got %q", out.String())
	}
}
