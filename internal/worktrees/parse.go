package worktrees

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// record is one entry of `git worktree list --porcelain`.
type record struct {
	Path           string
	Head           string // full SHA; empty for a bare entry
	Branch         string // full ref (refs/heads/x); empty when detached or bare
	Bare           bool
	Detached       bool
	Locked         bool
	LockReason     string
	Prunable       bool
	PrunableReason string
}

// parsePorcelain parses `git worktree list --porcelain` output: NUL-separated
// fields when nul is true (-z), otherwise lines. Without -z, git C-quotes a
// lock reason that has unusual characters; it is unquoted here. Unknown
// attributes are ignored so a newer git doesn't break the scan.
func parsePorcelain(data []byte, nul bool) ([]record, error) {
	sep := byte('\n')
	if nul {
		sep = 0
	}
	var (
		out []record
		cur *record
	)
	for _, field := range bytes.Split(data, []byte{sep}) {
		f := string(field)
		if f == "" { // end of a record
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
			continue
		}
		key, val, _ := strings.Cut(f, " ")
		if key == "worktree" {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &record{Path: val}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("worktree list: %q before any worktree line", f)
		}
		switch key {
		case "HEAD":
			cur.Head = val
		case "branch":
			cur.Branch = val
		case "bare":
			cur.Bare = true
		case "detached":
			cur.Detached = true
		case "locked":
			cur.Locked = true
			cur.LockReason = val
			if !nul && strings.HasPrefix(val, `"`) {
				if u, err := strconv.Unquote(val); err == nil {
					cur.LockReason = u
				}
			}
		case "prunable":
			cur.Prunable = true
			cur.PrunableReason = val
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out, nil
}

// ClaudeLock is a worktree lock taken by Claude Code. A worktree-isolated
// agent's worktree is locked with the reason
//
//	claude agent agent-<agentId> (pid <pid> start <time>)
//
// where pid and start identify the lead session's process: the pid
// is the lead's pid in `claude agents --json`, and start is when that
// process started, in UTC. The lock is released when the agent finishes, so
// a lock whose pid is dead, or alive with a different start time, is stale.
type ClaudeLock struct {
	Kind      string    // "agent" for worktree-isolated agents
	Name      string    // "agent-<agentId>"
	AgentID   string    // <agentId>, when Kind is "agent"
	PID       int       // the lead session's process
	Start     time.Time // the lead process's start time, UTC; zero if unparseable
	StartText string    // start as written in the reason
}

var claudeLockRE = regexp.MustCompile(`^claude (\S+) (\S+) \(pid ([0-9]+) start ([^()]*)\)$`)

// ParseLockReason parses a Claude Code lock reason. It returns false for any
// other reason, including an empty one.
func ParseLockReason(reason string) (ClaudeLock, bool) {
	m := claudeLockRE.FindStringSubmatch(strings.TrimSpace(reason))
	if m == nil {
		return ClaudeLock{}, false
	}
	pid, err := strconv.Atoi(m[3])
	if err != nil {
		return ClaudeLock{}, false
	}
	l := ClaudeLock{Kind: m[1], Name: m[2], PID: pid, StartText: m[4]}
	if l.Kind == "agent" {
		l.AgentID = strings.TrimPrefix(l.Name, "agent-")
	}
	// ctime(3) format, e.g. "Fri Nov  6 01:23:32 2026". Parse gives UTC.
	if t, err := time.Parse(time.ANSIC, m[4]); err == nil {
		l.Start = t
	}
	return l, true
}

// branchInfo is one local branch's upstream, from for-each-ref.
type branchInfo struct {
	Upstream string // short name, e.g. origin/x; empty when none is set
	State    UpstreamState
	Ahead    int
	Behind   int
}

// refsFormat is the for-each-ref format parseRefs reads: refname, upstream
// and track, each ended by a NUL (git adds a newline after each record).
// NULs because branch names and upstreams may contain '|' or spaces.
const refsFormat = "--format=%(refname)%00%(upstream:short)%00%(upstream:track,nobracket)%00"

// parseRefs parses for-each-ref output in refsFormat into a map keyed by
// short branch name.
func parseRefs(data []byte) (map[string]branchInfo, error) {
	const nfields = 3
	fields := strings.Split(string(data), "\x00")
	// The text after the last NUL is the final record's newline.
	if n := len(fields); n > 0 && strings.TrimSpace(fields[n-1]) == "" {
		fields = fields[:n-1]
	}
	if len(fields)%nfields != 0 {
		return nil, fmt.Errorf("for-each-ref: %d fields, want a multiple of %d", len(fields), nfields)
	}
	out := make(map[string]branchInfo, len(fields)/nfields)
	for i := 0; i < len(fields); i += nfields {
		name := strings.TrimPrefix(strings.TrimPrefix(fields[i], "\n"), "refs/heads/")
		b := branchInfo{Upstream: fields[i+1], State: UpstreamNone}
		if b.Upstream != "" {
			gone, ahead, behind, err := parseTrack(fields[i+2])
			if err != nil {
				return nil, fmt.Errorf("for-each-ref: branch %q: %w", name, err)
			}
			b.Ahead, b.Behind = ahead, behind
			b.State = UpstreamOK
			if gone {
				b.State = UpstreamGone
			}
		}
		out[name] = b
	}
	return out, nil
}

// parseTrack parses %(upstream:track): "", "gone", "ahead N", "behind N" or
// "ahead N, behind M", with or without the surrounding brackets.
func parseTrack(s string) (gone bool, ahead, behind int, err error) {
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "["), "]")
	if s == "" {
		return false, 0, 0, nil
	}
	if s == "gone" {
		return true, 0, 0, nil
	}
	for _, part := range strings.Split(s, ",") {
		key, num, ok := strings.Cut(strings.TrimSpace(part), " ")
		n, convErr := strconv.Atoi(num)
		if !ok || convErr != nil || n < 0 {
			return false, 0, 0, fmt.Errorf("upstream track %q", s)
		}
		switch key {
		case "ahead":
			ahead = n
		case "behind":
			behind = n
		default:
			return false, 0, 0, fmt.Errorf("upstream track %q", s)
		}
	}
	return false, ahead, behind, nil
}

// parseStatus counts the entries of `git status --porcelain=v2` output
// (NUL-separated with -z, else lines): tracked changes (ordinary, renamed or
// copied, unmerged) and untracked entries. Headers and ignored entries are
// skipped. With -z a renamed entry is followed by its original path as a
// separate field, which is skipped too.
func parseStatus(data []byte, nul bool) (uncommitted, untracked int, err error) {
	sep := "\n"
	if nul {
		sep = "\x00"
	}
	fields := strings.Split(string(data), sep)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		switch f[0] {
		case '1', 'u':
			uncommitted++
		case '2':
			uncommitted++
			if nul {
				i++ // the original path
			}
		case '?':
			untracked++
		case '#', '!':
		default:
			return 0, 0, fmt.Errorf("status: unexpected entry %q", f)
		}
	}
	return uncommitted, untracked, nil
}

// parseConfigZ parses `git config -z --get-regexp` output: entries ended by
// NUL, each a key, a newline and the value. A key set with no value (a bare
// boolean) has no newline.
func parseConfigZ(data []byte) [][2]string {
	var out [][2]string
	for _, entry := range strings.Split(string(data), "\x00") {
		if entry == "" {
			continue
		}
		key, val, _ := strings.Cut(entry, "\n")
		out = append(out, [2]string{key, val})
	}
	return out
}

// subsectionOf returns <name> for a key section.<name>.<variable>, or "" if
// the key has no subsection. Section and variable are lower case in git's
// output; the subsection keeps its case and may contain dots or spaces.
func subsectionOf(key, section string) (name, variable string) {
	rest, ok := strings.CutPrefix(key, section+".")
	if !ok {
		return "", ""
	}
	i := strings.LastIndex(rest, ".")
	if i <= 0 {
		return "", ""
	}
	return rest[:i], rest[i+1:]
}

// filterDrivers returns the names of the filter drivers defined in config
// entries for ^filter\., sorted and without duplicates.
func filterDrivers(entries [][2]string) []string {
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if name, _ := subsectionOf(e[0], "filter"); name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// errFilterName is why status isn't run when a filter driver can't be
// overridden on the command line.
var errFilterName = errors.New("filter driver name contains '=', so it can't be overridden; status not run")

// filterOverrideArgs returns -c options that turn each driver's clean,
// smudge and process commands into empty strings, which git treats as no
// filter: nothing runs. A driver marked required then makes status fail,
// which is reported as an unknown state. git splits -c at the first '=', so
// a driver name containing one can't be overridden.
func filterOverrideArgs(names []string) ([]string, error) {
	var args []string
	for _, name := range names {
		if strings.Contains(name, "=") {
			return nil, fmt.Errorf("%w: %q", errFilterName, name)
		}
		for _, v := range []string{"clean", "smudge", "process"} {
			args = append(args, "-c", "filter."+name+"."+v+"=")
		}
	}
	return args, nil
}

// remotesFrom returns remote name → URL from config entries for
// ^remote\..*\.url$. A remote with several URLs keeps the first, which is
// the one git fetches from.
func remotesFrom(entries [][2]string) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		name, variable := subsectionOf(e[0], "remote")
		if name == "" || variable != "url" {
			continue
		}
		if _, ok := out[name]; !ok {
			out[name] = e[1]
		}
	}
	return out
}

// parseRevListTimestamps parses `git rev-list --timestamp` lines
// ("<unix time> <sha>") into commit times by SHA.
func parseRevListTimestamps(data []byte) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		ts, sha, ok := strings.Cut(line, " ")
		sec, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil {
			return nil, errors.New("rev-list: unexpected line " + strconv.Quote(line))
		}
		out[sha] = time.Unix(sec, 0)
	}
	return out, nil
}
