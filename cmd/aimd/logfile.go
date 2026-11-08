package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
)

// logMaxSize is the size past which aimd.log is rotated to aimd.log.1.
// Tests shrink it.
var logMaxSize int64 = 10 << 20

// logFile is aimd.log, rotated by size: when a write would take it past
// maxSize bytes, it is renamed to <path>.1 (replacing an older one) and a
// fresh file is opened at path. A write never splits across the two, so a
// log line is never cut. It is safe for concurrent use.
type logFile struct {
	path    string
	maxSize int64
	// errs gets a line when a rotation fails (aimd's stderr: under
	// launchd, aimd.launchd.log).
	errs io.Writer

	mu sync.Mutex
	f  *os.File
	// size is the file's size as counted from open and the writes since.
	// After a failed rotation it restarts at 0, so the next try waits for
	// another maxSize bytes rather than coming on every write.
	size int64
}

func openLogFile(path string, maxSize int64, errs io.Writer) (*logFile, error) {
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &logFile{path: path, maxSize: maxSize, errs: errs, f: f, size: fi.Size()}, nil
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxSize > 0 && l.size > 0 && l.size+int64(len(p)) > l.maxSize {
		if err := l.rotate(); err != nil {
			l.size = 0
			fmt.Fprintf(l.errs, "aimd: rotate %s: %v\n", l.path, err)
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate renames the file to <path>.1 and opens a new one. On failure the
// current file stays open: writes go on, to wherever it now is.
func (l *logFile) rotate() error {
	// The open file follows the rename, so nothing is lost if the
	// reopen fails.
	if err := os.Rename(l.path, l.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := openAppend(l.path)
	if err != nil {
		return err
	}
	l.f.Close()
	l.f = f
	l.size = 0
	return nil
}

// Close closes the current file.
func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// logWriter is where aimd's log goes: lf, and stderr too unless aimd runs
// under launchd, whose stderr file would only repeat aimd.log. The file
// comes first, so a failing stderr doesn't keep a line out of it.
func logWriter(lf io.Writer, stderr io.Writer, underLaunchd bool) io.Writer {
	if underLaunchd {
		return lf
	}
	return io.MultiWriter(lf, stderr)
}
