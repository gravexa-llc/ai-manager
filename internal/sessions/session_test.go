package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const fixtureDir = "../../testdata/agents"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseFixture(t *testing.T, name string) Snapshot {
	t.Helper()
	snap, err := Parse(readFixture(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return snap
}

// Verdicts for every entry in the captured fixtures, by name. The capture
// kept the session-N names consistent across these files.
var capturedVerdicts = map[string]Verdict{
	"session-1":  {StateNeedsYou, NeedReply, ReasonReply}, // background blocked, no process
	"session-2":  {StateNeedsYou, NeedReply, ReasonReply},
	"session-3":  {State: StateTurnDone, Reason: "done"}, // --all only: finished, no process
	"session-4":  {State: StateIdle, Reason: "idle"},
	"session-5":  {State: StateWorking, Reason: "busy"},
	"session-6":  {State: StateIdle, Reason: "idle"},
	"session-7":  {State: StateIdle, Reason: "idle"},
	"session-8":  {State: StateIdle, Reason: "idle"},
	"session-9":  {State: StateIdle, Reason: "idle"},
	"session-10": {State: StateIdle, Reason: "idle"},
	"session-11": {State: StateIdle, Reason: "idle"},
	"session-12": {State: StateIdle, Reason: "idle"},
	"session-13": {State: StateIdle, Reason: "idle"},
	"session-14": {StateNeedsYou, NeedReply, ReasonReply}, // status idle + state blocked
	"session-15": {StateNeedsYou, NeedReply, ReasonReply},
	"session-16": {State: StateTurnDone, Reason: "done"}, // status idle + state done
	"session-17": {State: StateWorking, Reason: "busy"},
	"session-18": {State: StateWorking, Reason: "busy"}, // a transient claude -p
}

func TestParseFixtures(t *testing.T) {
	type want struct {
		key     Key
		kind    Kind
		verdict Verdict
		active  bool
	}
	synthetic := []want{
		{"job:aaaa0001", Background, Verdict{State: StateError, Reason: "failed"}, false},
		{"job:aaaa0002", Background, Verdict{State: StateGone, Reason: "stopped"}, false},
		{"job:aaaa0003", Background, Verdict{State: StateWorking, Reason: "working"}, true},
		// waiting beats blocked: a permission prompt, not a reply.
		{"job:aaaa0004", Background, Verdict{StateNeedsYou, NeedPrompt, WaitingPermission}, true},
		// A background process with no job: keyed and judged by its process.
		{"pid:10005", Background, Verdict{State: StateIdle, Reason: "idle"}, true},
		{"pid:10006", Interactive, Verdict{StateNeedsYou, NeedPrompt, WaitingInput}, true},
		{"pid:10007", Interactive, Verdict{StateNeedsYou, NeedPrompt, WaitingDialog}, true},
		// No sessionId or name.
		{"pid:10008", Interactive, Verdict{State: StateWorking, Reason: "busy"}, true},
	}

	type fixtureCase struct {
		file  string
		count int
		all   bool   // finished jobs may be listed
		want  []want // per entry; nil means look names up in capturedVerdicts
	}
	tests := []fixtureCase{
		{file: "active-2026-11-05T2115.json", count: 16},
		{file: "active-2026-11-05T2127.json", count: 16},
		{file: "active-with-claude-p-2026-11-05T2117.json", count: 17},
		{file: "all-2026-11-05T2115.json", count: 17, all: true},
		{file: "cwd-dogwood.json", count: 3},
		{file: "synthetic-all-values.json", count: 8, all: true, want: synthetic},
	}

	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		covered := slices.ContainsFunc(tests, func(tt fixtureCase) bool { return tt.file == e.Name() })
		if !covered && strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("fixture %s has no test case", e.Name())
		}
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			snap := parseFixture(t, tt.file)
			if len(snap.Warnings) > 0 {
				t.Errorf("warnings: %q", snap.Warnings)
			}
			if got := len(snap.Sessions); got != tt.count {
				t.Fatalf("got %d sessions, want %d", got, tt.count)
			}

			keys := map[Key]bool{}
			var prevStart time.Time
			for i, s := range snap.Sessions {
				k := s.Key()
				if keys[k] {
					t.Errorf("duplicate key %s", k)
				}
				keys[k] = true
				if s.Cwd == "" || s.StartedAt.IsZero() {
					t.Errorf("%s: missing cwd or startedAt: %+v", k, s)
				}
				if s.StartedAt.Before(prevStart) {
					t.Errorf("%s: startedAt out of order", k)
				}
				prevStart = s.StartedAt
				if len(s.Extra) > 0 {
					t.Errorf("%s: unexpected extra fields %v", k, s.Extra)
				}
				if s.Kind == Background && s.JobID == "" && s.PID == 0 {
					t.Errorf("%s: background entry with neither id nor pid", k)
				}
				if !tt.all && !s.Active() {
					t.Errorf("%s: listed without --all but not Active", k)
				}

				if tt.want != nil {
					w := tt.want[i]
					if k != w.key || s.Kind != w.kind {
						t.Errorf("entry %d: key %s kind %s, want %s %s", i, k, s.Kind, w.key, w.kind)
					}
					if v := s.Verdict(); v != w.verdict {
						t.Errorf("%s: verdict %v, want %v", k, v, w.verdict)
					}
					if a := s.Active(); a != w.active {
						t.Errorf("%s: Active() = %v, want %v", k, a, w.active)
					}
					continue
				}
				want, ok := capturedVerdicts[s.Name]
				if !ok {
					t.Errorf("%s: no expected verdict for name %q", k, s.Name)
					continue
				}
				if v := s.Verdict(); v != want {
					t.Errorf("%s (%s): verdict %v, want %v", k, s.Name, v, want)
				}
			}
		})
	}
}

func TestParseFixtureFields(t *testing.T) {
	snap := parseFixture(t, "active-2026-11-05T2115.json")
	byKey := map[Key]Session{}
	for _, s := range snap.Sessions {
		byKey[s.Key()] = s
	}

	// A respawned job: keyed on id, and its sessionId differs.
	job, ok := byKey["job:199dc94c"]
	if !ok {
		t.Fatal("job:199dc94c missing")
	}
	want := Session{
		JobID:     "199dc94c",
		PID:       69310,
		SessionID: "1adba5b1-0bfc-449c-a01b-e881219d9920",
		Name:      "session-14",
		Cwd:       "~/Projects/forms",
		Kind:      Background,
		StartedAt: time.UnixMilli(1793925425727),
		Status:    StatusIdle,
		State:     JobBlocked,
	}
	if !equalSession(job, want) {
		t.Errorf("job:199dc94c = %+v\nwant %+v", job, want)
	}

	// The lead: an interactive session keyed on its pid.
	lead, ok := byKey["pid:91470"]
	if !ok {
		t.Fatal("pid:91470 missing")
	}
	if lead.Kind != Interactive || lead.Status != StatusBusy || lead.JobID != "" || lead.State != "" {
		t.Errorf("pid:91470 = %+v", lead)
	}
}

func equalSession(a, b Session) bool {
	return len(changedFields(a, b)) == 0
}

func TestVerdict(t *testing.T) {
	tests := []struct {
		name string
		s    Session
		want Verdict
	}{
		{"interactive idle", Session{Kind: Interactive, Status: StatusIdle}, Verdict{State: StateIdle, Reason: "idle"}},
		{"interactive busy", Session{Kind: Interactive, Status: StatusBusy}, Verdict{State: StateWorking, Reason: "busy"}},
		{"permission prompt", Session{Kind: Interactive, Status: StatusWaiting, WaitingFor: WaitingPermission}, Verdict{StateNeedsYou, NeedPrompt, WaitingPermission}},
		{"input needed", Session{Kind: Interactive, Status: StatusWaiting, WaitingFor: WaitingInput}, Verdict{StateNeedsYou, NeedPrompt, WaitingInput}},
		{"dialog open", Session{Kind: Interactive, Status: StatusWaiting, WaitingFor: WaitingDialog}, Verdict{StateNeedsYou, NeedPrompt, WaitingDialog}},
		{"sandbox request", Session{Kind: Interactive, Status: StatusWaiting, WaitingFor: WaitingSandbox}, Verdict{StateNeedsYou, NeedPrompt, WaitingSandbox}},
		{"goal proposal", Session{Kind: Interactive, Status: StatusWaiting, WaitingFor: WaitingGoal}, Verdict{StateNeedsYou, NeedPrompt, WaitingGoal}},
		{"unknown waitingFor kept", Session{Kind: Interactive, Status: StatusWaiting, WaitingFor: "plan approval"}, Verdict{StateNeedsYou, NeedPrompt, "plan approval"}},
		{"waiting without reason", Session{Kind: Interactive, Status: StatusWaiting}, Verdict{StateNeedsYou, NeedPrompt, ReasonWaiting}},
		{"blocked, no process: reply", Session{Kind: Background, JobID: "a", State: JobBlocked}, Verdict{StateNeedsYou, NeedReply, ReasonReply}},
		{"blocked, idle process: reply", Session{Kind: Background, JobID: "a", PID: 1, Status: StatusIdle, State: JobBlocked}, Verdict{StateNeedsYou, NeedReply, ReasonReply}},
		{"blocked, waiting process: prompt", Session{Kind: Background, JobID: "a", PID: 1, Status: StatusWaiting, WaitingFor: WaitingSandbox, State: JobBlocked}, Verdict{StateNeedsYou, NeedPrompt, WaitingSandbox}},
		{"working", Session{Kind: Background, JobID: "a", PID: 1, Status: StatusBusy, State: JobWorking}, Verdict{State: StateWorking, Reason: "working"}},
		{"working, no process", Session{Kind: Background, JobID: "a", State: JobWorking}, Verdict{State: StateWorking, Reason: "working"}},
		{"failed", Session{Kind: Background, JobID: "a", State: JobFailed}, Verdict{State: StateError, Reason: "failed"}},
		{"done", Session{Kind: Background, JobID: "a", State: JobDone}, Verdict{State: StateTurnDone, Reason: "done"}},
		{"done, idle process", Session{Kind: Background, JobID: "a", PID: 1, Status: StatusIdle, State: JobDone}, Verdict{State: StateTurnDone, Reason: "done"}},
		{"stopped", Session{Kind: Background, JobID: "a", State: JobStopped}, Verdict{State: StateGone, Reason: "stopped"}},
		{"background process, no job", Session{Kind: Background, PID: 1, Status: StatusIdle}, Verdict{State: StateIdle, Reason: "idle"}},
		{"unknown state", Session{Kind: Background, JobID: "a", State: "paused"}, Verdict{State: StateUnknown, Reason: "paused"}},
		{"unknown state, idle process", Session{Kind: Background, JobID: "a", PID: 1, Status: StatusIdle, State: "paused"}, Verdict{State: StateUnknown, Reason: "paused"}},
		{"unknown state, busy process", Session{Kind: Background, JobID: "a", PID: 1, Status: StatusBusy, State: "paused"}, Verdict{State: StateWorking, Reason: "busy"}},
		{"unknown status", Session{Kind: Interactive, PID: 1, Status: "compacting"}, Verdict{State: StateUnknown, Reason: "compacting"}},
		{"no status or state", Session{Kind: Interactive, PID: 1}, Verdict{State: StateUnknown}},
		{"unknown kind", Session{Kind: "remote", Status: StatusBusy}, Verdict{State: StateWorking, Reason: "busy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.s.Verdict()
			if got != tt.want {
				t.Errorf("Verdict() = %v, want %v", got, tt.want)
			}
			if got.NeedsYou() != (tt.want.State == StateNeedsYou) {
				t.Errorf("NeedsYou() = %v", got.NeedsYou())
			}
			if (got.Need != "") != got.NeedsYou() {
				t.Errorf("Need %q set without needs_you, or missing", got.Need)
			}
		})
	}
}

func TestVerdictString(t *testing.T) {
	for _, tt := range []struct {
		v    Verdict
		want string
	}{
		{Verdict{StateNeedsYou, NeedPrompt, WaitingPermission}, "needs_you (prompt: permission prompt)"},
		{Verdict{StateNeedsYou, NeedReply, ReasonReply}, "needs_you (reply: reply)"},
		{Verdict{State: StateIdle, Reason: "idle"}, "idle (idle)"},
		{Verdict{State: StateUnknown}, "unknown"},
	} {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%#v.String() = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestKey(t *testing.T) {
	started := time.UnixMilli(1793900000000)
	tests := []struct {
		s    Session
		want Key
	}{
		{Session{JobID: "199dc94c", PID: 69310, SessionID: "1adba5b1"}, "job:199dc94c"},
		{Session{JobID: "199dc94c"}, "job:199dc94c"},
		{Session{PID: 91470, SessionID: "a394c94b"}, "pid:91470"},
		{Session{SessionID: "a394c94b"}, "session:a394c94b"},
		{Session{Kind: Interactive, Cwd: "/x", StartedAt: started}, "entry:interactive:/x@1793900000000"},
		{Session{}, "entry::@0"},
	}
	for _, tt := range tests {
		if got := tt.s.Key(); got != tt.want {
			t.Errorf("%+v.Key() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestActive(t *testing.T) {
	tests := []struct {
		s    Session
		want bool
	}{
		{Session{Kind: Interactive, PID: 1}, true},
		{Session{Kind: Interactive}, true},
		{Session{Kind: Background, PID: 1, State: JobDone}, true},
		{Session{Kind: Background, State: JobWorking}, true},
		{Session{Kind: Background, State: JobBlocked}, true},
		{Session{Kind: Background, State: JobDone}, false},
		{Session{Kind: Background, State: JobFailed}, false},
		{Session{Kind: Background, State: JobStopped}, false},
	}
	for _, tt := range tests {
		if got := tt.s.Active(); got != tt.want {
			t.Errorf("%+v.Active() = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestParseLenient(t *testing.T) {
	in := `[
	  {"pid": 1, "kind": "interactive", "status": "compacting", "startedAt": 1793900000000,
	   "entrypoint": "cli", "peer": {"socket": "/tmp/x"}, "cwd": "/a"},
	  {"id": "j1", "kind": "remote", "state": "paused", "startedAt": "2026-11-05T21:15:00Z"},
	  {"pid": "42", "kind": "interactive", "status": "idle", "startedAt": 1793900000000.0},
	  {"pid": 7.5, "id": 12345678, "kind": "background", "state": "done", "startedAt": "soon", "name": null},
	  "not an object",
	  null,
	  {"pid": 1, "kind": "interactive", "status": "busy"},
	  {"pid": 9, "name": ["x"], "kind": "interactive", "status": "idle", "startedAt": 1793900000000}
	]`
	snap, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snap.Sessions); got != 5 {
		t.Fatalf("got %d sessions, want 5: %+v", got, snap.Sessions)
	}

	// Unknown values are kept as strings; unknown fields go to Extra.
	s := snap.Sessions[0]
	if s.Status != "compacting" || s.Verdict().State != StateUnknown {
		t.Errorf("entry 0: status %q verdict %v", s.Status, s.Verdict())
	}
	if string(s.Extra["entrypoint"]) != `"cli"` || string(s.Extra["peer"]) != `{"socket": "/tmp/x"}` {
		t.Errorf("entry 0: extra %q", s.Extra)
	}

	s = snap.Sessions[1]
	if s.Kind != "remote" || s.State != "paused" || s.Key() != "job:j1" {
		t.Errorf("entry 1: %+v", s)
	}
	if want := time.Date(2026, 11, 5, 21, 15, 0, 0, time.UTC); !s.StartedAt.Equal(want) {
		t.Errorf("entry 1: startedAt %v, want %v", s.StartedAt, want)
	}

	// A numeric string pid and a float startedAt are read.
	s = snap.Sessions[2]
	if s.PID != 42 || !s.StartedAt.Equal(time.UnixMilli(1793900000000)) {
		t.Errorf("entry 2: pid %d startedAt %v", s.PID, s.StartedAt)
	}

	// A fractional pid and an unreadable startedAt are kept in Extra; a
	// numeric id is kept as its text; null reads as empty.
	s = snap.Sessions[3]
	if s.PID != 0 || string(s.Extra["pid"]) != "7.5" || !s.StartedAt.IsZero() || string(s.Extra["startedAt"]) != `"soon"` {
		t.Errorf("entry 3: %+v", s)
	}
	if s.JobID != "12345678" || s.Name != "" {
		t.Errorf("entry 3: id %q name %q", s.JobID, s.Name)
	}

	// An array where a string belongs.
	s = snap.Sessions[4]
	if s.PID != 9 || s.Name != "" || string(s.Extra["name"]) != `["x"]` {
		t.Errorf("entry 4: %+v", s)
	}

	wantWarnings := []string{
		`entry 3: pid: not an integer: 7.5`,
		`entry 3: startedAt: not epoch milliseconds or a timestamp: "soon"`,
		`entry 4: not an object: "not an object"; skipped`,
		`entry 5: not an object: null; skipped`,
		`entry 6: duplicate key pid:1 (first at entry 0); skipped`,
		`entry 7: name: not a string: ["x"]`,
	}
	if !slices.Equal(snap.Warnings, wantWarnings) {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(snap.Warnings, "\n"), strings.Join(wantWarnings, "\n"))
	}
}

func TestParseFraming(t *testing.T) {
	good := readFixture(t, "cwd-dogwood.json")
	tests := []struct {
		name     string
		in       string
		sessions int
		warnings int
		wantErr  string
	}{
		{name: "fixture", in: string(good), sessions: 3},
		{name: "empty array", in: "[]\n", sessions: 0},
		{name: "notice before", in: "A new version is available\n" + string(good), sessions: 3, warnings: 1},
		{name: "text after", in: string(good) + "\nbye\n", sessions: 3, warnings: 1},
		{name: "byte order mark", in: "\xef\xbb\xbf" + string(good), sessions: 3},
		{name: "empty", in: "  \n", wantErr: "empty output"},
		{name: "null", in: "null", wantErr: "not a JSON array"},
		{name: "object", in: `{"sessions": []}`, wantErr: "not a JSON array"},
		{name: "truncated", in: string(good[:len(good)/2]), wantErr: "read JSON array"},
		{name: "error text", in: "Error: not logged in\n", wantErr: "not a JSON array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := Parse([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				if len(snap.Sessions) != 0 {
					t.Errorf("sessions on error: %d", len(snap.Sessions))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Sessions) != tt.sessions || len(snap.Warnings) != tt.warnings {
				t.Errorf("got %d sessions, warnings %q; want %d sessions, %d warnings",
					len(snap.Sessions), snap.Warnings, tt.sessions, tt.warnings)
			}
		})
	}
}

// The parser must not depend on encoding/json's field matching: a field
// whose name differs only in case is unknown, not a match.
func TestParseCaseSensitive(t *testing.T) {
	snap, err := Parse([]byte(`[{"PID": 5, "Status": "busy", "kind": "interactive", "pid": 6}]`))
	if err != nil {
		t.Fatal(err)
	}
	s := snap.Sessions[0]
	if s.PID != 6 || s.Status != "" {
		t.Errorf("got pid %d status %q", s.PID, s.Status)
	}
	if _, ok := s.Extra["PID"]; !ok {
		t.Errorf("PID not kept in Extra: %v", s.Extra)
	}
	var raw json.RawMessage = s.Extra["Status"]
	if string(raw) != `"busy"` {
		t.Errorf("Status not kept in Extra: %v", s.Extra)
	}
}
