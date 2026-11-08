package web

// The Settings page (GET /app/settings): alert switches, the inbox's
// timings, who merges each plan's PRs, how sessions become monitored,
// keyboard shortcuts on or off, and the theme. The settings live in
// aimd's store (store/settings.go, migration 0010), not in config.toml,
// and each takes effect where it is read:
//
//   - the count in the tab title, the chime, the shortcuts and the theme
//     are the shell's flags (appSettingsFlags), drawn on <html> with every
//     page and carried by the menu, so every open tab follows a change:
//     the script copies them onto <html> after each menu refresh;
//   - the theme is <html data-theme="dark|light"> when it forces one, so
//     the page loads in its colors; System draws none, and the
//     stylesheet's prefers-color-scheme query follows macOS;
//   - the chime and the highlight of new items also mark the inbox's rows
//     (fillInbox): data-chime on new Open permission prompts and
//     questions while the chime is on, the "new" class while the
//     highlight is;
//   - the back-to-Open delay and the Done history are the inbox's
//     timings (inboxSettings);
//   - macOS notifications are asked for by the notifier before each check
//     (internal/notify Options.On, wired in cmd/aimd);
//   - who merges a plan's PRs replaces the plan's Merges line before
//     monitor.Compute reads it (monitor.Source.Merges, wired in cmd/aimd),
//     so the PR rule, Needs you and GET /api/prs follow it.
//
// The page's switches and segmented controls are forms posting to
// /app/settings/set and /app/settings/merges, answered with the toast
// protocol (APP.md), each with an Undo. A change publishes the inbox topic
// on /api/stream, so open pages refresh their inbox rows and menu, and
// with the menu the shell's flags.
//
// GET /api/settings and POST /api/settings are the same for programs
// (docs/api.md), with the token on writes.

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

var appSettings = &appPage{
	pattern: "/app/settings",
	name:    "settings",
	title:   "Settings",
	panes:   1,
	// The plans: who merges each one's PRs.
	topics: []string{api.TopicPlans},
	tmpl:   parseApp("settings.html"),
	fill:   fillSettings,
	routes: settingsRoutes,
}

// SettingsStore is the part of *store.Store the settings need. A Store
// without it shows the defaults and can't change them.
type SettingsStore interface {
	Settings(ctx context.Context) (store.Settings, error)
	SetSettings(ctx context.Context, values map[string]string) (store.Settings, error)
	PlanMerges(ctx context.Context) (map[string]string, error)
	SetPlanMerges(ctx context.Context, slug, merges string) error
}

var _ SettingsStore = (*store.Store)(nil)

func (s *Server) settingsStore() (SettingsStore, bool) {
	st, ok := s.opts.Store.(SettingsStore)
	return st, ok
}

// settings are the stored settings, or the defaults when there is no
// settings store or it can't be read (logged): a page never fails over
// its settings.
func (s *Server) settings(ctx context.Context) store.Settings {
	st, ok := s.settingsStore()
	if !ok {
		return store.DefaultSettings()
	}
	set, err := st.Settings(ctx)
	if err != nil {
		s.log.Error("read settings", "err", err)
		return store.DefaultSettings()
	}
	return set
}

// appSettingsFlags are the settings every page's shell obeys: the count in
// the tab title, the chime, the keyboard shortcuts and the theme.
func (s *Server) appSettingsFlags(ctx context.Context) appFlags {
	set := s.settings(ctx)
	return appFlags{TabCount: set.TabCount, Chime: set.Chime, Keys: set.Shortcuts, Theme: set.Theme}
}

// inboxSettingsOf are the inbox's timings for the settings: the spec's
// defaults with the stored back-to-Open delay and Done history.
func inboxSettingsOf(set store.Settings) inbox.Settings {
	in := inbox.Defaults()
	in.BackToOpen, in.KeepDone = set.BackToOpen(), set.KeepDone()
	return in
}

// keptText says how long Done items are kept: "30 days".
func keptText(set inbox.Settings) string {
	return plural(int(set.KeepDone.Hours()/24), "day", "days")
}

// ---- View ----

type settingsView struct {
	// Unsaved says why changes can't be saved, when they can't.
	Unsaved              string
	Alerts               []settingSwitch
	BackToOpen, KeepDone settingChoice
	Snoozes              string
	Plans                []settingMerges
	PlansNote            string
	AllMonitored         bool
	Shortcuts            []settingSwitch
	// Theme is the Appearance card's System / Dark / Light.
	Theme settingChoice
}

// settingForm is what every settings form carries.
type settingForm struct {
	CSRF, Return string
}

// settingSwitch is an on/off row: its switch posts Next.
type settingSwitch struct {
	settingForm
	Key, Title, Desc string
	On               bool
	Next             string
	// Note, if set, is said under the description (notifications off in
	// config.toml).
	Note string
}

// settingChoice is a row with a segmented control.
type settingChoice struct {
	settingForm
	Key, Title, Desc string
	Options          []settingOption
}

type settingOption struct {
	Value, Label string
	On           bool
}

// settingMerges is one plan's who-merges row.
type settingMerges struct {
	settingForm
	Slug, Title string
	// Merges is who merges now; Line what the plan's line says.
	Merges, Line string
	Override     bool
	Note         string
}

// The switches' titles and descriptions, by key.
var settingText = map[string][2]string{
	store.SettingTabCount:      {"Count in the tab title", "The browser tab reads “(3) aim” while things wait on you."},
	store.SettingNotifications: {"macOS notifications", "From aimd, for monitored sessions only, and only for Open items."},
	store.SettingChime:         {"Chime", "A short chime for new permission prompts and questions, while the Needs you page is open."},
	store.SettingHighlight:     {"Highlight new items", "New inbox items stay highlighted until you open them."},
	store.SettingShortcuts:     {"Keyboard shortcuts", "j / k, Enter, g, s, d, u, /, 1–6 and ? anywhere outside a text field. Esc always closes a menu or dialog."},
	store.SettingBackToOpen:    {"After Go, back to Open if still waiting", "An item you clicked Go on returns to Open when its session is still waiting after this long."},
	store.SettingKeepDone:      {"Done history", "How long handled items stay in Done, with undo. A shorter history deletes the older ones at once."},
	store.SettingTheme:         {"Theme", "System follows your Mac's appearance, light or dark, and changes with it."},
}

// themeLabels are the theme choices' labels, by value.
var themeLabels = map[string]string{store.ThemeSystem: "System", store.ThemeDark: "Dark", store.ThemeLight: "Light"}

func onOff(b bool) string {
	if b {
		return store.On
	}
	return store.Off
}

func fillSettings(s *Server, r *http.Request, sn *snapshot, d *appData) error {
	ctx := r.Context()
	d.Summary = "Alerts, inbox and plans"
	set := s.settings(ctx)
	form := settingForm{CSRF: d.CSRF, Return: d.Return}
	v := &settingsView{}
	d.View = v
	if _, ok := s.settingsStore(); !ok {
		v.Unsaved = "This aimd has no store for settings: these are the defaults, and they can't be changed here."
	}

	toggle := func(key string) settingSwitch {
		on := set.Value(key) == store.On
		t := settingText[key]
		return settingSwitch{settingForm: form, Key: key, Title: t[0], Desc: t[1], On: on, Next: onOff(!on)}
	}
	for _, k := range []string{store.SettingTabCount, store.SettingNotifications, store.SettingChime, store.SettingHighlight} {
		sw := toggle(k)
		if k == store.SettingNotifications && s.opts.NotifyDisabled {
			sw.Note = "config.toml turns them off (notify.enabled = false), whatever this says."
		}
		v.Alerts = append(v.Alerts, sw)
	}
	v.Shortcuts = []settingSwitch{toggle(store.SettingShortcuts)}

	choice := func(key string, choices []int, unit string, now int) settingChoice {
		t := settingText[key]
		c := settingChoice{settingForm: form, Key: key, Title: t[0], Desc: t[1]}
		for _, n := range choices {
			c.Options = append(c.Options, settingOption{Value: strconv.Itoa(n), Label: strconv.Itoa(n) + " " + unit, On: n == now})
		}
		return c
	}
	v.BackToOpen = choice(store.SettingBackToOpen, store.BackToOpenChoices, "min", set.BackToOpenMinutes)
	v.KeepDone = choice(store.SettingKeepDone, store.KeepDoneChoices, "days", set.KeepDoneDays)
	var snoozes []string
	for _, c := range inboxSettingsOf(set).Snoozes() {
		snoozes = append(snoozes, strings.ToLower(c.Label[:1])+c.Label[1:])
	}
	v.Snoozes = strings.Join(snoozes, " · ")

	rows, note := s.mergesRows(ctx)
	v.PlansNote = note
	for _, m := range rows {
		v.Plans = append(v.Plans, settingMerges{settingForm: form, Slug: m.Plan, Title: m.Title, Merges: m.Merges,
			Line: m.Line, Override: m.Override, Note: mergesNote(m)})
	}
	v.AllMonitored = s.opts.Monitor != nil && s.opts.Monitor.Mode == monitor.ModeAll

	t := settingText[store.SettingTheme]
	v.Theme = settingChoice{settingForm: form, Key: store.SettingTheme, Title: t[0], Desc: t[1]}
	for _, th := range store.ThemeChoices {
		v.Theme.Options = append(v.Theme.Options, settingOption{Value: th, Label: themeLabels[th], On: th == set.Theme})
	}
	return nil
}

// mergesNote says where a plan's who-merges comes from.
func mergesNote(m api.PlanMerges) string {
	line := map[string]string{store.MergesYou: "you merge", store.MergesLead: "its lead session merges"}[m.Line]
	if m.Override {
		return "Overridden here. The plan's Merges line says " + line + "."
	}
	return "From the plan's Merges line: " + line + "."
}

// mergesOverrides are the Settings page's who-merges overrides, by
// lower-case plan slug; none without a settings store, or when they can't
// be read (logged).
func (s *Server) mergesOverrides(ctx context.Context) map[string]string {
	st, ok := s.settingsStore()
	if !ok {
		return map[string]string{}
	}
	over, err := st.PlanMerges(ctx)
	if err != nil {
		s.log.Error("settings plan merges", "err", err)
		return map[string]string{}
	}
	return over
}

// mergesOf is who merges a plan's PRs: its Merges line (line), or the
// override in over (mergesOverrides).
func mergesOf(slug, line string, over map[string]string) api.PlanMerges {
	l := store.MergesYou
	if strings.EqualFold(line, store.MergesLead) {
		l = store.MergesLead
	}
	m := api.PlanMerges{Plan: slug, Merges: l, Line: l}
	if o := over[strings.ToLower(slug)]; o != "" {
		m.Merges, m.Override = o, true
	}
	return m
}

// mergesRows are who merges each registered plan's PRs, in registration
// order, or a note saying why there are none.
func (s *Server) mergesRows(ctx context.Context) ([]api.PlanMerges, string) {
	if s.opts.Plans == nil {
		return []api.PlanMerges{}, "Plans aren't available."
	}
	list, err := s.opts.Plans.Plans(ctx)
	if err != nil {
		s.log.Error("settings plans", "err", err)
		return []api.PlanMerges{}, "aimd couldn't read the plans."
	}
	over := s.mergesOverrides(ctx)
	rows := []api.PlanMerges{}
	for _, b := range list.Plans {
		m := mergesOf(b.Slug, b.Merges, over)
		m.Title = cmp.Or(b.Title, b.Slug)
		rows = append(rows, m)
	}
	if len(rows) == 0 {
		return rows, "No plans registered."
	}
	return rows, ""
}

// ---- Forms ----

// settingsRoutes adds the page's forms and the API.
func settingsRoutes(s *Server) {
	s.mux.HandleFunc("POST /app/settings/set", s.settingsSetForm)
	s.mux.HandleFunc("POST /app/settings/merges", s.settingsMergesForm)
	s.mux.HandleFunc("GET /api/settings", s.getSettings)
	s.mux.HandleFunc("POST /api/settings", s.postSettings)
}

// settingsFail is a change refused or failed: the status and what to say.
type settingsFail struct {
	status int
	msg    string
}

// badSetting is an ErrBadSetting's message without the store's prefix.
func badSetting(err error) string {
	return "Can't save that: " + strings.TrimPrefix(err.Error(), store.ErrBadSetting.Error()+": ") + "."
}

// setSettings stores values (key → stored form), all or none, and tells
// open pages. It returns the settings before and after.
func (s *Server) setSettings(ctx context.Context, values map[string]string) (before, after store.Settings, fail *settingsFail) {
	st, ok := s.settingsStore()
	if !ok {
		return before, after, &settingsFail{http.StatusNotFound, "This aimd has no store for settings."}
	}
	before, err := st.Settings(ctx)
	if err == nil {
		after, err = st.SetSettings(ctx, values)
	}
	switch {
	case errors.Is(err, store.ErrBadSetting):
		return before, after, &settingsFail{http.StatusBadRequest, badSetting(err)}
	case err != nil:
		s.log.Error("save settings", "err", err)
		return before, after, &settingsFail{http.StatusInternalServerError, "aimd couldn't save that; see aimd.log."}
	}
	// The inbox's rows show the chime and the highlight, its footer the
	// Done history; and the menu refreshes with it.
	s.opts.Hub.Publish(api.TopicInbox)
	return before, after, nil
}

// setMerges stores who merges a plan's PRs ("" drops the override), runs
// an inbox pass so a PR that stops or starts needing you moves at once,
// and tells open pages.
func (s *Server) setMerges(ctx context.Context, slug, merges string) *settingsFail {
	st, ok := s.settingsStore()
	if !ok {
		return &settingsFail{http.StatusNotFound, "This aimd has no store for settings."}
	}
	err := st.SetPlanMerges(ctx, slug, merges)
	switch {
	case errors.Is(err, store.ErrBadSetting):
		return &settingsFail{http.StatusBadRequest, badSetting(err)}
	case errors.Is(err, store.ErrNotFound):
		return &settingsFail{http.StatusNotFound, "No plan " + strconv.Quote(slug) + " is registered."}
	case err != nil:
		s.log.Error("save plan merges", "plan", slug, "err", err)
		return &settingsFail{http.StatusInternalServerError, "aimd couldn't save that; see aimd.log."}
	}
	if _, err := s.SyncInbox(ctx); err != nil {
		s.log.Error("inbox sync", "err", err)
	}
	s.opts.Hub.Publish(api.TopicPRs)
	return nil
}

// settingsSetForm is POST /app/settings/set: key and value (its stored
// form). An Undo posts the old value back with undo set.
func (s *Server) settingsSetForm(w http.ResponseWriter, r *http.Request) {
	ret := appReturn(r)
	key, value := formValue(r, "key"), formValue(r, "value")
	before, after, fail := s.setSettings(r.Context(), map[string]string{key: value})
	if fail != nil {
		s.appRefuse(w, r, fail.status, ret, fail.msg)
		return
	}
	t := &appToast{Text: settingToast(key, after)}
	if old := before.Value(key); formValue(r, "undo") == "" && old != value {
		t.Undo = &appUndo{Action: "/app/settings/set", Fields: map[string]string{"key": key, "value": old, "return": ret, "undo": "1"}}
	}
	s.appReply(w, r, ret, t)
}

// settingToast says what a setting is now.
func settingToast(key string, set store.Settings) string {
	switch key {
	case store.SettingBackToOpen:
		return "After Go, back to Open after " + plural(set.BackToOpenMinutes, "minute", "minutes")
	case store.SettingKeepDone:
		return "Done items are kept " + plural(set.KeepDoneDays, "day", "days")
	case store.SettingTheme:
		if set.Theme == store.ThemeSystem {
			return "Theme: System, following your Mac"
		}
		return "Theme: " + themeLabels[set.Theme]
	}
	return settingText[key][0] + ": " + set.Value(key)
}

// settingsMergesForm is POST /app/settings/merges: plan and merges (you,
// lead, or "" to go back to the plan's line).
func (s *Server) settingsMergesForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ret := appReturn(r)
	slug, merges := formValue(r, "plan"), formValue(r, "merges")
	rows, _ := s.mergesRows(ctx)
	i := slices.IndexFunc(rows, func(m api.PlanMerges) bool { return strings.EqualFold(m.Plan, slug) })
	if i < 0 {
		s.appRefuse(w, r, http.StatusNotFound, ret, "No plan "+strconv.Quote(slug)+" is registered.")
		return
	}
	was := rows[i]
	if fail := s.setMerges(ctx, was.Plan, merges); fail != nil {
		s.appRefuse(w, r, fail.status, ret, fail.msg)
		return
	}
	now := was
	now.Merges, now.Override = cmp.Or(merges, was.Line), merges != ""
	t := &appToast{Text: mergesToast(now)}
	old := ""
	if was.Override {
		old = was.Merges
	}
	if formValue(r, "undo") == "" && old != merges {
		t.Undo = &appUndo{Action: "/app/settings/merges", Fields: map[string]string{"plan": was.Plan, "merges": old, "return": ret, "undo": "1"}}
	}
	s.appReply(w, r, ret, t)
}

// mergesToast says who merges a plan's PRs now.
func mergesToast(m api.PlanMerges) string {
	who := map[string]string{store.MergesYou: "you merge its PRs", store.MergesLead: "its lead session merges its PRs"}[m.Merges]
	if m.Override {
		return m.Title + ": " + who + " (an override)"
	}
	return m.Title + ": back to the plan's line, " + who
}

// ---- API ----

// settingsJSON is the settings as the API gives them.
func (s *Server) settingsJSON(ctx context.Context) api.Settings {
	set := s.settings(ctx)
	rows, _ := s.mergesRows(ctx)
	return api.Settings{
		TabCount: set.TabCount, Notifications: set.Notifications, Chime: set.Chime, Highlight: set.Highlight,
		BackToOpenMinutes: set.BackToOpenMinutes, KeepDoneDays: set.KeepDoneDays, Shortcuts: set.Shortcuts,
		Theme: set.Theme, Merges: rows,
	}
}

// getSettings is GET /api/settings.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsJSON(r.Context()))
}

// postSettings is POST /api/settings: the settings in the body change,
// all of them or, on a refusal, none; then who merges each plan named.
func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req api.SettingsUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	values := map[string]string{}
	flag := func(key string, b *bool) {
		if b != nil {
			values[key] = onOff(*b)
		}
	}
	flag(store.SettingTabCount, req.TabCount)
	flag(store.SettingNotifications, req.Notifications)
	flag(store.SettingChime, req.Chime)
	flag(store.SettingHighlight, req.Highlight)
	flag(store.SettingShortcuts, req.Shortcuts)
	if req.BackToOpenMinutes != nil {
		values[store.SettingBackToOpen] = strconv.Itoa(*req.BackToOpenMinutes)
	}
	if req.KeepDoneDays != nil {
		values[store.SettingKeepDone] = strconv.Itoa(*req.KeepDoneDays)
	}
	if req.Theme != nil {
		values[store.SettingTheme] = *req.Theme
	}
	// Check every change before making any.
	var check store.Settings
	for k, v := range values {
		if err := check.Set(k, v); err != nil {
			writeError(w, http.StatusBadRequest, badSetting(err))
			return
		}
	}
	rows, _ := s.mergesRows(ctx)
	merges := map[string]string{}
	for _, slug := range slices.Sorted(maps.Keys(req.Merges)) {
		v := ""
		if p := req.Merges[slug]; p != nil {
			v = *p
		}
		if v != "" && v != store.MergesYou && v != store.MergesLead {
			writeError(w, http.StatusBadRequest, "merges."+slug+" is you, lead, or empty to use the plan's line")
			return
		}
		i := slices.IndexFunc(rows, func(m api.PlanMerges) bool { return strings.EqualFold(m.Plan, slug) })
		if i < 0 {
			writeError(w, http.StatusNotFound, "no plan "+strconv.Quote(slug)+" is registered")
			return
		}
		merges[rows[i].Plan] = v
	}
	if len(values) > 0 {
		if _, _, fail := s.setSettings(ctx, values); fail != nil {
			writeError(w, fail.status, fail.msg)
			return
		}
	}
	for _, slug := range slices.Sorted(maps.Keys(merges)) {
		if fail := s.setMerges(ctx, slug, merges[slug]); fail != nil {
			writeError(w, fail.status, fail.msg)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.settingsJSON(ctx))
}
