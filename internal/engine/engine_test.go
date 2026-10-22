package engine

import "testing"

func TestParseNamesAndIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   ID
	}{{" Claude ", Claude}, {"cc", Claude}, {"CODEX", Codex}, {"cx", Codex}, {"opencode", OpenCode}} {
		got, err := Parse(tc.name)
		if err != nil || got != tc.id {
			t.Fatalf("Parse(%q)=%s,%v", tc.name, got, err)
		}
	}
	if _, err := Parse(""); err == nil {
		t.Fatal("empty engine accepted")
	}
}
