package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// tokenBytes is the number of random bytes in a new token (hex-encoded on disk).
const tokenBytes = 32

// tokenMode is the only permission set EnsureToken accepts for the token file.
const tokenMode fs.FileMode = 0o600

// EnsureToken returns the token stored in home's token file, creating the
// file (and home, mode 0700) with a new random token if it doesn't exist.
// A new token is 32 random bytes, hex-encoded, written with mode 0600.
//
// An existing token file is refused if its permissions are wider than 0600,
// if it isn't a regular file, or if it is empty. It is never rewritten.
func EnsureToken(home string) (string, error) {
	path := TokenPath(home)

	tok, err := readToken(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return tok, err
	}

	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("config: create %s: %w", home, err)
	}
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("config: generate token: %w", err)
	}
	tok = hex.EncodeToString(buf)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tokenMode)
	if errors.Is(err, fs.ErrExist) {
		// Another process created it between our read and our create.
		return readToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("config: create token: %w", err)
	}
	// The umask can only narrow the mode; set it exactly anyway.
	if err := f.Chmod(tokenMode); err != nil {
		f.Close()
		return "", fmt.Errorf("config: create token: %w", err)
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("config: write token: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("config: write token: %w", err)
	}
	return tok, nil
}

// readToken reads and checks an existing token file. A missing file returns
// an error matching fs.ErrNotExist.
func readToken(path string) (string, error) {
	return readSecret(path, "token")
}

// ReadSecretFile reads a secret, such as the GitHub token that
// github_token_file names, with the same checks as aimd's token file: a
// regular file, mode 0600 or narrower, not empty. Surrounding whitespace is
// trimmed. A missing file returns an error matching fs.ErrNotExist. Errors
// name the path, never the contents.
func ReadSecretFile(path string) (string, error) {
	return readSecret(path, "secret")
}

// readSecret implements readToken and ReadSecretFile; what names the file in
// errors.
func readSecret(path, what string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return "", fmt.Errorf("config: %s: %w", what, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("config: %s %s is not a regular file", what, path)
	}
	if perm := info.Mode().Perm(); perm&^tokenMode != 0 {
		return "", fmt.Errorf("config: %s %s has mode %#o, wider than %#o; run: chmod 600 %s", what, path, perm, tokenMode, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("config: %s: %w", what, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("config: %s %s is empty", what, path)
	}
	return tok, nil
}
