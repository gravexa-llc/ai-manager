package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

const openUsage = `usage: aim open [--print] [plan]

Opens the dashboard in your browser and signs that browser in for 30 days.
aimd serves its pages only to a signed-in browser, and only someone who can
read aimd's token (~/.ai-manager/token) can sign one in: aim asks aimd for a
link that works once, within a minute, and opens it.

  plan      open that plan's page (its slug) instead of the Overview
  --print   print the link instead of opening it

On a machine without a browser (a Linux server reached over ssh), aim prints
the link. aimd listens on 127.0.0.1 only, so forward its port from the
machine with the browser and open the link there:

  ssh -L 7777:127.0.0.1:7777 <this machine>

The port must be the same on both ends: aimd answers only to its own address,
so a tunnel from another local port gets "unexpected Host header". If the
port is taken on the machine with the browser, give aimd another one (port
in config.toml, then ` + "`aim up`" + `) and forward that.
`

// openWait bounds the call to aimd.
const openWait = 5 * time.Second

// openSys is what `aim open` needs from the machine; tests fake it.
type openSys struct {
	// post sends a JSON body to aimd with its token (postJSON).
	post func(ctx context.Context, url, token string, body []byte) (int, []byte, error)
	// browse opens a URL in the browser.
	browse func(ctx context.Context, url string) error
}

func realOpenSys() openSys {
	return openSys{post: postJSON, browse: browser(runtime.GOOS)}
}

// errNoBrowser is browse's error on a machine with no browser to open: a
// Linux one without a display, or without xdg-open.
var errNoBrowser = errors.New("no browser on this machine")

// browser opens a URL on goos: with open on a Mac, else with xdg-open when
// the session has a display.
func browser(goos string) func(ctx context.Context, url string) error {
	if goos == "darwin" {
		return func(ctx context.Context, url string) error {
			return exec.CommandContext(ctx, "/usr/bin/open", url).Run()
		}
	}
	return func(ctx context.Context, url string) error {
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errNoBrowser
		}
		opener, err := exec.LookPath("xdg-open")
		if err != nil {
			return errNoBrowser
		}
		// xdg-open can stay in the foreground for as long as the browser
		// it started runs, so it isn't tied to ctx: one still running when
		// ctx ends has opened the link, and goes on after aim exits.
		cmd := exec.Command(opener, url)
		if err := cmd.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

func cmdOpen(args []string, stdout, stderr io.Writer) int {
	return openCmd(realOpenSys(), args, stdout, stderr)
}

func openCmd(s openSys, args []string, stdout, stderr io.Writer) int {
	print, plan := false, ""
	for _, a := range args {
		switch {
		case a == "-h" || a == "-help" || a == "--help":
			fmt.Fprint(stdout, openUsage)
			return 0
		case a == "-print" || a == "--print":
			print = true
		case strings.HasPrefix(a, "-") || plan != "":
			fmt.Fprint(stderr, openUsage)
			return 2
		default:
			plan = a
		}
	}
	home, err := config.Home()
	if err != nil {
		fmt.Fprintf(stderr, "aim open: %v\n", err)
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "aim open: %v\n", err)
		return 1
	}
	// aimd creates its token when it starts, so no token means no aimd.
	token, err := config.ReadSecretFile(config.TokenPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "aim open: aimd hasn't run yet (no %s); start it with `aim up`\n", config.TokenPath(home))
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "aim open: %v\n", err)
		return 1
	}

	req := api.SignInRequest{}
	if plan != "" {
		req.Return = "/app/plans/" + url.PathEscape(plan)
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), openWait)
	defer cancel()
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	status, resp, err := s.post(ctx, base+"/api/sign-in", token, body)
	if err != nil {
		fmt.Fprintf(stderr, "aim open: aimd on 127.0.0.1:%d: %v (is it running? try `aim up`)\n", cfg.Port, err)
		return 1
	}
	var link api.SignInLink
	if status != http.StatusOK || json.Unmarshal(resp, &link) != nil || !strings.HasPrefix(link.Path, "/app/") {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(resp, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", status)
		}
		fmt.Fprintf(stderr, "aim open: aimd refused: %s\n", e.Error)
		return 1
	}
	target := base + link.Path
	if print {
		fmt.Fprintln(stdout, target)
		return 0
	}
	err = s.browse(ctx, target)
	if errors.Is(err, errNoBrowser) {
		fmt.Fprintf(stdout, "There is no browser to open on this machine. This link signs a browser in; it works once, for %d seconds.\nFrom the machine with the browser, forward aimd's port (ssh -L %d:127.0.0.1:%d <this machine>), then open:\n%s\nThe port must be %d on both ends of the tunnel; aimd refuses a browser that reaches it through another one.\n",
			link.ExpiresIn, cfg.Port, cfg.Port, target, cfg.Port)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "aim open: couldn't open a browser (%v); the link works once, for %d seconds:\n%s\n", err, link.ExpiresIn, target)
		return 1
	}
	return 0
}
