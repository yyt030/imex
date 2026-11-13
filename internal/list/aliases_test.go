package list

import "testing"

func TestListShortNamesAreUnique(t *testing.T) {
	seen := make(map[string]string)
	for _, object := range Objects() {
		short := ShortName(object)
		if short == "" {
			t.Errorf("%s has an empty short name", object)
		}
		if len(short) != 3 {
			t.Errorf("%s short name %q is not three letters", object, short)
		}
		for _, name := range Objects() {
			if short == name {
				t.Errorf("%s short name %q collides with object name %q", object, short, name)
			}
		}
		if prev, ok := seen[short]; ok {
			t.Errorf("short name %q used by both %s and %s", short, prev, object)
		}
		seen[short] = object
	}
}

func TestShortNamesFollowObjectOrder(t *testing.T) {
	objs := Objects()
	shorts := ShortNames()
	if len(shorts) != len(objs) {
		t.Fatalf("ShortNames = %d, want %d", len(shorts), len(objs))
	}
	for i, o := range objs {
		if shorts[i] != ShortName(o) {
			t.Errorf("ShortNames[%d] = %q, want %q (for %s)", i, shorts[i], ShortName(o), o)
		}
	}
}

func TestShortNameFallsBackToObject(t *testing.T) {
	if got := ShortName("nope"); got != "nope" {
		t.Errorf("unknown object short = %q, want the object itself", got)
	}
}
