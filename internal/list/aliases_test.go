package list

import "testing"

func TestObjectAliasesAreUnique(t *testing.T) {
	seen := make(map[string]string)
	for _, object := range Objects() {
		aliases := Aliases(object)
		if len(aliases) != 1 {
			t.Errorf("%s has %d aliases, want exactly one", object, len(aliases))
		}
		for _, alias := range aliases {
			if alias == "" {
				t.Errorf("%s has an empty alias", object)
			}
			if len(alias) != 3 {
				t.Errorf("%s alias %q is not three letters", object, alias)
			}
			for _, name := range Objects() {
				if alias == name {
					t.Errorf("%s alias %q collides with object name", object, alias)
				}
			}
			if prev, ok := seen[alias]; ok {
				t.Errorf("alias %q used by both %s and %s", alias, prev, object)
			}
			seen[alias] = object
		}
	}
}

func TestAliasesReturnsCopy(t *testing.T) {
	got := Aliases(ObjectDatabases)
	if len(got) == 0 {
		t.Fatal("databases has no aliases")
	}
	got[0] = "broken"
	if Aliases(ObjectDatabases)[0] == "broken" {
		t.Error("Aliases exposed the registry slice")
	}
}
