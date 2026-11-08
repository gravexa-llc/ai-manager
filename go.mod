module github.com/gravexa-llc/ai-manager

go 1.26.0

// Go 1.26.0 (Homebrew's) has a race-detector fork bug on darwin/arm64
// (https://go.dev/issue/79804). This line makes the go command download
// and use 1.26.10 for this repo only; other projects keep their own Go.
toolchain go1.26.10

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/fsnotify/fsnotify v1.9.0
	golang.org/x/sys v0.49.0
	modernc.org/sqlite v1.61.1
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
