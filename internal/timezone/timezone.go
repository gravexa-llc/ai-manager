// Package timezone names and loads the time zones the dashboard shows
// clock times in: the machine's own zone, which is the Time zone setting's
// System, and any IANA zone ("America/New_York") picked instead.
//
// The machine's zone is read from /etc/localtime, not from TZ: aimd runs
// under launchd, where TZ is unset, and the link names the zone System
// Settings › Date & Time sets. Go's time.Local reads the same file when TZ
// is unset, but names it only "Local"; the link's target names it.
package timezone

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Zone is a time zone and the name to show for it.
type Zone struct {
	// Name is the IANA name ("America/New_York", "UTC"), or, for a machine
	// zone without one, what Go calls it.
	Name string
	Loc  *time.Location
}

// UTC is the zone of that name.
var UTC = Zone{Name: "UTC", Loc: time.UTC}

// ErrName is returned for a name that isn't a zone this machine can load.
var ErrName = errors.New("timezone: not a time zone")

// loaded caches Load's answers that worked: time.LoadLocation reads the
// zone's file on every call, and a page asks for the zone several times.
var loaded sync.Map // name → *time.Location

// Load returns the zone with an IANA name, such as "America/New_York" or
// "UTC". "" and "Local" are refused, though time.LoadLocation takes them
// (as UTC and as the process' zone): a stored setting names its zone.
func Load(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, ErrName
	}
	if loc, ok := loaded.Load(name); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, ErrName
	}
	loaded.Store(name, loc)
	return loc, nil
}

// Machine is the machine's time zone, read from a link like
// /etc/localtime → /var/db/timezone/zoneinfo/Europe/Berlin. It reads
// the link again on each call, so a change in System Settings (or a laptop
// that sets its zone from its location) shows at the next page, and it
// loads the zone again only when the link changes.
type Machine struct {
	path string

	mu     sync.Mutex
	target string
	zone   Zone
}

// NewMachine reads the zone from the link at path: /etc/localtime on a
// Mac.
func NewMachine(path string) *Machine {
	return &Machine{path: path}
}

// Zone is the machine's zone. When the link can't be read or doesn't name
// a zone this machine can load, it is Go's local zone (time.Local).
func (m *Machine) Zone() Zone {
	target, err := os.Readlink(m.path)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil && target == m.target && m.zone.Loc != nil {
		return m.zone
	}
	m.target, m.zone = target, local()
	if name := linkZone(target); err == nil && name != "" {
		if loc, err := Load(name); err == nil {
			m.zone = Zone{Name: name, Loc: loc}
		}
	}
	return m.zone
}

// linkZone is the zone a localtime link's target names: what follows its
// zoneinfo folder, without the posix/ or right/ variants' folder; "" when
// the target isn't in a zoneinfo folder.
func linkZone(target string) string {
	_, name, ok := strings.Cut(filepath.ToSlash(target), "/zoneinfo/")
	if !ok {
		return ""
	}
	for _, variant := range []string{"posix/", "right/"} {
		name = strings.TrimPrefix(name, variant)
	}
	return name
}

// local is Go's local zone, named: time.Local's name, or, when that is
// only "Local", its abbreviation now ("PST").
func local() Zone {
	name := time.Local.String()
	if name == "Local" {
		name, _ = time.Now().In(time.Local).Zone()
	}
	return Zone{Name: name, Loc: time.Local}
}

// Common are the zones the Settings page lists first, west to east.
var Common = []string{
	"Pacific/Honolulu", "America/Anchorage", "America/Los_Angeles", "America/Denver", "America/Phoenix",
	"America/Chicago", "America/New_York", "America/Halifax", "America/Sao_Paulo",
	"UTC", "Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin",
	"Europe/Athens", "Africa/Johannesburg", "Europe/Moscow", "Asia/Dubai", "Asia/Kolkata",
	"Asia/Singapore", "Asia/Shanghai", "Asia/Tokyo", "Australia/Sydney", "Pacific/Auckland",
}

// zoneDirs are where the tz database's tables are looked for, as Go's
// time package looks for its zone files.
var zoneDirs = []string{"/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/etc/zoneinfo"}

var (
	allOnce  sync.Once
	allZones []string
)

// All lists every zone the machine's tz database names in its zone.tab
// (zone1970.tab when there is none), and UTC, sorted: each one loads. It
// is nil when no table is found.
func All() []string {
	allOnce.Do(func() { allZones = allIn(zoneDirs) })
	return allZones
}

// allIn is All, reading the first table found in dirs.
func allIn(dirs []string) []string {
	for _, dir := range dirs {
		for _, file := range []string{"zone.tab", "zone1970.tab"} {
			f, err := os.Open(filepath.Join(dir, file))
			if err != nil {
				continue
			}
			names := parseTab(f)
			f.Close()
			if len(names) == 0 {
				continue
			}
			names = slices.DeleteFunc(append(names, "UTC"), func(n string) bool {
				_, err := Load(n)
				return err != nil
			})
			slices.Sort(names)
			return slices.Compact(names)
		}
	}
	return nil
}

// parseTab reads the zone names in a zone.tab or zone1970.tab: the third
// tab-separated field of each line that isn't a comment.
func parseTab(r io.Reader) []string {
	var names []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if f := strings.Split(line, "\t"); len(f) >= 3 && f[2] != "" {
			names = append(names, f[2])
		}
	}
	return names
}
