package _import

import (
	"reflect"
	"testing"
)

func TestSplitGeneratedColumns(t *testing.T) {
	cases := []struct {
		name      string
		cols      []string
		generated map[string]bool
		wantKept  []string
		wantDrop  []bool
	}{
		{
			name:     "no generated column keeps the list untouched",
			cols:     []string{"id", "v"},
			wantKept: []string{"id", "v"},
			wantDrop: nil,
		},
		{
			name:      "generated columns at the end drop their tail positions",
			cols:      []string{"id", "v", "gv", "gs"},
			generated: map[string]bool{"GV": true, "GS": true},
			wantKept:  []string{"id", "v"},
			wantDrop:  []bool{false, false, true, true},
		},
		{
			name:      "a generated column in the middle keeps later positions aligned",
			cols:      []string{"id", "gv", "v"},
			generated: map[string]bool{"GV": true},
			wantKept:  []string{"id", "v"},
			wantDrop:  []bool{false, true, false},
		},
		{
			name:      "names are matched case-insensitively and trimmed",
			cols:      []string{" ID ", "V"},
			generated: map[string]bool{"ID": true},
			wantKept:  []string{"V"},
			wantDrop:  []bool{true, false},
		},
		{
			name:      "generated columns outside the file change nothing",
			cols:      []string{"id", "v"},
			generated: map[string]bool{"GS": true},
			wantKept:  []string{"id", "v"},
			wantDrop:  nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, drop := splitGeneratedColumns(c.cols, c.generated)
			if !reflect.DeepEqual(kept, c.wantKept) {
				t.Fatalf("kept = %v, want %v", kept, c.wantKept)
			}
			if !reflect.DeepEqual(drop, c.wantDrop) {
				t.Fatalf("drop = %v, want %v", drop, c.wantDrop)
			}
		})
	}
}

func TestAppendRowValues(t *testing.T) {
	cases := []struct {
		name string
		row  []interface{}
		drop []bool
		want []interface{}
	}{
		{
			name: "no mask appends the whole row",
			row:  []interface{}{1, "a", 2},
			want: []interface{}{1, "a", 2},
		},
		{
			name: "mask at the end drops the generated values",
			row:  []interface{}{1, 10, 20, 21},
			drop: []bool{false, false, true, true},
			want: []interface{}{1, 10},
		},
		{
			name: "mask in the middle keeps the following values in order",
			row:  []interface{}{1, 20, 10},
			drop: []bool{false, true, false},
			want: []interface{}{1, 10},
		},
		{
			name: "a short row never reads past the mask",
			row:  []interface{}{1},
			drop: []bool{false, true},
			want: []interface{}{1},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := appendRowValues(nil, c.row, c.drop)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("appendRowValues = %v, want %v", got, c.want)
			}
		})
	}
}
