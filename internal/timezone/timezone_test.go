package timezone

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	for _, name := range []string{"UTC", "America/New_York", "Asia/Tokyo"} {
		loc, err := Load(name)
		if err != nil || loc.String() != name {
			t.Errorf("Load(%q) = %v, %v", name, loc, err)
		}
		again, _ := Load(name)
		if again != loc {
			t.Errorf("Load(%q) isn't cached", name)
		}
	}
	for _, name := range []string{"", "Local", "Mars/Olympus_Mons", "../etc/passwd", "/etc/localtime", "America/"} {
		if loc, err := Load(name); err == nil {
			t.Errorf("Load(%q) = %v, want an error", name, loc)
		}
	}
}

// link makes a localtime link in a temp dir pointing at target, which
// need not exist.
func link(t *testing.T, target string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "localtime")
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMachine(t *testing.T) {
	for target, want := range map[string]string{
		"/var/db/timezone/zoneinfo/America/New_York": "America/New_York", // a Mac
		"/usr/share/zoneinfo/Europe/Berlin":          "Europe/Berlin",
		"../usr/share/zoneinfo/Asia/Tokyo":           "Asia/Tokyo", // relative
		"/usr/share/zoneinfo/posix/Australia/Sydney": "Australia/Sydney",
		"/usr/share/zoneinfo/UTC":                    "UTC",
	} {
		z := NewMachine(link(t, target)).Zone()
		if z.Name != want || z.Loc.String() != want {
			t.Errorf("%s: zone %q (%v), want %s", target, z.Name, z.Loc, want)
		}
	}
	// A link that names no zone, or no link: Go's local zone, named.
	for _, p := range []string{link(t, "/usr/share/zoneinfo/Mars/Olympus_Mons"), link(t, "/nowhere/localtime"), filepath.Join(t.TempDir(), "missing")} {
		z := NewMachine(p).Zone()
		if z.Loc != time.Local || z.Name == "" || z.Name == "Local" {
			t.Errorf("%s: zone %q (%v), want time.Local by a name", p, z.Name, z.Loc)
		}
	}
}

// The link is read again on each call: a change shows at once.
func TestMachineFollowsTheLink(t *testing.T) {
	p := link(t, "/usr/share/zoneinfo/America/New_York")
	m := NewMachine(p)
	if z := m.Zone(); z.Name != "America/New_York" {
		t.Fatalf("zone %q", z.Name)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/share/zoneinfo/Asia/Tokyo", p); err != nil {
		t.Fatal(err)
	}
	if z := m.Zone(); z.Name != "Asia/Tokyo" {
		t.Errorf("after the change: zone %q", z.Name)
	}
}

func TestCommonZonesLoad(t *testing.T) {
	for _, name := range Common {
		if _, err := Load(name); err != nil {
			t.Errorf("common zone %q doesn't load: %v", name, err)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(Common)))) != len(Common) {
		t.Error("a common zone is listed twice")
	}
}

func TestAll(t *testing.T) {
	dir := t.TempDir()
	tab := "# tzdb timezone descriptions\n#\n" +
		"US\t+404251-0740023\tAmerica/New_York\tEastern (most areas)\n" +
		"JP\t+353916+1394441\tAsia/Tokyo\n" +
		"XX\t+0000+00000\tMars/Olympus_Mons\tno such zone\n" +
		"DE,DK\t+5230+01322\tEurope/Berlin\n\n"
	if err := os.WriteFile(filepath.Join(dir, "zone1970.tab"), []byte(tab), 0o644); err != nil {
		t.Fatal(err)
	}
	got := allIn([]string{filepath.Join(dir, "missing"), dir})
	if want := []string{"America/New_York", "Asia/Tokyo", "Europe/Berlin", "UTC"}; !slices.Equal(got, want) {
		t.Errorf("allIn = %v, want %v", got, want)
	}
	if got := allIn([]string{t.TempDir()}); got != nil {
		t.Errorf("no table: %v", got)
	}
	// This machine's own list, when it has one.
	if all := All(); all != nil {
		for _, name := range []string{"America/New_York", "Europe/London", "UTC"} {
			if !slices.Contains(all, name) {
				t.Errorf("All() lacks %s (%d zones)", name, len(all))
			}
		}
		if !slices.IsSorted(all) || slices.ContainsFunc(all, func(n string) bool { return strings.HasPrefix(n, "#") }) {
			t.Error("All() isn't a sorted list of names")
		}
	}
}
