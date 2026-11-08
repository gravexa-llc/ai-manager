package web

// Plugin builds: which build of the plugin each session loaded, and
// whether the plugin on disk has moved on since.
//
// A session loads the plugin's mod once, when it starts, and keeps it until
// it restarts. The mod reports the build and its folder on session_start
// (store.Session PluginBuild, PluginRoot); Options.PluginBuilds says what
// that folder holds now. A session whose build differs from it is flagged
// "Older plugin" on the Sessions page, in its row and its detail, and
// counted in the page's summary. The Settings page's Versions card shows
// aimd's own version and the plugin build on disk in each folder live
// sessions loaded it from.

import (
	"cmp"
	"context"
	"slices"
	"strconv"

	"github.com/gravexa-llc/ai-manager/internal/pluginbuild"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// PluginBuilds tells which plugin build is on disk in a plugin folder now
// (*pluginbuild.Cache): "" when it can't tell. It must be cheap and quick:
// pages ask it for every session.
type PluginBuilds interface {
	OnDisk(ctx context.Context, root string) string
}

// pluginOf is the plugin build on disk in the folder a session loaded its
// plugin from, and whether the session runs another (older) build. Both
// are zero when the session reported no folder or aimd can't tell.
func (s *Server) pluginOf(ctx context.Context, row store.Session) (onDisk string, outdated bool) {
	if s.opts.PluginBuilds == nil || row.PluginRoot == "" {
		return "", false
	}
	onDisk = s.opts.PluginBuilds.OnDisk(ctx, row.PluginRoot)
	return onDisk, pluginbuild.Outdated(row.PluginBuild, onDisk)
}

// oldPluginTitle is the Older plugin tag's hover text.
func oldPluginTitle(build, onDisk string) string {
	return "It runs plugin " + build + "; the plugin on disk is " + onDisk + ". Restart the session to update."
}

// pluginNotReported says why a session shows no build: it reported its
// work (so a plugin runs there) but no build, as plugins from before the
// build was reported do. A session the plugin never reported from gets "".
func pluginNotReported(row store.Session) string {
	if row.PluginBuild != "" || row.LastEventAt.IsZero() {
		return ""
	}
	return "Not reported: a plugin from before builds were reported, or its folder isn't a git checkout. Restart the session to update."
}

// olderPlugins counts the live sessions the Sessions page lists (headless
// runs and expired jobs aside) that run an older plugin than their folder
// holds now.
func (s *Server) olderPlugins(ctx context.Context, sn *snapshot) int {
	n := 0
	for _, row := range sn.sessions {
		if row.Headless || sn.jobExpired(row) {
			continue
		}
		if _, old := s.pluginOf(ctx, row); old {
			n++
		}
	}
	return n
}

// versionsCard is the Settings page's Versions card: what this Mac runs.
type versionsCard struct {
	// Aimd is aimd's version, Started how long ago it started.
	Aimd, Started string
	// Plugins is the plugin build on disk in each folder a live session
	// loaded the plugin from.
	Plugins []pluginFolder
	// NoPlugins says why Plugins is empty.
	NoPlugins string
}

// pluginFolder is one plugin folder and its sessions.
type pluginFolder struct {
	Folder, FolderFull string
	// Build is the build on disk there now ("" when aimd can't tell).
	Build string
	// Sessions says how many live sessions loaded the plugin from it, and
	// how many of them run an older build.
	Sessions string
}

// versionsOf fills the Versions card.
func (s *Server) versionsOf(ctx context.Context, sn *snapshot) versionsCard {
	st := sn.status
	v := versionsCard{Aimd: cmp.Or(st.Version, "unknown")}
	if !st.StartedAt.IsZero() {
		v.Started = ago(sn.now, st.StartedAt)
	}
	type tally struct{ n, old int }
	byRoot := map[string]*tally{}
	var roots []string
	for _, row := range sn.sessions {
		if row.PluginRoot == "" || row.Headless {
			continue
		}
		t := byRoot[row.PluginRoot]
		if t == nil {
			t = &tally{}
			byRoot[row.PluginRoot] = t
			roots = append(roots, row.PluginRoot)
		}
		t.n++
		if _, old := s.pluginOf(ctx, row); old {
			t.old++
		}
	}
	slices.Sort(roots)
	for _, root := range roots {
		t := byRoot[root]
		f := pluginFolder{Folder: s.displayPath(root), FolderFull: root, Sessions: plural(t.n, "live session", "live sessions")}
		if s.opts.PluginBuilds != nil {
			f.Build = s.opts.PluginBuilds.OnDisk(ctx, root)
		}
		if t.old > 0 {
			f.Sessions += " · " + strconv.Itoa(t.old) + " on an older build"
		}
		v.Plugins = append(v.Plugins, f)
	}
	if len(v.Plugins) == 0 {
		v.NoPlugins = "No live session has reported its plugin build yet. Sessions report it when they start."
	}
	return v
}
