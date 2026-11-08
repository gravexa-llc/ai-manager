package plans

import (
	"bytes"
)

// Detection is Detect's verdict on one markdown file.
type Detection struct {
	// IsPlan is true when the file has at least one package table (a package
	// column plus a Needs column) and at least MinPackages package IDs in
	// all.
	IsPlan   bool
	Title    string // as Plan.Title
	Packages int    // number of packages (unique IDs)
	Lanes    int    // number of lanes; 0 when the file has no packages
}

// MinPackages is how many package IDs a file needs to count as a plan.
const MinPackages = 2

// Detect reports whether a markdown file looks like a dispatch plan, with a
// short summary for the band. It's cheap enough to run on every .md write:
// a file without a pipe character can't hold a table, so only its title is
// read; anything else costs one Parse, which is a single pass over the
// lines (about 0.2 ms for a 64 KB plan on an M2).
func Detect(src []byte) Detection {
	if bytes.IndexByte(src, '|') < 0 {
		return Detection{Title: quickTitle(src)}
	}
	p := Parse(src)
	d := Detection{
		IsPlan:   len(p.Packages) >= MinPackages,
		Title:    p.Title,
		Packages: len(p.Packages),
	}
	if d.Packages > 0 {
		d.Lanes = len(p.Lanes)
	}
	return d
}

// quickTitle returns the title without looking for tables, stopping at the
// first level-1 heading. It follows the same rules as titleOf.
func quickTitle(src []byte) string {
	first := ""
	r := newLineReader(src)
	for {
		l, ok := r.next()
		if !ok {
			return first
		}
		if l.kind != lineHeading || l.head == "" {
			continue
		}
		if l.level == 1 {
			return l.head
		}
		if first == "" {
			first = l.head
		}
	}
}
