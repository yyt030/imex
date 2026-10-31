package list

import (
	"bytes"
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
