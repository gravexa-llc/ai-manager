package config

import (
	"slices"
	"strings"
	"testing"
)

func TestPruneRebuildable(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
		errPart    string
	}{
		{"unset", "", DefaultRebuildable, ""},
		{"table without the key", "[prune]\n", DefaultRebuildable, ""},
		{"replaced", "[prune]\nrebuildable = [\"node_modules/\", \"/out/\", \"**/gen/*.pb.go\"]\n",
			[]string{"node_modules/", "/out/", "**/gen/*.pb.go"}, ""},
		{"empty list means none", "[prune]\nrebuildable = []\n", []string{}, ""},
		{"empty entry", "[prune]\nrebuildable = [\"/\"]\n", nil, "empty entry"},
		{"dot-dot", "[prune]\nrebuildable = [\"../x/\"]\n", nil, ".. component"},
		{"double slash", "[prune]\nrebuildable = [\"a//b\"]\n", nil, "empty, . or .."},
		{"bad pattern", "[prune]\nrebuildable = [\"[x\"]\n", nil, "syntax error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, aimHome := setup(t)
			if tc.body != "" {
				writeConfig(t, aimHome, tc.body)
			}
			cfg, err := Load(aimHome)
			if err != nil {
				t.Fatal(err)
			}
			got, err := cfg.Prune.RebuildablePaths()
			if tc.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("RebuildablePaths() error = %v, want one containing %q", err, tc.errPart)
				}
				return
			}
			if err != nil || got == nil || !slices.Equal(got, tc.want) {
				t.Fatalf("RebuildablePaths() = %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestPruneUnknownKey(t *testing.T) {
	_, aimHome := setup(t)
	writeConfig(t, aimHome, "[prune]\nrebuildible = [\"x/\"]\n")
	if _, err := Load(aimHome); err == nil || !strings.Contains(err.Error(), "prune.rebuildible") {
		t.Errorf("Load() with a misspelt key = %v", err)
	}
}

func TestPruneDefaultsNotShared(t *testing.T) {
	got, _ := Prune{}.RebuildablePaths()
	got[0] = "changed"
	if DefaultRebuildable[0] == "changed" {
		t.Error("RebuildablePaths returned DefaultRebuildable itself")
	}
}
