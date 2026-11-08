package sessions

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestClaudeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv(ConfigDirEnv, "")
	got, err := ClaudeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".claude"); got != want {
		t.Errorf("ClaudeDir() = %q, want %q", got, want)
	}
	dirs, err := WatchDirs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(home, ".claude", "sessions"), filepath.Join(home, ".claude", "jobs")}; !slices.Equal(dirs, want) {
		t.Errorf("WatchDirs() = %q, want %q", dirs, want)
	}

	other := filepath.Join(t.TempDir(), "claude-config")
	t.Setenv(ConfigDirEnv, other+"/")
	got, err = ClaudeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != other {
		t.Errorf("ClaudeDir() with %s = %q, want %q", ConfigDirEnv, got, other)
	}
	dirs, err = WatchDirs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(other, "sessions"), filepath.Join(other, "jobs")}; !slices.Equal(dirs, want) {
		t.Errorf("WatchDirs() = %q, want %q", dirs, want)
	}

	// A relative value is made absolute, as claude (which inherits the
	// same cwd) would resolve it.
	t.Chdir(home)
	t.Setenv(ConfigDirEnv, "rel")
	got, err = ClaudeDir()
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if want := filepath.Join(cwd, "rel"); got != want {
		t.Errorf("ClaudeDir() with a relative value = %q, want %q", got, want)
	}
}

// startWatch runs watch on dirs with a short retry and returns a channel
// that receives one value per kick.
func startWatch(t *testing.T, dirs []string, retry time.Duration) <-chan struct{} {
	t.Helper()
	kicks := make(chan struct{}, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watch(ctx, dirs, func() { kicks <- struct{}{} }, retry) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("watch returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watch did not return after cancel")
		}
	})
	// Wait until the watcher sees changes in each directory that exists:
	// a change made before it registers is never reported, and a fixed
	// sleep can be too short on a loaded machine. Rewrite a probe file
	// until a kick comes, then remove it and wait for that kick too.
	for _, d := range dirs {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		probe := filepath.Join(d, ".watch-probe")
		deadline := time.Now().Add(10 * time.Second)
	probing:
		for {
			write(t, probe, "probe")
			select {
			case <-kicks:
				break probing
			case <-time.After(50 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatalf("the watcher never reported a change in %s", d)
				}
			}
		}
		if err := os.Remove(probe); err != nil {
			t.Fatal(err)
		}
		waitKick(t, kicks, "the probe was removed")
	}
	return kicks
}

// waitKick waits for at least one kick, then drains any that follow
// within a short quiet period.
func waitKick(t *testing.T, kicks <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-kicks:
	case <-time.After(5 * time.Second):
		t.Fatalf("no kick after %s", what)
	}
	drain(kicks, 100*time.Millisecond)
}

func drain(kicks <-chan struct{}, quiet time.Duration) {
	for {
		select {
		case <-kicks:
		case <-time.After(quiet):
			return
		}
	}
}

func noKick(t *testing.T, kicks <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-kicks:
		t.Errorf("kick after %s", what)
	case <-time.After(300 * time.Millisecond):
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWatch(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	jobsDir := filepath.Join(root, "jobs")
	if err := os.Mkdir(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(sessionsDir, "100.json")
	write(t, pidFile, `{"status":"idle"}`)

	// jobs/ doesn't exist yet.
	kicks := startWatch(t, []string{sessionsDir, jobsDir}, 100*time.Millisecond)

	// A session starts, changes status in place, changes it by rename,
	// and exits.
	write(t, filepath.Join(sessionsDir, "200.json"), `{"status":"busy"}`)
	waitKick(t, kicks, "a new registry file")
	write(t, pidFile, `{"status":"busy"}`)
	waitKick(t, kicks, "a registry file rewritten in place")
	tmp := filepath.Join(sessionsDir, ".100.json.tmp")
	write(t, tmp, `{"status":"waiting"}`)
	if err := os.Rename(tmp, pidFile); err != nil {
		t.Fatal(err)
	}
	waitKick(t, kicks, "a registry file replaced by rename")
	if err := os.Remove(filepath.Join(sessionsDir, "200.json")); err != nil {
		t.Fatal(err)
	}
	waitKick(t, kicks, "a registry file removed")

	// Attribute-only changes are ignored. Let any late kick from the
	// removal arrive first: on a loaded machine one can come after
	// waitKick's quiet period and would be blamed on the chmod.
	drain(kicks, 500*time.Millisecond)
	if err := os.Chmod(pidFile, 0o644); err != nil {
		t.Fatal(err)
	}
	noKick(t, kicks, "a chmod")

	// jobs/ appears: picked up on the next retry, with a kick.
	if err := os.Mkdir(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	waitKick(t, kicks, "jobs/ was created")
	if err := os.Mkdir(filepath.Join(jobsDir, "abcd1234"), 0o700); err != nil {
		t.Fatal(err)
	}
	waitKick(t, kicks, "a new job directory")

	// sessions/ is removed and comes back: watched again.
	if err := os.RemoveAll(sessionsDir); err != nil {
		t.Fatal(err)
	}
	waitKick(t, kicks, "sessions/ was removed")
	if err := os.Mkdir(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	waitKick(t, kicks, "sessions/ came back")
	write(t, filepath.Join(sessionsDir, "300.json"), `{}`)
	waitKick(t, kicks, "a new registry file in the new sessions/")
}

func TestWatchNothingThere(t *testing.T) {
	root := t.TempDir()
	// Neither directory exists; Watch waits for them without failing.
	kicks := startWatch(t, []string{filepath.Join(root, "sessions"), filepath.Join(root, "jobs")}, 50*time.Millisecond)
	noKick(t, kicks, "nothing changed")
	if err := os.Mkdir(filepath.Join(root, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	waitKick(t, kicks, "jobs/ was created")
}
