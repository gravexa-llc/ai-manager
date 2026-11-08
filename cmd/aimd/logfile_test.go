package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

// tempLogPath points AIM_HOME at a temp dir and returns aimd.log's path in it.
func tempLogPath(t *testing.T) string {
	t.Helper()
	t.Setenv(config.HomeEnv, t.TempDir())
	home, err := config.Home()
	if err != nil {
		t.Fatal(err)
	}
	return config.LogPath(home)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if n, err := io.WriteString(w, s); err != nil || n != len(s) {
		t.Fatalf("write %q = %d, %v", s, n, err)
	}
}

func TestLogFileRotates(t *testing.T) {
	path := tempLogPath(t)
	old := strings.Repeat("a", 59) + "\n" // left by an earlier aimd
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	line := func(c string) string { return strings.Repeat(c, 29) + "\n" } // 30 bytes
	var errs bytes.Buffer
	lf, err := openLogFile(path, 100, &errs)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()

	write(t, lf, line("1")) // 90 bytes: fits
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("rotated too early: %v", err)
	}
	write(t, lf, line("2")) // 120 would pass the cap: rotate first
	if got := readFile(t, path+".1"); got != old+line("1") {
		t.Errorf("aimd.log.1 = %q", got)
	}
	if got := readFile(t, path); got != line("2") {
		t.Errorf("aimd.log = %q", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("new aimd.log: %v, %v", fi, err)
	}

	write(t, lf, line("3"))
	write(t, lf, line("4"))
	write(t, lf, line("5")) // a second rotation replaces aimd.log.1
	if got := readFile(t, path+".1"); got != line("2")+line("3")+line("4") {
		t.Errorf("aimd.log.1 after a second rotation = %q", got)
	}
	if got := readFile(t, path); got != line("5") {
		t.Errorf("aimd.log after a second rotation = %q", got)
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Errorf("kept a second old log: %v", err)
	}
	if errs.Len() > 0 {
		t.Errorf("errs = %q", errs.String())
	}
}

// A single line longer than the cap goes into an empty file whole, rather
// than rotating an empty file away.
func TestLogFileLongLine(t *testing.T) {
	path := tempLogPath(t)
	lf, err := openLogFile(path, 100, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	long := strings.Repeat("x", 150) + "\n"
	write(t, lf, long)
	if got := readFile(t, path); got != long {
		t.Errorf("aimd.log = %q", got)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("rotated an empty file: %v", err)
	}
}

// A rotation that fails is reported, logging goes on in the same file, and
// the next try waits for another cap's worth of writes.
func TestLogFileRotationFails(t *testing.T) {
	path := tempLogPath(t)
	// A non-empty directory in aimd.log.1's place: the rename fails.
	if err := os.MkdirAll(filepath.Join(path+".1", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	lf, err := openLogFile(path, 100, &errs)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	line := strings.Repeat("l", 39) + "\n" // 40 bytes
	for range 4 {
		write(t, lf, line)
	}
	// Writes 3 and 4 would both pass the cap; only write 3 tries, since
	// the failure restarts the count.
	if got := strings.Count(errs.String(), "aimd: rotate "+path); got != 1 {
		t.Errorf("rotation errors reported %d times, want 1:\n%s", got, errs.String())
	}
	if got := readFile(t, path); got != strings.Repeat(line, 4) {
		t.Errorf("aimd.log = %q", got)
	}

	// Once the way is clear, the next try rotates. It comes at write 5,
	// when writes 3 to 5 pass the cap again.
	if err := os.RemoveAll(path + ".1"); err != nil {
		t.Fatal(err)
	}
	write(t, lf, line)
	if got := readFile(t, path+".1"); got != strings.Repeat(line, 4) {
		t.Errorf("aimd.log.1 = %q", got)
	}
	if got := readFile(t, path); got != line {
		t.Errorf("aimd.log = %q", got)
	}
	if got := strings.Count(errs.String(), "aimd: rotate"); got != 1 {
		t.Errorf("rotation errors reported %d times, want 1:\n%s", got, errs.String())
	}
}

// slog writes from many goroutines: no race, no line cut or interleaved,
// and neither file over the cap.
func TestLogFileConcurrent(t *testing.T) {
	path := tempLogPath(t)
	const maxSize = 4 << 10
	lf, err := openLogFile(path, maxSize, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(lf, nil))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				log.Info("line", "g", g, "i", i)
			}
		})
	}
	wg.Wait()
	if err := lf.Close(); err != nil {
		t.Fatal(err)
	}

	lineRE := regexp.MustCompile(`^time=\S+ level=INFO msg=line g=\d+ i=\d+$`)
	for _, p := range []string{path, path + ".1"} {
		text := readFile(t, p)
		if len(text) > maxSize {
			t.Errorf("%s is %d bytes, over %d", filepath.Base(p), len(text), maxSize)
		}
		if !strings.HasSuffix(text, "\n") {
			t.Errorf("%s ends mid-line", filepath.Base(p))
		}
		for _, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			if !lineRE.MatchString(l) {
				t.Errorf("%s has a broken line %q", filepath.Base(p), l)
			}
		}
	}
}

// Under launchd (AIMD_LAUNCHD=1 from `aim up`) aimd logs only to aimd.log,
// since launchd's stderr file would repeat it; run by hand, to both.
func TestLogWriter(t *testing.T) {
	for _, underLaunchd := range []bool{true, false} {
		t.Run(fmt.Sprintf("launchd=%v", underLaunchd), func(t *testing.T) {
			path := tempLogPath(t)
			var stderr bytes.Buffer
			lf, err := openLogFile(path, logMaxSize, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer lf.Close()
			slog.New(slog.NewTextHandler(logWriter(lf, &stderr, underLaunchd), nil)).Info("hello")

			if got := readFile(t, path); !strings.Contains(got, "msg=hello") {
				t.Errorf("aimd.log = %q", got)
			}
			if got := stderr.String(); underLaunchd != (got == "") {
				t.Errorf("stderr = %q", got)
			}
			if !underLaunchd && stderr.String() != readFile(t, path) {
				t.Errorf("stderr %q and aimd.log %q differ", stderr.String(), readFile(t, path))
			}
		})
	}
}
