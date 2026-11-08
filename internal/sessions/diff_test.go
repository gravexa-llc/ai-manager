package sessions

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// summary renders events compactly for comparison.
func summary(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = string(e.Kind) + " " + string(e.Key)
		for j, f := range e.Fields {
			if j == 0 {
				out[i] += " "
			} else {
				out[i] += ","
			}
			out[i] += f
		}
	}
	return out
}

func TestDiffFixtures(t *testing.T) {
	at2115 := parseFixture(t, "active-2026-11-05T2115.json")
	at2117 := parseFixture(t, "active-with-claude-p-2026-11-05T2117.json")
	at2127 := parseFixture(t, "active-2026-11-05T2127.json")
	all2115 := parseFixture(t, "all-2026-11-05T2115.json")
	cwd := parseFixture(t, "cwd-dogwood.json")

	tests := []struct {
		name       string
		prev, next Snapshot
		want       []string
	}{
		{"same listing", at2115, at2115, nil},
		{"stable over the window", at2115, at2127, nil},
		// A claude -p run shows up as an interactive session...
		{"claude -p appears", at2115, at2117, []string{"appeared pid:48169"}},
		// ...and is gone by the next listing.
		{"claude -p gone", at2117, at2127, []string{"gone pid:48169"}},
		// --all adds the finished job; dropping --all drops it again,
		// which is why the poller sticks to one mode.
		{"--all adds a finished job", at2115, all2115, []string{"appeared job:b845f48c"}},
		{"leaving --all", all2115, at2115, []string{"gone job:b845f48c"}},
		{"to a --cwd listing", at2115, cwd, []string{
			"gone job:022c44bf", "gone job:47bedaa8",
			"gone pid:47792", "gone pid:24996", "gone pid:79552", "gone pid:36430", "gone pid:37975",
			"gone pid:74245", "gone pid:60441", "gone pid:76780", "gone pid:69087",
			"gone job:199dc94c", "gone pid:91470",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summary(Diff(tt.prev, tt.next))
			if !slices.Equal(got, tt.want) {
				t.Errorf("Diff:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestDiffFromEmpty(t *testing.T) {
	next := parseFixture(t, "synthetic-all-values.json")
	next.At = time.UnixMilli(1793900010000)
	events := Diff(Snapshot{}, next)
	if len(events) != len(next.Sessions) {
		t.Fatalf("got %d events, want %d", len(events), len(next.Sessions))
	}
	for i, e := range events {
		s := next.Sessions[i]
		if e.Kind != Appeared || e.Key != s.Key() || e.After.Key() != s.Key() || !e.At.Equal(next.At) {
			t.Errorf("event %d: %+v", i, e)
		}
		if e.Before.Key() == e.Key || !e.VerdictChanged() {
			t.Errorf("event %d: Before should be zero and the verdict new", i)
		}
	}

	gone := Diff(next, Snapshot{})
	if len(gone) != len(next.Sessions) || gone[0].Kind != Gone || gone[0].After.Key() == gone[0].Key {
		t.Errorf("all gone: %q", summary(gone))
	}
}

// mutate returns a copy of the fixture with its sessions passed through f.
func mutate(t *testing.T, file string, f func(*Session)) Snapshot {
	t.Helper()
	snap := parseFixture(t, file)
	for i := range snap.Sessions {
		f(&snap.Sessions[i])
	}
	return snap
}

func TestDiffChanges(t *testing.T) {
	base := parseFixture(t, "active-2026-11-05T2115.json")

	tests := []struct {
		name    string
		f       func(*Session)
		want    []string
		verdict bool // the verdict changes too
	}{
		{
			// A respawned job keeps its id; its pid and sessionId change.
			name: "background respawn",
			f: func(s *Session) {
				if s.JobID == "199dc94c" {
					s.PID = 70001
					s.SessionID = "1adba5b1-0000-4000-8000-00000000beef"
				}
			},
			want: []string{"changed job:199dc94c pid,sessionId"},
		},
		{
			// A job with no process starts one: same key.
			name: "blocked job gets a process",
			f: func(s *Session) {
				if s.JobID == "022c44bf" {
					s.PID = 70002
					s.Status = StatusBusy
					s.State = JobWorking
				}
			},
			want:    []string{"changed job:022c44bf pid,status,state"},
			verdict: true,
		},
		{
			// /clear in an interactive session: same process, new sessionId.
			name: "interactive sessionId changes",
			f: func(s *Session) {
				if s.PID == 91470 {
					s.SessionID = "a394c94b-0000-4000-8000-00000000c1ea"
				}
			},
			want: []string{"changed pid:91470 sessionId"},
		},
		{
			name: "permission prompt",
			f: func(s *Session) {
				if s.PID == 24996 {
					s.Status = StatusWaiting
					s.WaitingFor = WaitingPermission
				}
			},
			want:    []string{"changed pid:24996 status,waitingFor"},
			verdict: true,
		},
		{
			name: "renamed",
			f: func(s *Session) {
				if s.PID == 47792 {
					s.Name = "session-4b"
				}
			},
			want: []string{"changed pid:47792 name"},
		},
		{
			name: "job finishes",
			f: func(s *Session) {
				if s.JobID == "09a1a428" {
					s.PID = 0
					s.Status = ""
				}
			},
			want: []string{"changed job:09a1a428 pid,status"},
		},
		{
			name: "unknown field appears",
			f: func(s *Session) {
				if s.PID == 79552 {
					s.Extra = map[string]json.RawMessage{"entrypoint": json.RawMessage(`"cli"`)}
				}
			},
			want: []string{"changed pid:79552 entrypoint"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := mutate(t, "active-2026-11-05T2115.json", tt.f)
			events := Diff(base, next)
			if got := summary(events); !slices.Equal(got, tt.want) {
				t.Fatalf("Diff:\n got %q\nwant %q", got, tt.want)
			}
			e := events[0]
			if e.Before.Key() != e.Key || e.After.Key() != e.Key {
				t.Errorf("Before/After keys %s/%s, want %s", e.Before.Key(), e.After.Key(), e.Key)
			}
			if e.VerdictChanged() != tt.verdict {
				t.Errorf("VerdictChanged() = %v, want %v (%v → %v)", e.VerdictChanged(), tt.verdict, e.Before.Verdict(), e.After.Verdict())
			}
		})
	}
}

// Keying on sessionId would turn a respawn into gone + appeared.
func TestDiffKeysJobsOnID(t *testing.T) {
	prev := Snapshot{Sessions: []Session{{JobID: "j1", SessionID: "s1", Kind: Background, State: JobWorking}}}
	next := Snapshot{Sessions: []Session{{JobID: "j1", SessionID: "s2", Kind: Background, State: JobWorking}}}
	if got := summary(Diff(prev, next)); !slices.Equal(got, []string{"changed job:j1 sessionId"}) {
		t.Errorf("got %q", got)
	}
}

func TestDiffOrderAndDuplicates(t *testing.T) {
	a := Session{PID: 1, Kind: Interactive, Status: StatusIdle}
	b := Session{PID: 2, Kind: Interactive, Status: StatusIdle}
	c := Session{PID: 3, Kind: Interactive, Status: StatusIdle}
	d := Session{PID: 4, Kind: Interactive, Status: StatusIdle}
	bBusy := b
	bBusy.Status = StatusBusy

	prev := Snapshot{Sessions: []Session{a, b, c, c}}
	next := Snapshot{Sessions: []Session{d, bBusy, d}}
	want := []string{"appeared pid:4", "changed pid:2 status", "gone pid:1", "gone pid:3"}
	if got := summary(Diff(prev, next)); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
