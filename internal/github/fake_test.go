package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

// fixtureDir holds the scrubbed recorded GraphQL responses.
const fixtureDir = "../../testdata/github"

// t0 is the test clock's start: before both rate-limit windows in the
// fixtures reset (05:37:12Z and 06:07:31Z).
var t0 = time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeTokens is a TokenSource with fixed tokens per owner.
type fakeTokens struct {
	mu      sync.Mutex
	tokens  map[string]string // lower-case owner → token; "*" for any other owner
	errs    map[string]error  // lower-case owner → error; "*" for any
	calls   []string
	forgets []string
}

func staticTokens(tok string) *fakeTokens {
	return &fakeTokens{tokens: map[string]string{"*": tok}}
}

func (f *fakeTokens) Token(_ context.Context, owner string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, owner)
	o := strings.ToLower(owner)
	if err := f.errs[o]; err != nil {
		return "", err
	}
	if err := f.errs["*"]; err != nil {
		return "", err
	}
	if tok, ok := f.tokens[o]; ok {
		return tok, nil
	}
	return f.tokens["*"], nil
}

func (f *fakeTokens) Forget(owner string) {
	f.mu.Lock()
	f.forgets = append(f.forgets, owner)
	f.mu.Unlock()
}

func (f *fakeTokens) setErr(owner string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errs == nil {
		f.errs = map[string]error{}
	}
	if err == nil {
		delete(f.errs, owner)
	} else {
		f.errs[owner] = err
	}
}

// fakeRequest is one request the fake received.
type fakeRequest struct {
	Query  string
	Vars   map[string]any
	Token  string
	Path   string
	Header http.Header
}

// fakeRepo is a repo the fake knows, built from the recorded fixtures.
type fakeRepo struct {
	nameWithOwner string
	defaultBranch string
	branches      map[string]json.RawMessage // branch → {totalCount, nodes}
}

// fakeGitHub is an httptest GraphQL server. By default it answers branch
// queries of any shape from the repos in branch_prs_combined_18_repos.json
// (so batching can be checked against real responses), and search queries
// with search(). handle can take over any request.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	repos    map[string]*fakeRepo        // lower-case requested or canonical "owner/name"
	bodies   map[string]string           // lower-case "owner/name#number" → body to add
	extra    map[string][]map[string]any // lower-case "owner/name|branch" → nodes to add
	rate     RateLimit                   // rateLimit for each answer
	tokens   map[string]bool             // accepted tokens; nil accepts any
	handle   func(w http.ResponseWriter, req fakeRequest) bool
	search   func(req fakeRequest) any
	requests []fakeRequest
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:      t,
		repos:  loadFixtureRepos(t),
		bodies: map[string]string{},
		extra:  map[string][]map[string]any{},
		rate:   RateLimit{Cost: 1, Remaining: 4900, Limit: 5000, Used: 100, ResetAt: t0.Add(time.Hour)},
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// loadFixtureRepos reads the combined 18-repo response and its variables
// into repos keyed by both the requested and the answered name.
func loadFixtureRepos(t *testing.T) map[string]*fakeRepo {
	t.Helper()
	var vars struct {
		Combined map[string]string `json:"combined"`
	}
	if err := json.Unmarshal(readFixture(t, "branch_queries_variables.json"), &vars); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(readFixture(t, "branch_prs_combined_18_repos.json"), &resp); err != nil {
		t.Fatal(err)
	}
	repos := map[string]*fakeRepo{}
	for i := 0; ; i++ {
		owner, ok := vars.Combined[fmt.Sprintf("o%d", i)]
		if !ok {
			break
		}
		name := vars.Combined[fmt.Sprintf("n%d", i)]
		var r map[string]json.RawMessage
		if err := json.Unmarshal(resp.Data[fmt.Sprintf("r%d", i)], &r); err != nil {
			t.Fatal(err)
		}
		fr := &fakeRepo{branches: map[string]json.RawMessage{}}
		_ = json.Unmarshal(r["nameWithOwner"], &fr.nameWithOwner)
		var def struct{ Name string }
		_ = json.Unmarshal(r["defaultBranchRef"], &def)
		fr.defaultBranch = def.Name
		for j := 0; ; j++ {
			b, ok := vars.Combined[fmt.Sprintf("r%db%d", i, j)]
			if !ok {
				break
			}
			fr.branches[b] = r[fmt.Sprintf("b%d", j)]
		}
		repos[strings.ToLower(owner+"/"+name)] = fr
		repos[strings.ToLower(fr.nameWithOwner)] = fr
	}
	return repos
}

// client returns a Client talking to f with tokens (staticTokens("tok") if
// nil) and the given options.
func (f *fakeGitHub) client(tokens TokenSource, clk *clock, opts Options) *Client {
	if tokens == nil {
		tokens = staticTokens("tok")
	}
	opts.Endpoint = f.srv.URL
	opts.Tokens = tokens
	if clk != nil {
		opts.Now = clk.Now
	}
	return New(config.Config{}, opts)
}

func (f *fakeGitHub) reqs() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeGitHub) setRate(rl RateLimit) {
	f.mu.Lock()
	f.rate = rl
	f.mu.Unlock()
}

// acceptOnly makes the fake answer 401 to any token but these.
func (f *fakeGitHub) acceptOnly(tokens ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = map[string]bool{}
	for _, t := range tokens {
		f.tokens[t] = true
	}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("fake: %s request, want POST", r.Method)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		f.t.Errorf("fake: Content-Type %q", ct)
	}
	if r.Header.Get("User-Agent") == "" {
		f.t.Error("fake: no User-Agent")
	}
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("fake: decode request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req := fakeRequest{Query: body.Query, Vars: body.Variables, Path: r.URL.Path, Header: r.Header.Clone()}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "bearer ") {
		f.t.Errorf("fake: Authorization %q, want a bearer token", auth)
	}
	req.Token = strings.TrimPrefix(auth, "bearer ")

	f.mu.Lock()
	f.requests = append(f.requests, req)
	tokens, handle, search := f.tokens, f.handle, f.search
	f.mu.Unlock()

	if !strings.HasPrefix(strings.TrimSpace(req.Query), "query") || strings.Contains(req.Query, "mutation") {
		f.t.Errorf("fake: received a non-query document:\n%s", req.Query)
	}
	if !strings.Contains(req.Query, "rateLimit { cost remaining resetAt") {
		f.t.Error("fake: query doesn't read rateLimit")
	}
	if tokens != nil && !tokens[req.Token] {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/graphql"}`)
		return
	}
	if handle != nil && handle(w, req) {
		return
	}
	var out any
	if strings.Contains(req.Query, "search(") {
		if search == nil {
			f.t.Error("fake: unexpected search query")
			http.Error(w, "no search", http.StatusInternalServerError)
			return
		}
		out = search(req)
	} else {
		out = f.branchAnswer(req)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		f.t.Errorf("fake: encode: %v", err)
	}
}

var repoVarRE = regexp.MustCompile(`^o(\d+)$`)

// branchAnswer answers a branch query of any shape from f.repos.
func (f *fakeGitHub) branchAnswer(req fakeRequest) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := map[string]any{"rateLimit": f.rate}
	var errs []map[string]any
	for v := range req.Vars {
		m := repoVarRE.FindStringSubmatch(v)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		owner, _ := req.Vars[v].(string)
		name, _ := req.Vars[fmt.Sprintf("n%d", i)].(string)
		alias := fmt.Sprintf("r%d", i)
		if want := fmt.Sprintf("%s: repository(owner: $o%d, name: $n%d)", alias, i, i); !strings.Contains(req.Query, want) {
			f.t.Errorf("fake: query lacks %q", want)
		}
		repo := f.repos[strings.ToLower(owner+"/"+name)]
		if repo == nil {
			data[alias] = nil
			errs = append(errs, map[string]any{
				"type":    "NOT_FOUND",
				"path":    []string{alias},
				"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name),
			})
			continue
		}
		obj := map[string]any{
			"nameWithOwner":    repo.nameWithOwner,
			"defaultBranchRef": map[string]string{"name": repo.defaultBranch},
		}
		for j := 0; ; j++ {
			bv := fmt.Sprintf("r%db%d", i, j)
			branch, ok := req.Vars[bv].(string)
			if !ok {
				break
			}
			if want := fmt.Sprintf("b%d: pullRequests(headRefName: $%s, first: 5,", j, bv); !strings.Contains(req.Query, want) {
				f.t.Errorf("fake: query lacks %q", want)
			}
			obj[fmt.Sprintf("b%d", j)] = f.connection(repo, branch)
		}
		// PRsByNumber's fields, answered from every PR the repo's
		// branches have.
		for _, num := range numbersIn(req.Query, alias) {
			pa := fmt.Sprintf("p%d", num)
			if node := f.prByNumber(repo, num); node != nil {
				obj[pa] = node
				continue
			}
			obj[pa] = nil
			errs = append(errs, map[string]any{
				"type":    "NOT_FOUND",
				"path":    []string{alias, pa},
				"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", num),
			})
		}
		data[alias] = obj
	}
	out := map[string]any{"data": data}
	if errs != nil {
		out["errors"] = errs
	}
	return out
}

// connection returns a branch's {totalCount, nodes}, with any bodies and
// extra nodes added. Caller holds f.mu.
func (f *fakeGitHub) connection(repo *fakeRepo, branch string) map[string]any {
	conn := map[string]any{"totalCount": float64(0), "nodes": []any{}}
	if raw, ok := repo.branches[branch]; ok {
		if err := json.Unmarshal(raw, &conn); err != nil {
			f.t.Fatal(err)
		}
	}
	nodes, _ := conn["nodes"].([]any)
	for _, n := range f.extra[strings.ToLower(repo.nameWithOwner+"|"+branch)] {
		nodes = append(nodes, n)
		conn["totalCount"] = conn["totalCount"].(float64) + 1
	}
	for _, n := range nodes {
		node := n.(map[string]any)
		key := fmt.Sprintf("%s#%v", strings.ToLower(repo.nameWithOwner), node["number"])
		if b, ok := f.bodies[key]; ok {
			node["body"] = b
		}
	}
	conn["nodes"] = nodes
	return conn
}

var numberFieldRE = regexp.MustCompile(`^\s*p(\d+): pullRequest\(number: (\d+)\) \{ \.\.\.PRFields \}$`)

// numbersIn returns the PR numbers a query asks for under a repo alias
// (PRsByNumber's pN: pullRequest(number: N) fields), checking that each
// alias names its own number.
func numbersIn(q, alias string) []int {
	var out []int
	in := false
	for line := range strings.Lines(q) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "  "+alias+": repository("):
			in = true
		case in && line == "  }":
			return out
		case in:
			if m := numberFieldRE.FindStringSubmatch(line); m != nil && m[1] == m[2] {
				n, _ := strconv.Atoi(m[1])
				out = append(out, n)
			}
		}
	}
	return out
}

// prByNumber finds a PR node by number among the repo's branches and the
// extra nodes. Caller holds f.mu.
func (f *fakeGitHub) prByNumber(repo *fakeRepo, number int) map[string]any {
	for branch := range repo.branches {
		nodes, _ := f.connection(repo, branch)["nodes"].([]any)
		for _, n := range nodes {
			if node := n.(map[string]any); node["number"] == float64(number) {
				return node
			}
		}
	}
	for k, nodes := range f.extra {
		if !strings.HasPrefix(k, strings.ToLower(repo.nameWithOwner)+"|") {
			continue
		}
		for _, node := range nodes {
			if fmt.Sprint(node["number"]) == fmt.Sprint(number) {
				return node
			}
		}
	}
	return nil
}

// branchCount returns how many pullRequests connections a query holds.
func branchCount(q string) int {
	return strings.Count(q, ": pullRequests(headRefName:")
}

// rawJSON writes a fixed response.
func rawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// trackedRepos is the recorded batch's repos and branches as RepoBranches,
// in the same order as the combined fixture.
func trackedRepos(t *testing.T) []RepoBranches {
	t.Helper()
	var vars struct {
		Combined map[string]string `json:"combined"`
	}
	if err := json.Unmarshal(readFixture(t, "branch_queries_variables.json"), &vars); err != nil {
		t.Fatal(err)
	}
	var out []RepoBranches
	for i := 0; ; i++ {
		owner, ok := vars.Combined[fmt.Sprintf("o%d", i)]
		if !ok {
			return out
		}
		rb := RepoBranches{Owner: owner, Name: vars.Combined[fmt.Sprintf("n%d", i)]}
		for j := 0; ; j++ {
			b, ok := vars.Combined[fmt.Sprintf("r%db%d", i, j)]
			if !ok {
				break
			}
			rb.Branches = append(rb.Branches, b)
		}
		out = append(out, rb)
	}
}
