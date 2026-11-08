package focus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/osascript"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeSource serves fixed targets and leads.
type fakeSource struct {
	targets []Target
	leads   map[string]Lead
	err     error
}

func (f fakeSource) Targets(context.Context) ([]Target, error) { return f.targets, f.err }

func (f fakeSource) LeadOf(_ context.Context, id string) (Lead, error) {
	if l, ok := f.leads[id]; ok {
		return l, nil
	}
	return Lead{}, fmt.Errorf("%w: %s", ErrUnknownAgent, id)
}

// fakeTerminal records what Go asked of the terminal apps. app is where
// it "finds" sessions and opens jobs (iTerm2 when empty).
type fakeTerminal struct {
	focusErr, attachErr error
	app                 string
	focused             []string // "pid/iterm id"
	attached            []string
}

func (f *fakeTerminal) where() string {
	if f.app == "" {
		return term.AppITerm
	}
	return f.app
}

func (f *fakeTerminal) Focus(_ context.Context, pid int, itermID string) (term.Focused, error) {
	f.focused = append(f.focused, fmt.Sprintf("%d/%s", pid, itermID))
	if f.focusErr != nil {
		return term.Focused{}, f.focusErr
	}
	if pid > 0 {
		return term.Focused{App: f.where(), TTY: "/dev/ttys021", Via: "tty"}, nil
	}
	return term.Focused{App: term.AppITerm, Via: "iterm_session_id"}, nil
}

func (f *fakeTerminal) Attach(_ context.Context, job string) (term.Attached, error) {
	f.attached = append(f.attached, job)
	if f.attachErr != nil {
		return term.Attached{}, f.attachErr
	}
	return term.Attached{App: f.where(), Command: "'/bin/claude' attach " + job}, nil
}

const home = "/Users/someone"

// world is a few live sessions like this machine's.
var world = []Target{
	{Key: "pid:101", SessionID: "s-101", PID: 101, Kind: KindInteractive, Name: "lead", Cwd: home + "/Projects/ai-manager"},
	{Key: "pid:102", SessionID: "s-102", PrevSessionID: "s-102-old", PID: 102, Kind: KindInteractive, Cwd: home + "/Projects/pivot-app"},
	{Key: "pid:103", SessionID: "s-103", PID: 103, Kind: KindInteractive, Name: "pivot notifications", Cwd: home + "/Projects/pivot-site"},
	{Key: "job:022c44bf", JobID: "022c44bf", SessionID: "s-job", Kind: KindBackground, Name: "sharing reports", Cwd: home + "/Projects/pivot-app"},
	{Key: "pid:104", SessionID: "s-104", PID: 104, Kind: KindBackground, Cwd: home + "/Projects/x"}, // no job
	{Key: "pid:105", SessionID: "s-105", PID: 105, Kind: KindInteractive, Cwd: "/private/tmp/run", Headless: true},
	{Key: "session:s-106", SessionID: "s-106", Kind: KindInteractive, Name: "iterm only", ItermSessionID: "w0t0p0:0A1B2C3D-0000-4000-8000-00000000AB01"},
}

func service(ft *fakeTerminal) *Service {
	return &Service{Source: fakeSource{targets: world, leads: map[string]Lead{
		"a2743fbbaf44b95d0": {SessionID: "s-101"},
		"a740b0899d775992b": {PID: 101},
		"ab66668f3b601afd9": {SessionID: "s-gone"},
	}}, Terminal: ft, HomeDir: home}
}

func TestGoInteractive(t *testing.T) {
	ft := &fakeTerminal{}
	res, err := service(ft).Go(context.Background(), "pid:101")
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != api.GoFocused || res.Key != "pid:101" || res.TTY != "/dev/ttys021" || res.Via != "tty" || res.Title != "lead" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Message, "Focused lead in iTerm2 (/dev/ttys021)") {
		t.Errorf("message = %q", res.Message)
	}
	if !slices.Equal(ft.focused, []string{"101/"}) || len(ft.attached) != 0 {
		t.Errorf("focused %v, attached %v", ft.focused, ft.attached)
	}
}

func TestGoItermSessionIDOnly(t *testing.T) {
	ft := &fakeTerminal{}
	res, err := service(ft).Go(context.Background(), "iterm only")
	if err != nil {
		t.Fatal(err)
	}
	if res.Via != "iterm_session_id" || !slices.Equal(ft.focused, []string{"0/w0t0p0:0A1B2C3D-0000-4000-8000-00000000AB01"}) {
		t.Errorf("result %+v, focused %v", res, ft.focused)
	}
}

func TestGoBackground(t *testing.T) {
	ft := &fakeTerminal{}
	for _, key := range []string{"job:022c44bf", "022c44bf"} {
		ft.attached = nil
		res, err := service(ft).Go(context.Background(), key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if res.Action != api.GoAttached || res.Command != "'/bin/claude' attach 022c44bf" || res.Key != "job:022c44bf" {
			t.Errorf("%s: result = %+v", key, res)
		}
		if !slices.Equal(ft.attached, []string{"022c44bf"}) || len(ft.focused) != 0 {
			t.Errorf("%s: attached %v, focused %v", key, ft.attached, ft.focused)
		}
		if res.App != term.AppITerm || !strings.Contains(res.Message, "in a new iTerm2 tab") {
			t.Errorf("%s: app %q, message %q", key, res.App, res.Message)
		}
	}
}

// The app that took it shows in the result.
func TestGoInTerminalApp(t *testing.T) {
	ft := &fakeTerminal{app: term.AppTerminal}
	res, err := service(ft).Go(context.Background(), "lead")
	if err != nil || res.App != term.AppTerminal || res.Message != "Focused lead in Terminal (/dev/ttys021)." {
		t.Errorf("focus: %+v, %v", res, err)
	}
	res, err = service(ft).Go(context.Background(), "job:022c44bf")
	if err != nil || res.App != term.AppTerminal || res.Message != "Opened sharing reports in a new Terminal window (claude attach 022c44bf)." {
		t.Errorf("attach: %+v, %v", res, err)
	}
}

func TestGoSubagentGoesToLead(t *testing.T) {
	for _, key := range []string{
		"agent:a2743fbbaf44b95d0",
		"agent-a2743fbbaf44b95d0",
		"a2743fbbaf44b95d0",
		"s-101/a2743fbbaf44b95d0", // an agent link's ref
		"a740b0899d775992b",       // known by its worktree lock's pid
	} {
		ft := &fakeTerminal{}
		res, err := service(ft).Go(context.Background(), key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if res.Key != "pid:101" || res.AgentID == "" || !slices.Equal(ft.focused, []string{"101/"}) {
			t.Errorf("%s: result %+v, focused %v", key, res, ft.focused)
		}
		if !strings.Contains(res.Message, "runs inside its lead session") || !strings.Contains(res.Message, "Open the agent from the lead") {
			t.Errorf("%s: message = %q", key, res.Message)
		}
	}
	ft := &fakeTerminal{}
	for key, status := range map[string]int{
		"agent:a0000000000000000":   http.StatusNotFound, // unknown agent
		"ab66668f3b601afd9":         http.StatusNotFound, // lead gone
		"s-gone/a2743fbbaf44b95d0":  http.StatusNotFound,
		"agent:a2743fbbaf44b95d0x":  http.StatusNotFound, // not an agent id: a name prefix that matches nothing
		"agent-a2743fbbaf44b95d0/x": http.StatusNotFound,
	} {
		_, err := service(ft).Go(context.Background(), key)
		if got := statusOf(err); got != status {
			t.Errorf("%s: status %d (%v), want %d", key, got, err, status)
		}
	}
	if len(ft.focused)+len(ft.attached) != 0 {
		t.Errorf("refusals reached iTerm2: %v %v", ft.focused, ft.attached)
	}
}

func statusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.HTTPStatus()
	}
	if err == nil {
		return 200
	}
	return 500
}

func TestGoRefusals(t *testing.T) {
	for _, tc := range []struct {
		key    string
		status int
		says   string
	}{
		{"", http.StatusBadRequest, "no session given"},
		{"   ", http.StatusBadRequest, "no session given"},
		{"pid:104", http.StatusUnprocessableEntity, "without a job id"},
		{"pid:999", http.StatusNotFound, "no live session has key pid:999"},
		{"job:deadbeef", http.StatusNotFound, "no live session has key job:deadbeef"},
		{"999", http.StatusNotFound, "no live session has pid 999"},
		{"nothing-like-it", http.StatusNotFound, `no live session matches "nothing-like-it"`},
	} {
		ft := &fakeTerminal{}
		_, err := service(ft).Go(context.Background(), tc.key)
		if statusOf(err) != tc.status || err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%q: %d %v; want %d saying %q", tc.key, statusOf(err), err, tc.status, tc.says)
		}
		if len(ft.focused)+len(ft.attached) != 0 {
			t.Errorf("%q reached iTerm2", tc.key)
		}
	}
}

func TestGoTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		says   string
	}{
		{fmt.Errorf("%w tty /dev/ttys021", term.ErrNotFound), http.StatusUnprocessableEntity,
			"lead isn't reachable: no iTerm2 or Terminal tab has tty /dev/ttys021. Is it running in another terminal app?"},
		{term.ErrNotRunning, http.StatusUnprocessableEntity, "neither iTerm2 nor Terminal is running"},
		{term.ErrNoTTY, http.StatusUnprocessableEntity, "has no terminal"},
		{term.ErrNoProcess, http.StatusUnprocessableEntity, "is gone"},
		{fmt.Errorf("%w: tty x", term.ErrInvalid), http.StatusUnprocessableEntity, "refused"},
		{fmt.Errorf("%w after 30s", osascript.ErrTimeout), http.StatusGatewayTimeout, "allow it and try again"},
		{errors.New("osascript: exit status 1: syntax error"), http.StatusBadGateway, "syntax error"},
	} {
		ft := &fakeTerminal{focusErr: tc.err}
		_, err := service(ft).Go(context.Background(), "lead")
		if statusOf(err) != tc.status || !strings.Contains(err.Error(), tc.says) || !errors.Is(err, tc.err) {
			t.Errorf("%v: %d %v; want %d saying %q", tc.err, statusOf(err), err, tc.status, tc.says)
		}
	}
	ft := &fakeTerminal{attachErr: fmt.Errorf("%w: claude's path", term.ErrInvalid)}
	if _, err := service(ft).Go(context.Background(), "job:022c44bf"); statusOf(err) != http.StatusUnprocessableEntity {
		t.Errorf("attach refused: %d %v", statusOf(err), err)
	}
	boom := errors.New("disk on fire")
	s := &Service{Source: fakeSource{err: boom}, Terminal: ft}
	if _, err := s.Go(context.Background(), "lead"); !errors.Is(err, boom) || statusOf(err) != 500 {
		t.Errorf("source failure: %v", err)
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct{ q, want string }{
		{"pid:102", "pid:102"},
		{"job:022c44bf", "job:022c44bf"},
		{"session:s-106", "session:s-106"},
		{"session:s-103", "pid:103"},     // a session id behind a pid key
		{"session:s-102-old", "pid:102"}, // its id before a /clear
		{"103", "pid:103"},
		{"105", "pid:105"}, // headless, but a pid is exact
		{"022c44bf", "job:022c44bf"},
		{"s-103", "pid:103"},
		{"s-102-old", "pid:102"},
		{"lead", "pid:101"},
		{"LEAD", "pid:101"},
		{"pivot n", "pid:103"},       // name prefix
		{"sharing", "job:022c44bf"},  // a background job's name
		{"ai-man", "pid:101"},        // cwd's last folder
		{"~/Projects/ai", "pid:101"}, // cwd prefix, ~ expanded
		{home + "/Projects/ai", "pid:101"},
	} {
		got, err := Match(world, tc.q, home)
		if err != nil || got.Key != tc.want {
			t.Errorf("Match(%q) = %q, %v; want %q", tc.q, got.Key, err, tc.want)
		}
	}
}

func TestMatchAmbiguous(t *testing.T) {
	// "pivot" is a prefix of one name and of two cwds' folders.
	_, err := Match(world, "pivot", home)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict {
		t.Fatalf("err = %v, want 409", err)
	}
	var keys []string
	for _, c := range e.GoCandidates() {
		keys = append(keys, c.Key)
	}
	slices.Sort(keys)
	if want := []string{"job:022c44bf", "pid:102", "pid:103"}; !slices.Equal(keys, want) {
		t.Errorf("candidates = %v, want %v", keys, want)
	}
	if !strings.Contains(err.Error(), `"pivot" matches 3 sessions`) {
		t.Errorf("message = %q", err)
	}

	// An exact name wins over prefixes; two exact names are ambiguous.
	twins := append(slices.Clone(world), Target{Key: "pid:200", PID: 200, Name: "lead"})
	if _, err := Match(twins, "lead", home); statusOf(err) != http.StatusConflict {
		t.Errorf("two sessions named lead: %v", err)
	}
	if got, err := Match(append(slices.Clone(world), Target{Key: "pid:201", PID: 201, Name: "leader"}), "lead", home); err != nil || got.Key != "pid:101" {
		t.Errorf("exact name beside a longer one: %q %v", got.Key, err)
	}
	// A headless run never matches by prefix.
	if _, err := Match(world, "/private/tmp", home); statusOf(err) != http.StatusNotFound {
		t.Errorf("headless by cwd: %v", err)
	}
}

func TestAgentForms(t *testing.T) {
	for q, want := range map[string]string{
		"agent:a2743fbbaf44b95d0": "a2743fbbaf44b95d0",
		"agent-a2743fbbaf44b95d0": "a2743fbbaf44b95d0",
		"a2743fbbaf44b95d0":       "a2743fbbaf44b95d0",
		"a2743fbbaf44b95d":        "",
		"b2743fbbaf44b95d0":       "",
		"agent:":                  "",
	} {
		got, ok := AgentID(q)
		if got != want || ok != (want != "") {
			t.Errorf("AgentID(%q) = %q, %v", q, got, ok)
		}
	}
	if sid, aid, ok := AgentRef("s-1/a2743fbbaf44b95d0"); !ok || sid != "s-1" || aid != "a2743fbbaf44b95d0" {
		t.Errorf("AgentRef = %q %q %v", sid, aid, ok)
	}
	if sid, aid, ok := AgentRef("lead-1/agent-d1"); !ok || sid != "lead-1" || aid != "agent-d1" {
		t.Errorf("AgentRef with another id format = %q %q %v", sid, aid, ok)
	}
	for _, q := range []string{"~/Projects/x", "/a2743fbbaf44b95d0", "s 1/a2743fbbaf44b95d0", "s-1/", "s-1/a/b", "pid:1/a"} {
		if _, _, ok := AgentRef(q); ok {
			t.Errorf("AgentRef(%q) accepted", q)
		}
	}
}

func TestGoable(t *testing.T) {
	for _, tc := range []struct {
		t    Target
		want bool
	}{
		{Target{Kind: KindInteractive, PID: 1}, true},
		{Target{Kind: KindInteractive, ItermSessionID: "w0t0p0:0A1B2C3D-0000-4000-8000-00000000AB01"}, true},
		{Target{Kind: KindInteractive, ItermSessionID: "junk"}, false},
		{Target{Kind: KindInteractive}, false},
		{Target{Kind: KindBackground, JobID: "022c44bf"}, true},
		{Target{Kind: KindBackground, PID: 7}, false},
	} {
		if got := tc.t.Goable(); got != tc.want {
			t.Errorf("Goable(%+v) = %v", tc.t, got)
		}
	}
}

// A name with a slash is a name, unless its first part is a live
// session's id.
func TestResolveNameWithSlash(t *testing.T) {
	targets := append(slices.Clone(world), Target{Key: "pid:300", PID: 300, Kind: KindInteractive, Name: "share/v1-email-reports"})
	s := &Service{Source: fakeSource{targets: targets}, Terminal: &fakeTerminal{}}
	got, agent, err := s.Resolve(context.Background(), "share/v1")
	if err != nil || got.Key != "pid:300" || agent != "" {
		t.Errorf("Resolve = %q, %q, %v", got.Key, agent, err)
	}
}

// A job id made only of digits is a job id, not a pid.
func TestMatchNumericJobID(t *testing.T) {
	targets := append(slices.Clone(world), Target{Key: "job:12345678", JobID: "12345678", Kind: KindBackground})
	if got, err := Match(targets, "12345678", home); err != nil || got.Key != "job:12345678" {
		t.Errorf("Match = %q, %v", got.Key, err)
	}
}
