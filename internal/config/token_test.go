package config

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureTokenCreates(t *testing.T) {
	_, aimHome := setup(t) // aimHome doesn't exist yet

	tok, err := EnsureToken(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		t.Errorf("token %q is not 32 hex-encoded bytes (err %v)", tok, err)
	}

	info, err := os.Stat(TokenPath(aimHome))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token mode = %#o, want 0600", perm)
	}
	dir, err := os.Stat(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("home dir mode = %#o, want no group or other access", perm)
	}

	data, err := os.ReadFile(TokenPath(aimHome))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != tok {
		t.Errorf("file holds %q, EnsureToken returned %q", data, tok)
	}
}

func TestEnsureTokenReturnsExisting(t *testing.T) {
	_, aimHome := setup(t)

	first, err := EnsureToken(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureToken(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("second call returned %q, want the first token %q", second, first)
	}

	// A token written by hand is returned as is, trimmed.
	if err := os.WriteFile(TokenPath(aimHome), []byte("  hand-written\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureToken(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hand-written" {
		t.Errorf("EnsureToken() = %q, want %q", got, "hand-written")
	}
}

func TestEnsureTokenAcceptsNarrowerMode(t *testing.T) {
	_, aimHome := setup(t)
	if err := os.MkdirAll(aimHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TokenPath(aimHome), []byte("abc\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if got, err := EnsureToken(aimHome); err != nil || got != "abc" {
		t.Errorf("EnsureToken() = %q, %v; want %q, nil", got, err, "abc")
	}
}

func TestEnsureTokenRefusesWideMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o700, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			_, aimHome := setup(t)
			if err := os.MkdirAll(aimHome, 0o700); err != nil {
				t.Fatal(err)
			}
			path := TokenPath(aimHome)
			if err := os.WriteFile(path, []byte("abc\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}

			tok, err := EnsureToken(aimHome)
			if err == nil {
				t.Fatalf("EnsureToken() = %q, nil; want an error for mode %#o", tok, mode)
			}
			if !strings.Contains(err.Error(), "wider than 0600") {
				t.Errorf("error = %q, want it to mention the mode", err)
			}

			// The file is left alone.
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != mode {
				t.Errorf("mode changed to %#o", info.Mode().Perm())
			}
			if data, _ := os.ReadFile(path); string(data) != "abc\n" {
				t.Errorf("contents changed to %q", data)
			}
		})
	}
}

func TestEnsureTokenRefusesEmpty(t *testing.T) {
	_, aimHome := setup(t)
	if err := os.MkdirAll(aimHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TokenPath(aimHome), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureToken(aimHome); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("EnsureToken() error = %v, want an empty-token error", err)
	}
}

func TestEnsureTokenRefusesDirectory(t *testing.T) {
	_, aimHome := setup(t)
	if err := os.MkdirAll(filepath.Join(aimHome, TokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureToken(aimHome); err == nil {
		t.Error("EnsureToken() succeeded with token as a directory")
	}
}

func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, name, body string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("ok", func(t *testing.T) {
		path := write(t, "ok", "  ghp_example\n", 0o600)
		if got, err := ReadSecretFile(path); err != nil || got != "ghp_example" {
			t.Errorf("ReadSecretFile() = %q, %v; want %q, nil", got, err, "ghp_example")
		}
	})
	t.Run("read-only", func(t *testing.T) {
		path := write(t, "ro", "ghp_example\n", 0o400)
		if got, err := ReadSecretFile(path); err != nil || got != "ghp_example" {
			t.Errorf("ReadSecretFile() = %q, %v; want %q, nil", got, err, "ghp_example")
		}
	})
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		t.Run("mode "+mode.String(), func(t *testing.T) {
			path := write(t, "wide"+mode.String(), "ghp_secretvalue\n", mode)
			_, err := ReadSecretFile(path)
			if err == nil || !strings.Contains(err.Error(), "wider than 0600") {
				t.Fatalf("ReadSecretFile() error = %v, want a mode error", err)
			}
			if strings.Contains(err.Error(), "ghp_secretvalue") {
				t.Errorf("error %q echoes the secret", err)
			}
		})
	}
	t.Run("empty", func(t *testing.T) {
		path := write(t, "empty", " \n", 0o600)
		if _, err := ReadSecretFile(path); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("ReadSecretFile() error = %v, want an empty-secret error", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := ReadSecretFile(filepath.Join(dir, "nope")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadSecretFile() error = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := ReadSecretFile(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("ReadSecretFile() error = %v, want not a regular file", err)
		}
	})
}
