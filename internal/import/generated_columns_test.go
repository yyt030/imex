package _import

import (
	"reflect"
	"testing"
)

func TestSplitGeneratedColumns(t *testing.T) {
	cases := []struct {
		name       string
		cols       []string
		projection []int
		generated  map[string]bool
		wantKept   []string
		wantProj   []int
	}{
		{
			name:     "no generated column keeps the list untouched",
			cols:     []string{"id", "v"},
			wantKept: []string{"id", "v"},
			wantProj: nil,
		},
		{
			name:      "generated columns at the end drop their tail positions",
			cols:      []string{"id", "v", "gv", "gs"},
			generated: map[string]bool{"GV": true, "GS": true},
			wantKept:  []string{"id", "v"},
			wantProj:  []int{0, 1},
		},
		{
			name:      "a generated column in the middle keeps later positions aligned",
			cols:      []string{"id", "gv", "v"},
			generated: map[string]bool{"GV": true},
			wantKept:  []string{"id", "v"},
			wantProj:  []int{0, 2},
		},
		{
			name:      "names are matched case-insensitively and trimmed",
			cols:      []string{" ID ", "V"},
			generated: map[string]bool{"ID": true},
			wantKept:  []string{"V"},
			wantProj:  []int{1},
		},
		{
			name:      "generated columns outside the file change nothing",
			cols:      []string{"id", "v"},
			generated: map[string]bool{"GS": true},
			wantKept:  []string{"id", "v"},
			wantProj:  nil,
		},
		{
			name:       "a mapping projection survives the generated filter",
			cols:       []string{"name", "id", "gv"},
			projection: []int{1, 2, 0},
			generated:  map[string]bool{"GV": true},
			wantKept:   []string{"name", "id"},
			wantProj:   []int{1, 2},
		},
		{
			name:       "a subset mapping projection composes with the filter",
			cols:       []string{"t_name", "gv"},
			projection: []int{2, 1},
			generated:  map[string]bool{"GV": true},
			wantKept:   []string{"t_name"},
			wantProj:   []int{2},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, proj := splitGeneratedColumns(c.cols, c.projection, c.generated)
			if !reflect.DeepEqual(kept, c.wantKept) {
				t.Fatalf("kept = %v, want %v", kept, c.wantKept)
			}
			if !reflect.DeepEqual(proj, c.wantProj) {
				t.Fatalf("projection = %v, want %v", proj, c.wantProj)
			}
		})
	}
}

func TestAppendRowValues(t *testing.T) {
	cases := []struct {
		name       string
		row        []interface{}
		projection []int
		want       []interface{}
	}{
		{
			name: "nil projection appends the whole row",
			row:  []interface{}{1, "a", 2},
			want: []interface{}{1, "a", 2},
		},
		{
			name:       "projection drops the generated values at the end",
			row:        []interface{}{1, 10, 20, 21},
			projection: []int{0, 1},
			want:       []interface{}{1, 10},
		},
		{
			name:       "projection skips a value in the middle",
			row:        []interface{}{1, 20, 10},
			projection: []int{0, 2},
			want:       []interface{}{1, 10},
		},
		{
			name:       "projection reorders a mapped subset",
			row:        []interface{}{"one", 1, "note"},
			projection: []int{1, 0},
			want:       []interface{}{1, "one"},
		},
		{
			name:       "a short row never reads past its end",
			row:        []interface{}{1},
			projection: []int{0, 1},
			want:       []interface{}{1},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := appendRowValues(nil, c.row, c.projection)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("appendRowValues = %v, want %v", got, c.want)
			}
		})
	}
}
