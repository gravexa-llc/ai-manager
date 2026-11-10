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
	"os/exec"
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
	return openSys{
		post: postJSON,
		browse: func(ctx context.Context, url string) error {
			return exec.CommandContext(ctx, "/usr/bin/open", url).Run()
		},
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
	if err := s.browse(ctx, target); err != nil {
		fmt.Fprintf(stderr, "aim open: couldn't open a browser (%v); the link works once, for %d seconds:\n%s\n", err, link.ExpiresIn, target)
		return 1
	}
	return 0
}
