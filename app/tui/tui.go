package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/candy-tools/dibs/app/metainfo"
	"github.com/candy-tools/dibs/internal/config"
	"github.com/candy-tools/dibs/internal/ident"
	"github.com/candy-tools/dibs/internal/lifecycle"
	"github.com/candy-tools/dibs/internal/localstat"
	"github.com/candy-tools/dibs/internal/sanity"
	"github.com/candy-tools/dibs/internal/status"
	"github.com/candy-tools/dibs/libs/threewayrsync"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type mode int

const (
	modeMain mode = iota
	modeForm
	modeConfirm
	modeSettings
	modeServers
	modeServerForm
)

// mainSub selects what modeMain's top box shows. Enter (subList) reveals the
// selected profile's actions; esc (subActions) returns to the list.
type mainSub int

const (
	subList mainSub = iota
	subActions
)

// actPane selects which panel in the actions view (subActions) has keyboard
// focus: the Actions menu (default) or the scrollable Activity panel. Tab
// toggles between them; the focused panel is drawn with the accent border.
type actPane int

const (
	paneActions actPane = iota
	paneActivity
)

type model struct {
	path         string
	cfg          *config.Config
	checks       map[string]*sanity.Result
	version      string
	identity     string
	width        int
	height       int
	list         listModel
	mode         mode
	sub          mainSub
	pane         actPane // which panel is focused while sub == subActions
	form         formModel
	settings     settingsModel
	servers      listModel
	serverForm   serverFormModel
	profile      profileModel
	confirmName  string
	confirmKind  confirmKind
	confirmFocus confirmFocus
	err          error
	id           ident.Ident
	runner       lifecycle.Runner
	// checkoutSteal is the checkout dialog's "steal the lock" checkbox (the TUI
	// equivalent of checkout --force). It only exists when the dialog opened on
	// a foreign lock (confirmForeign) and resets on every open.
	checkoutSteal bool
	// confirmForeign records, when the checkout dialog opens, that the profile
	// is locked by someone else (or the marker is unreadable), so the dialog
	// shows the holder and the steal checkbox. Snapshotted at open time so
	// render and key handling agree even if a sanity refresh lands mid-dialog.
	confirmForeign bool
	// confirmResumable records, at checkout-dialog open, that a released resume
	// token exists for the profile (lifecycle.HasResumeToken): the dialog shows
	// an informational line and the confirmed checkout runs with Resume set —
	// adopting the kept local copy is automatic, not an option. Reset on every
	// open.
	confirmResumable bool
	// syncAllowDeletes and syncLocalWins are the sync dialog's checkboxes: the
	// TUI equivalents of sync --allow-deletes (off: planned deletes are skipped
	// and reported pending; a one-side wipe is refused either way) and --force
	// (off: sync stops on conflicts; on: conflicts resolve keeping the local
	// file). Both reset on every open.
	syncAllowDeletes bool
	syncLocalWins    bool
	checkinClean     bool // "delete local copy" checkbox in the check-in dialog
	// checkinAbandon is the check-in dialog's "abandon" checkbox: release the
	// lock without the in-sync verification (the TUI equivalent of --abandon).
	checkinAbandon bool
	// serverRefs carries the list of profiles referencing a server when the
	// confirmDeleteServer dialog opens.
	serverRefs []string
	// wipe carries the engine wipe-valve stop the confirmWipe dialog explains:
	// the side that would be emptied and how many files. Set when a sync result
	// carries a *threewayrsync.WouldWipeError; cleared when the dialog closes.
	wipe *threewayrsync.WouldWipeError
	// cancel aborts the in-flight streaming action (Sync/Checkout/Check-in): its
	// context feeds exec.CommandContext, so calling it kills the live rsync. It is
	// nil for Status (a pure file walk with nothing to kill) and once an action
	// finishes.
	cancel context.CancelFunc
	// actionSeq stamps each launched action; it is bumped on every launch and on
	// cancel. A result message whose seq no longer matches is a stale straggler
	// (from a canceled or superseded run) and is dropped, so it can't overwrite the
	// "Canceled." state or a freshly started action.
	actionSeq int
}

// Run loads the config at path and starts the interactive TUI.
func Run(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	p := tea.NewProgram(newModel(path, cfg).withStartupSettings(), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func newModel(path string, cfg *config.Config) model {
	id, _ := ident.Resolve(cfg)
	m := model{
		path:     path,
		cfg:      cfg,
		version:  metainfo.Version,
		identity: identityString(cfg),
		mode:     modeMain,
		list:     newList(nil),
		checks:   make(map[string]*sanity.Result),
		id:       id,
		runner:   lifecycle.Runner{ToolVersion: metainfo.Version, RsyncBin: cfg.RsyncPath},
	}
	m.refreshList()
	return m
}

// withStartupSettings opens the settings dialog when no identity is configured,
// so the user is prompted to set one on start rather than silently defaulting to
// $USER@$HOSTNAME. newModel itself always starts on the main view; this is a
// separate step applied by Run so the initial mode stays predictable for tests.
func (m model) withStartupSettings() model {
	if m.cfg.Identity == "" {
		m.settings = newSettings(m.cfg)
		m.settings.mandatory = true
		m.mode = modeSettings
	}
	return m
}

// identityString is the header's right-hand text: the configured identity, or
// "$USER@$HOSTNAME" as the default.
func identityString(cfg *config.Config) string {
	id, err := ident.Resolve(cfg)
	if err != nil {
		return "unknown"
	}
	return id.By
}

// profileID returns the stable ID of a named profile ("" when the profile is
// unknown or predates IDs — the marker ownership check then falls back to
// identity+host).
func (m model) profileID(name string) string { return m.cfg.Profiles[name].ID }

func (m *model) refreshList() {
	names := make([]string, 0, len(m.cfg.Profiles))
	for name := range m.cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	m.list.setNames(names)
}

func (m *model) resize(ws tea.WindowSizeMsg) {
	m.width = ws.Width
	m.height = ws.Height
	m.form.setWidth(ws.Width)
	m.form.termHeight = ws.Height
	if m.form.browsing {
		m.form.picker.setHeight(m.form.pickerHeight())
	}
	if m.form.browsingRemote {
		m.form.remote.height = m.form.pickerHeight()
		m.form.remote.ensureVisible()
	}
	if m.form.browsingServer {
		m.form.serverPick.height = m.form.pickerHeight()
		m.form.serverPick.ensureVisible()
	}
	m.settings.setWidth(ws.Width)
	m.settings.termHeight = ws.Height
	if m.settings.browsing {
		m.settings.picker.setHeight(m.settings.pickerHeight())
	}
	m.serverForm.setWidth(ws.Width)
}

func (m model) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.cfg.Profiles)+2)
	for name := range m.cfg.Profiles {
		cmds = append(cmds, sanityCmd(m.cfg, name, m.cfg.RsyncPath))
	}
	// Verify the rsync binary up front so a macOS openrsync (or a stale override)
	// surfaces as a settings dialog at startup, not a cryptic mid-sync failure.
	cmds = append(cmds, rsyncCheckCmd(m.cfg.RsyncPath))
	// When the settings dialog auto-opens (no identity configured), blink its cursor.
	if m.mode == modeSettings {
		cmds = append(cmds, textinput.Blink)
	}
	return tea.Batch(cmds...)
}

// handleCtrlC routes the global Ctrl+C. While an action runs, a hard quit
// would orphan the live rsync (its context is never canceled on Quit), so it
// opens the same cancel confirm as Esc; while the cancel's kill escalation is
// already winding the run down there is nothing further to cancel and the key
// is inert — the terminal result is the only way out. Elsewhere it stays an
// immediate quit.
func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	if !m.running() {
		return m, tea.Quit
	}
	if !m.profile.canceling && m.mode != modeConfirm {
		m.openCancelConfirm()
	}
	return m, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "ctrl+c" {
		return m.handleCtrlC()
	}
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.resize(ws)
		return m, nil
	}
	if res, ok := msg.(statusResultMsg); ok {
		m.applyStatusResult(res)
		return m, nil
	}
	if res, ok := msg.(localStatResultMsg); ok {
		m.applyLocalStatResult(res)
		return m, nil
	}
	if res, ok := msg.(rsyncCheckMsg); ok {
		m.applyRsyncCheck(res)
		return m, nil
	}
	if res, ok := msg.(cylonTickMsg); ok {
		return m.applyCylonTick(res)
	}
	if res, ok := msg.(sanityResultMsg); ok {
		r := res.result
		m.checks[res.name] = &r
		// If this is the open profile, the refreshed state may have changed the
		// visible action list's length (e.g. a completed checkout swaps
		// [Checkout] for [Status, Sync, Check-in]), so keep the cursor in range.
		if m.sub == subActions && m.profile.name == res.name {
			m.profile.clampCursor(len(visibleActions(&r, m.id, m.profileID(res.name))))
		}
		return m, nil
	}
	if res, ok := msg.(syncEventMsg); ok {
		m.applySyncEvent(res)
		// Keep draining until the terminal actionResultMsg, regardless of whether
		// this event was for the open profile (see waitForMsg).
		return m, waitForMsg(res.ch)
	}
	if res, ok := msg.(actionResultMsg); ok {
		return m.handleActionResult(res)
	}
	switch m.mode {
	case modeForm:
		return m.updateForm(msg)
	case modeConfirm:
		return m.updateConfirm(msg)
	case modeSettings:
		return m.updateSettings(msg)
	case modeServers:
		return m.updateServers(msg)
	case modeServerForm:
		return m.updateServerForm(msg)
	default:
		return m.updateMain(msg)
	}
}

// handleActionResult applies a mutating action's terminal result and issues
// the follow-up refreshes (sanity mark, Contents re-scan) it warrants.
func (m model) handleActionResult(res actionResultMsg) (tea.Model, tea.Cmd) {
	m.applyActionResult(res)
	// A canceled or superseded action's terminal result is a straggler: skip the
	// sanity/Contents refresh it would otherwise trigger (applyActionResult has
	// already dropped its display).
	if res.seq != m.actionSeq {
		return m, nil
	}
	p := m.cfg.Profiles[res.name]
	// Refresh the sanity mark since the marker changed.
	cmds := []tea.Cmd{sanityCmd(m.cfg, res.name, m.cfg.RsyncPath)}
	// A successful mutating action changed the local tree, so re-scan it to
	// refresh the Contents summary in the Details box. Only while still on the
	// profile view — a released check-in has returned to the list — and never
	// for a dry run, which wrote nothing. The Activity panel keeps showing the
	// applied result; only the Details Contents block is refreshed.
	if res.err == nil && !res.report.DryRun && m.sub == subActions && m.profile.name == res.name {
		m.profile.scanning = true
		m.profile.statErr = nil
		cmds = append(cmds, localStatCmd(res.name, p, m.actionSeq))
	}
	return m, tea.Batch(cmds...)
}

func (m model) updateMain(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.sub == subActions {
		return m.updateProfile(msg)
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "q", "esc":
		return m, tea.Quit
	case "enter":
		if name, ok := m.list.selected(); ok {
			return m.openProfile(name)
		}
		return m, nil
	case "i":
		return m.openSettings()
	case "v":
		return m.openServers()
	case "a":
		// A fresh profile starts with the default ignore list (file-manager
		// metadata droppings); the user can remove the rows in the form.
		return m.openForm("", config.Profile{Ignore: config.DefaultIgnore()})
	case "e":
		if name, ok := m.list.selected(); ok {
			return m.openForm(name, m.cfg.Profiles[name])
		}
		return m, nil
	case "d":
		if name, ok := m.list.selected(); ok {
			m.confirmName = name
			m.confirmKind = confirmDelete
			m.confirmFocus = confirmFocusCancel
			m.mode = modeConfirm
		}
		return m, nil
	case "up", "w":
		m.list.moveUp()
		return m, nil
	case "down", "s":
		m.list.moveDown()
		return m, nil
	}
	return m, nil
}

func (m model) openForm(origName string, p config.Profile) (tea.Model, tea.Cmd) {
	m.form = newForm(origName, p, m.cfg.Servers)
	m.form.setWidth(m.width)
	m.form.termHeight = m.height
	m.form.rsyncBin = m.cfg.RsyncPath
	m.form.setDefaultLocalRoot(m.cfg.DefaultLocalRoot)
	m.mode = modeForm
	return m, textinput.Blink
}

func (m model) openSettings() (tea.Model, tea.Cmd) {
	m.settings = newSettings(m.cfg)
	m.settings.setWidth(m.width)
	m.settings.termHeight = m.height
	m.mode = modeSettings
	return m, textinput.Blink
}

// updateSettings handles the client-settings modal: focus movement
// (tab/shift+tab/↑↓ across fields, ←→ on the action row), esc to cancel,
// enter/space to activate, and typing into the focused input. Mirrors
// updateForm's key handling.
func (m model) updateSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.settings.browsing {
		var cmd tea.Cmd
		m.settings, cmd = m.settings.updatePicker(msg)
		return m, cmd
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.settings, cmd = m.settings.update(msg)
		return m, cmd
	}
	dlr := defaultLocalRootIdx()
	switch key.String() {
	case "esc":
		return m.cancelSettings()
	case "tab", "down":
		return m, m.settings.focusNext()
	case "shift+tab", "up":
		return m, m.settings.focusPrev()
	case "left":
		switch {
		case m.settings.focus == m.settings.cancelSlot():
			return m, m.settings.setFocus(m.settings.saveSlot())
		case m.settings.focusKind() == stBrowse:
			return m, m.settings.setFocus(m.settings.slotIndexForField(dlr))
		}
	case "right":
		switch {
		case m.settings.focus == m.settings.saveSlot():
			return m, m.settings.setFocus(m.settings.cancelSlot())
		case m.settings.onInput() && m.settings.focusField() == dlr && m.settings.atInputEnd():
			return m, m.settings.setFocus(m.settings.browseSlot())
		}
	case "enter":
		if mm, cmd, handled := m.activateSettingsFocus(); handled {
			return mm, cmd
		}
		return m.submitSettings() // on an input: submit
	case " ":
		if mm, cmd, handled := m.activateSettingsFocus(); handled {
			return mm, cmd
		}
		// On an input: fall through to type the space.
	}
	var cmd tea.Cmd
	m.settings, cmd = m.settings.update(msg)
	return m, cmd
}

// activateSettingsFocus performs the action for the focused button — Save
// submits, Cancel closes, Browse opens the directory picker — returning
// handled=false when focus is on a plain input (which the caller then treats as
// submit for enter, or a typed character for space).
func (m model) activateSettingsFocus() (tea.Model, tea.Cmd, bool) {
	switch m.settings.focusKind() {
	case stSave:
		mm, cmd := m.submitSettings()
		return mm, cmd, true
	case stCancel:
		mm, cmd := m.cancelSettings()
		return mm, cmd, true
	case stBrowse:
		return m, m.settings.openPicker(), true
	}
	return m, nil, false
}

// cancelSettings leaves the settings modal: it quits the app for the mandatory
// first-run dialog (no valid config to return to), otherwise returns to the main
// view discarding edits.
func (m model) cancelSettings() (tea.Model, tea.Cmd) {
	if m.settings.mandatory {
		return m, tea.Quit
	}
	m.mode = modeMain
	return m, nil
}

// submitSettings writes the edited client settings to disk. On failure it
// restores the previous values so in-memory state never diverges from disk and
// keeps the modal open with an error. On success it refreshes the header
// identity and re-resolves m.id so later checkouts record the new identity.
func (m model) submitSettings() (tea.Model, tea.Cmd) {
	if err := m.settings.validate(); err != nil {
		m.settings.err = err.Error()
		return m, nil
	}
	prev := *m.cfg
	m.settings.apply(m.cfg)
	if err := config.Save(m.path, m.cfg); err != nil {
		for _, f := range settingsFields {
			f.set(m.cfg, f.get(&prev))
		}
		m.settings.err = "save failed: " + err.Error()
		return m, nil
	}
	m.identity = identityString(m.cfg)
	m.id, _ = ident.Resolve(m.cfg)
	m.runner.RsyncBin = m.cfg.RsyncPath
	m.mode = modeMain
	m.err = nil
	// Re-verify the rsync binary: an empty override falls back to a PATH rsync
	// that may itself be broken, so a save must re-surface the dialog if so.
	return m, rsyncCheckCmd(m.cfg.RsyncPath)
}

func (m model) openProfile(name string) (tea.Model, tea.Cmd) {
	m.profile = newProfileView(name)
	m.sub = subActions
	m.pane = paneActions // always open focused on the action list
	// Refresh the sanity mark so action-row gating reflects the current on-disk
	// checkout state rather than whatever was cached at startup.
	return m, sanityCmd(m.cfg, name, m.cfg.RsyncPath)
}

// statusResultMsg carries a background Status compute back into Update. name
// identifies the profile it ran for, so a stale result from a profile the user
// has since left is ignored.
type statusResultMsg struct {
	name string
	seq  int
	st   status.ProfileStatus
	err  error
}

// cylonTickMsg drives the indeterminate progress bar's bouncing eye; seq
// stamps it so a canceled or superseded run's ticks are dropped.
type cylonTickMsg struct{ seq int }

// cylonTick schedules the next eye step. ~20 fps: a fast sweep — the pause
// that sells the bounce comes from the edge dwell in advance, not the tick
// rate.
func cylonTick(seq int) tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return cylonTickMsg{seq: seq} })
}

// applyCylonTick advances the indeterminate bar's bouncing eye one step and
// re-arms the tick. A stale tick (canceled or superseded run), a finished run,
// or a determinate bar (planned totals known) stops the loop: no re-arm.
func (m model) applyCylonTick(res cylonTickMsg) (tea.Model, tea.Cmd) {
	p := m.profile.progress
	if res.seq != m.actionSeq || !m.profile.acting || p == nil || p.totals != nil {
		return m, nil // stale, or the run is over, or the bar is determinate
	}
	w := m.width
	if w == 0 {
		w = 80 // matches mainView's pre-resize fallback
	}
	p.advance(progressBarW(w, p))
	return m, cylonTick(res.seq)
}

// statusCmd runs status.Compute off the UI thread and delivers the outcome as a
// statusResultMsg. seq is the launch stamp so a result abandoned by a cancel (or
// a newer run) can be recognised and dropped in applyStatusResult.
func statusCmd(name string, p config.Profile, seq int, rsyncBin string) tea.Cmd {
	return func() tea.Msg {
		st, err := status.Compute(context.Background(), name, p, rsyncBin)
		return statusResultMsg{name: name, seq: seq, st: st, err: err}
	}
}

// applyStatusResult stores a Status compute's outcome on the open profile. A
// result is ignored unless the actions view is still showing that same profile,
// so a slow compute can never overwrite newer state.
func (m *model) applyStatusResult(res statusResultMsg) {
	if res.seq != m.actionSeq || m.sub != subActions || m.profile.name != res.name {
		return // stale straggler (canceled or superseded), or a since-left profile
	}
	if m.cancel != nil { // no-op for Status (nil), but keeps the field hygienic
		m.cancel()
		m.cancel = nil
	}
	m.profile.checking = false
	m.pane = paneActions // the run is over; hand focus back to the action list
	if res.err != nil {
		m.profile.err = res.err
		m.profile.result = nil
		return
	}
	m.profile.err = nil
	st := res.st
	m.profile.result = &st
}

// localStatResultMsg carries a background local file-stat scan back into Update,
// guarded by profile name so a stale result from a since-left profile is ignored.
type localStatResultMsg struct {
	name  string
	seq   int
	stats localstat.Stats
	err   error
}

// localStatCmd runs localstat.Scan off the UI thread and delivers the outcome as
// a localStatResultMsg. It runs alongside statusCmd on the Status action and
// again after a mutating action to refresh the Contents summary. seq is the
// launch stamp so an abandoned scan's result is dropped in applyLocalStatResult.
func localStatCmd(name string, p config.Profile, seq int) tea.Cmd {
	return func() tea.Msg {
		stats, err := localstat.Scan(p)
		return localStatResultMsg{name: name, seq: seq, stats: stats, err: err}
	}
}

// applyLocalStatResult stores a local scan's outcome on the open profile,
// ignored unless the actions view is still showing that same profile so a slow
// scan can never overwrite newer state.
func (m *model) applyLocalStatResult(res localStatResultMsg) {
	if res.seq != m.actionSeq || m.sub != subActions || m.profile.name != res.name {
		return // stale straggler (canceled or superseded), or a since-left profile
	}
	m.profile.scanning = false
	if res.err != nil {
		m.profile.statErr = res.err
		m.profile.fileStats = nil
		return
	}
	m.profile.statErr = nil
	stats := res.stats
	m.profile.fileStats = &stats
}

// rsyncCheckMsg carries the startup rsync binary verification back into Update;
// a nil err means the binary is usable.
type rsyncCheckMsg struct {
	err error
}

// rsyncCheckCmd verifies the rsync binary (GNU rsync >= 3.1) off the UI thread.
func rsyncCheckCmd(bin string) tea.Cmd {
	return func() tea.Msg {
		_, err := (&threewayrsync.Syncer{Bin: bin}).CheckBinary(context.Background())
		return rsyncCheckMsg{err: err}
	}
}

// applyRsyncCheck surfaces a failed rsync binary check as the settings dialog,
// where the user can point the rsync path override at a working binary. An
// already-open settings dialog (e.g. the mandatory first-run one) just gets the
// error message; a passing check never touches the UI.
func (m *model) applyRsyncCheck(res rsyncCheckMsg) {
	if res.err == nil {
		return
	}
	if m.mode != modeSettings {
		m.settings = newSettings(m.cfg)
		m.settings.setWidth(m.width)
		m.settings.termHeight = m.height
		m.mode = modeSettings
	}
	m.settings.err = res.err.Error()
}

// sanityResultMsg carries one profile's lightweight sanity check back into Update.
type sanityResultMsg struct {
	name   string
	result sanity.Result
}

// sanityCmd resolves the named profile's server reference and runs the
// stat-only sanity.Check off the UI thread. A resolution failure (unknown
// server, or an old embedded rsync profile) comes back as a Result carrying
// only ConfigErr, so the list surfaces it and offers no actions.
func sanityCmd(cfg *config.Config, name, rsyncBin string) tea.Cmd {
	return func() tea.Msg {
		p, err := cfg.ResolveProfile(name)
		if err != nil {
			return sanityResultMsg{name: name, result: sanity.Result{ConfigErr: err.Error()}}
		}
		return sanityResultMsg{name: name, result: sanity.Check(p, rsyncBin)}
	}
}

// actionResultMsg carries a mutating action's outcome back into Update, guarded
// by profile name so a stale result from a since-left profile is ignored.
type actionResultMsg struct {
	name   string
	seq    int
	report lifecycle.Report
	err    error
}

// syncEventMsg carries one live applied change from a streaming action
// (Checkout, Sync, or Check-in) back into Update. ch is the same channel the
// action streams on, so Update can re-arm waitForMsg to drain the next message.
type syncEventMsg struct {
	name  string
	seq   int
	event lifecycle.Event
	ch    chan tea.Msg
}

// waitForMsg blocks on the streaming channel and returns the next message. It is
// re-issued after every syncEventMsg so the channel is drained to completion
// (through the terminal actionResultMsg), even if the user has navigated away —
// which keeps the producing goroutine from blocking forever on a full send.
func waitForMsg(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// checkoutCmd runs lifecycle.Runner.Checkout off the UI thread, streaming each
// pulled file live as a syncEventMsg and finishing with an actionResultMsg —
// the same shape as syncCmd/checkinCmd. ctx is the cancelable context whose
// cancel kills the run; seq stamps the messages so a canceled run's stragglers
// are dropped.
func checkoutCmd(ctx context.Context, r lifecycle.Runner, id ident.Ident, name string, p config.Profile, seq int, opts lifecycle.Options) tea.Cmd {
	return streamCmd(name, seq, func(o lifecycle.Options) (lifecycle.Report, error) {
		return r.Checkout(ctx, name, p, id, "", o)
	}, opts)
}

// streamCmd runs a streaming action (Checkout, Sync, or Check-in) on a
// background goroutine, streaming each applied change as a syncEventMsg on a
// channel and finishing with a terminal actionResultMsg. It returns the
// command that drains the first message; Update re-arms waitForMsg for the rest.
// seq stamps every emitted message so applySyncEvent/applyActionResult can drop
// the stragglers of a canceled or superseded run.
func streamCmd(name string, seq int, run func(opts lifecycle.Options) (lifecycle.Report, error), opts lifecycle.Options) tea.Cmd {
	ch := make(chan tea.Msg)
	opts.OnApply = func(e lifecycle.Event) { ch <- syncEventMsg{name: name, seq: seq, event: e, ch: ch} }
	go func() {
		rep, err := run(opts)
		ch <- actionResultMsg{name: name, seq: seq, report: rep, err: err}
		close(ch)
	}()
	return waitForMsg(ch)
}

// syncCmd runs lifecycle.Runner.Sync off the UI thread, streaming applied changes
// live and finishing with an actionResultMsg.
func syncCmd(ctx context.Context, r lifecycle.Runner, id ident.Ident, name string, p config.Profile, seq int, opts lifecycle.Options) tea.Cmd {
	return streamCmd(name, seq, func(o lifecycle.Options) (lifecycle.Report, error) {
		return r.Sync(ctx, name, p, id, "", o)
	}, opts)
}

// checkinCmd runs lifecycle.Runner.Checkin off the UI thread, streaming applied
// changes live and finishing with an actionResultMsg.
func checkinCmd(ctx context.Context, r lifecycle.Runner, id ident.Ident, name string, p config.Profile, seq int, opts lifecycle.Options) tea.Cmd {
	return streamCmd(name, seq, func(o lifecycle.Options) (lifecycle.Report, error) {
		return r.Checkin(ctx, name, p, id, o)
	}, opts)
}

// applyActionResult stores a mutating action's outcome on the open profile. A
// result is ignored unless the actions view is still showing that same profile,
// so a slow run can never overwrite newer state. The report is stored even on
// error — a conflict stop (*lifecycle.ConflictError) carries the conflicting
// paths in report.Conflicts, which renderStatus needs to show them instead of
// just a count-only error string. actionErr is still set so non-conflict
// failures (e.g. the remote root not being mounted) render as an error.
// applySyncEvent appends one live applied change to the open profile's list and
// auto-follows the scroll to the bottom so the newest row stays visible. A stale
// event from a since-left profile is ignored (the channel is still drained by the
// re-armed waitForMsg).
func (m *model) applySyncEvent(res syncEventMsg) {
	if res.seq != m.actionSeq || m.sub != subActions || m.profile.name != res.name {
		return // stale straggler (canceled or superseded), or a since-left profile
	}
	m.profile.applied = append(m.profile.applied, res.event)
	if m.profile.progress != nil {
		m.profile.progress.done.inc(res.event.Kind)
	}
	m.profile.statusScroll = m.statusMaxScroll()
}

func (m *model) applyActionResult(res actionResultMsg) {
	if res.seq != m.actionSeq {
		return // straggler from a canceled or superseded action
	}
	// The action finished on its own; call cancel to release the context's
	// resources (the process has already exited, so this only frees the watcher).
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.sub != subActions || m.profile.name != res.name {
		return
	}
	m.profile.acting = false
	m.pane = paneActions // the run is over; hand focus back to the action list
	// This terminal result is what the canceling state was waiting for: the
	// killed run has fully wound down, so exits are safe again. An error here is
	// the cancellation itself (context.Canceled, or rsync dying to the signal),
	// not a failure to report — show the Canceled note. No error means the run
	// beat the kill and completed: nothing was canceled, show the real result.
	if m.profile.canceling {
		m.profile.canceling = false
		if res.err != nil {
			m.profile.canceled = true
			return
		}
	}
	rep := res.report
	m.profile.actionReport = &rep
	m.profile.actionErr = res.err
	// The sync is over (finished, conflict-stopped, or failed): the progress
	// bar goes away, and the Status totals it fed from are stale now that the
	// trees changed — the next sync needs a fresh Status run for a real bar.
	if m.profile.progress != nil {
		m.profile.progress = nil
		m.profile.result = nil
	}
	// A sync stopped by the engine's wipe valve gets its own dialog: the flat
	// error text explains the CLI recovery, but in the TUI the natural follow-up
	// ("yes, really delete them") is one button press away. The dialog carries
	// the full explanation, so the Activity panel keeps only a short note — not
	// the CLI-oriented message with its flag names.
	var ww *threewayrsync.WouldWipeError
	if errors.As(res.err, &ww) {
		m.profile.actionErr = errors.New("sync stopped — no changes were written")
		m.profile.actionReport = nil // an empty "pull 0, push 0" summary would read as success
		m.wipe = ww
		m.confirmName = res.name
		m.confirmKind = confirmWipe
		m.confirmFocus = confirmFocusCancel // safe default: reaching "Delete" needs a move
		m.mode = modeConfirm
		return
	}
	// A completed check-in releases the profile, so its actions no longer apply.
	// Return to the profile list rather than lingering on that action view. On
	// error (e.g. a conflict stop) stay put so the failure stays on screen.
	if res.err == nil && rep.Action == "checkin" {
		m.sub = subList
	}
	// After a completed sync, park the actions cursor back on Status so the
	// natural follow-up (verifying the synced state) is one Enter away.
	if res.err == nil && rep.Action == "sync" {
		for i, a := range visibleActions(m.checks[res.name], m.id, m.profileID(res.name)) {
			if a == "Status" {
				m.profile.cursor = i
				break
			}
		}
	}
}

// running reports whether an operation is in flight on the open profile: a
// streaming mutation (Sync/Checkout/Check-in) or a Status compute. While
// running, the profile view is locked to monitoring the Activity panel and
// canceling — no other operation can be started.
func (m model) running() bool {
	return m.profile.acting || m.profile.checking
}

// openCancelConfirm opens the confirm dialog that guards stopping the
// in-flight action, focused on the safe "Keep running" button. Shared by
// Esc/q in the profile view and the Ctrl+C intercept.
func (m *model) openCancelConfirm() {
	m.confirmName = m.profile.name
	m.confirmKind = confirmCancel
	m.confirmFocus = confirmFocusCancel // safe default: reaching "Stop" needs a move
	m.mode = modeConfirm
}

// escProfile handles Esc in the profile actions view. While an action is in
// flight it opens the cancel confirm rather than silently leaving the work
// running in the background; otherwise it returns to the profile list. While
// the cancel's kill escalation is already in flight there is nothing left to
// cancel and leaving would orphan the dying rsync tree, so the key is inert.
func (m model) escProfile() (tea.Model, tea.Cmd) {
	if m.running() {
		if !m.profile.canceling {
			m.openCancelConfirm()
		}
		return m, nil
	}
	m.sub = subList
	return m, nil
}

func (m model) updateProfile(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	actions := visibleActions(m.checks[m.profile.name], m.id, m.profileID(m.profile.name))
	// While an action runs the view is locked to monitoring: only the Activity
	// scroll/filter keys are live, and esc/q open the cancel confirm. Tab (pane
	// switching) and enter (launching another operation) are ignored.
	if m.running() {
		switch key.String() {
		case "esc", "q":
			return m.escProfile()
		default:
			m.activityKey(key.String())
		}
		return m, nil
	}
	// Keys handled the same in both panes: Tab toggles focus and Esc leaves the
	// profile for the list (from either pane). q stays intentionally unbound
	// here — it only means "quit" on the plain profile list.
	switch key.String() {
	case "esc":
		return m.escProfile()
	case "tab", "shift+tab":
		if m.pane == paneActivity {
			m.pane = paneActions
		} else {
			m.pane = paneActivity
		}
		return m, nil
	}

	// Activity pane: the arrow/page keys scroll the status viewport and ←→
	// cycle the operation filter.
	if m.pane == paneActivity {
		m.activityKey(key.String())
		return m, nil
	}

	// Actions pane: the arrow keys move the action cursor; Enter runs it.
	switch key.String() {
	case "up", "w":
		m.profile.moveUp()
		return m, nil
	case "down", "s":
		m.profile.moveDown(len(actions))
		return m, nil
	case "enter":
		if m.profile.cursor >= len(actions) {
			return m, nil
		}
		return m.runSelectedAction(actions[m.profile.cursor])
	}
	return m, nil
}

// runSelectedAction handles Enter on an action row: Status runs immediately
// (a read-only compute), while every mutating action opens its confirm dialog
// with that dialog's option checkboxes reset to unchecked (safe) so a stale
// tick from a previous open can't carry into a new run.
func (m model) runSelectedAction(action string) (tea.Model, tea.Cmd) {
	switch action {
	case "Status":
		name := m.profile.name
		p, err := m.cfg.ResolveProfile(name)
		if err != nil {
			m.profile.actionErr = err
			return m, nil
		}
		m.profile.checking = true
		m.pane = paneActivity // lock focus to the Activity panel while it runs
		m.profile.err = nil
		m.profile.result = nil
		m.profile.statusScroll = 0
		m.profile.opFilter = ""
		// Drop any prior action report ("sync: N pulled") so the fresh
		// Status result isn't masked by it in renderStatus.
		m.profile.actionReport = nil
		m.profile.actionErr = nil
		m.profile.progress = nil
		m.profile.canceled = false
		m.profile.scanning = true
		m.profile.statErr = nil
		m.profile.fileStats = nil
		// Status is a pure file walk with no process to kill, so there is nothing
		// to cancel; a bumped actionSeq is enough to abandon its result on Esc.
		m.cancel = nil
		m.actionSeq++
		seq := m.actionSeq
		// Also refresh the sanity check so the Details existence marks
		// reflect the current on-disk state (e.g. a root created since the
		// profile was opened).
		return m, tea.Batch(
			statusCmd(name, p, seq, m.cfg.RsyncPath),
			localStatCmd(name, p, seq),
			sanityCmd(m.cfg, name, m.cfg.RsyncPath),
		)
	case "Checkout":
		m.confirmName = m.profile.name
		m.confirmKind = confirmCheckout
		m.confirmFocus = confirmFocusCancel // safe default: reaching "Check out" needs a move
		// Snapshot foreignness at open: a checked-out profile whose marker is
		// missing or held elsewhere shows the holder and the steal checkbox.
		r := m.checks[m.profile.name]
		m.confirmForeign = r != nil && r.CheckedOut &&
			(r.Marker == nil || !r.Marker.OwnedBy(m.id.By, m.id.Host, m.profileID(m.profile.name)))
		m.checkoutSteal = false
		// Snapshot the resume token too: with one present the confirmed checkout
		// automatically adopts the kept local copy (the dialog says so).
		m.confirmResumable = lifecycle.HasResumeToken(m.profile.name, m.cfg.Profiles[m.profile.name])
		m.mode = modeConfirm
	case "Sync":
		m.confirmName = m.profile.name
		m.confirmKind = confirmSync
		// Open focused on the first checkbox: a bare enter/space just toggles
		// it (harmless), so reaching the actual Run still needs a move.
		m.confirmFocus = confirmFocusAllowDeletes
		m.syncAllowDeletes = false
		m.syncLocalWins = false
		m.mode = modeConfirm
	case "Check-in":
		m.confirmName = m.profile.name
		m.confirmKind = confirmCheckin
		// Open focused on the first checkbox, as in the sync dialog.
		m.confirmFocus = confirmFocusAbandon
		m.checkinAbandon = false
		m.checkinClean = false
		m.mode = modeConfirm
	}
	return m, nil
}

func (m model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.form.browsing {
		var cmd tea.Cmd
		m.form, cmd = m.form.updatePicker(msg)
		return m, cmd
	}
	if m.form.browsingRemote {
		var cmd tea.Cmd
		m.form, cmd = m.form.updateRemotePicker(msg)
		return m, cmd
	}
	if m.form.browsingServer {
		var cmd tea.Cmd
		m.form, cmd = m.form.updateServerPicker(msg)
		return m, cmd
	}
	if res, ok := msg.(remoteListResultMsg); ok {
		// A listing that finished after the browser closed: seq-dropped.
		m.form.applyRemoteListResult(res)
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.form, cmd = m.form.updateInputs(msg)
		return m, cmd
	}
	if cmd, ok := m.form.navKey(key.String()); ok {
		return m, cmd
	}
	switch key.String() {
	case "esc":
		m.mode = modeMain
		return m, nil
	case "enter", " ":
		if mm, cmd, ok := m.activateFormSlot(); ok {
			return mm, cmd
		}
		// on a text input: enter does nothing, space falls through to be typed
	}
	var cmd tea.Cmd
	m.form, cmd = m.form.updateInputs(msg)
	return m, cmd
}

// activateFormSlot performs the focused slot's action for enter/space. ok is
// false on a text input, where neither key activates anything (enter is inert,
// space is typed).
func (m model) activateFormSlot() (tea.Model, tea.Cmd, bool) {
	switch m.form.focusKind() {
	case slotButton:
		return m, m.form.openBrowse(), true
	case slotTypeSel:
		m.form.cycleKind(1)
		return m, nil, true
	case slotServerSel:
		if m.form.serverSelIsModal() {
			return m, m.form.openServerPicker(), true
		}
		// Inline radio list: pick the focused server row (field is its index).
		if i := m.form.focusField(); i >= 0 {
			m.form.serverSel = i
		}
		return m, nil, true
	case slotRemove:
		return m, m.form.removeSubpath(m.form.focusField()), true
	case slotAdd:
		return m, m.form.addSubpath(), true
	case slotAddIgnore:
		return m, m.form.addIgnore(), true
	case slotSave:
		mm, cmd := m.submitForm()
		return mm, cmd, true
	case slotCancel:
		m.mode = modeMain
		return m, nil, true
	}
	return m, nil, false
}

func (m model) submitForm() (tea.Model, tea.Cmd) {
	name, p := m.form.values()
	// Gate the rsync kind: require a selected server
	if m.form.kind == remoteRsync {
		if len(m.form.serverNames) == 0 {
			m.form.err = "select a server (press v on the main view to add one)"
			return m, nil
		}
		if m.form.serverSel < 0 || m.form.serverSel >= len(m.form.serverNames) {
			m.form.err = "select a server (press v on the main view to add one)"
			return m, nil
		}
	}
	if err := validateProfile(m.cfg, m.form.origName, name, p); err != nil {
		m.form.err = err.Error()
		return m, nil
	}
	prev := cloneProfiles(m.cfg.Profiles)
	// The form never edits the ID: an edit keeps the profile's existing ID and
	// a new profile (or a hand-written one that predates IDs) gets a fresh one,
	// so checkout markers can tell profiles apart even on the same machine.
	if m.form.origName != "" {
		p.ID = m.cfg.Profiles[m.form.origName].ID
	}
	if p.ID == "" {
		p.ID = config.NewProfileID()
	}
	if m.form.origName != "" && m.form.origName != name {
		delete(m.cfg.Profiles, m.form.origName)
		delete(m.checks, m.form.origName)
	}
	m.cfg.Profiles[name] = p
	if err := commitProfiles(m.path, m.cfg, prev); err != nil {
		m.form.err = "save failed: " + err.Error()
		return m, nil
	}
	m.refreshList()
	m.mode = modeMain
	m.err = nil
	return m, sanityCmd(m.cfg, name, m.cfg.RsyncPath)
}

// cloneProfiles returns a shallow copy of p so a mutation can be snapshotted
// before a save and rolled back if the save fails.
func cloneProfiles(p map[string]config.Profile) map[string]config.Profile {
	out := make(map[string]config.Profile, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// commitProfiles saves cfg to path. On failure it restores prev into
// cfg.Profiles so in-memory state never diverges from what is on disk.
func commitProfiles(path string, cfg *config.Config, prev map[string]config.Profile) error {
	if err := config.Save(path, cfg); err != nil {
		cfg.Profiles = prev
		return err
	}
	return nil
}

func validateProfile(cfg *config.Config, origName, name string, p config.Profile) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	if name != origName {
		if _, exists := cfg.Profiles[name]; exists {
			return fmt.Errorf("profile %q already exists", name)
		}
	}
	if err := config.ValidateRoot(p.LocalRoot); err != nil {
		return fmt.Errorf("local root: %w", err)
	}
	// RULING 1: Branch on p.Server. When set, validate that the server exists
	// and the module is non-empty. Otherwise, validate RemoteRoot as before.
	if p.Server != "" {
		// Server-backed rsync profile
		if _, exists := cfg.Servers[p.Server]; !exists {
			return fmt.Errorf("unknown server %q", p.Server)
		}
		if strings.TrimSpace(p.RemoteModule) == "" {
			return fmt.Errorf("module is required")
		}
	} else {
		// URL-based profile (ssh://, local, or legacy rsync://)
		if err := config.ValidateRemoteRoot(p.RemoteRoot); err != nil {
			return fmt.Errorf("remote root: %w", err)
		}
	}
	for _, sub := range p.Subpaths {
		if err := config.ValidateSubpath(sub); err != nil {
			return fmt.Errorf("subpath %q: %w", sub, err)
		}
	}
	for _, pat := range p.Ignore {
		if err := config.ValidateIgnorePattern(pat); err != nil {
			return fmt.Errorf("ignore %q: %w", pat, err)
		}
	}
	return nil
}

func (m model) View() string {
	switch m.mode {
	case modeForm:
		return m.overlayModal(m.form.View())
	case modeConfirm:
		return m.overlayModal(confirmModal(m.confirmKind, m.confirmName, m.confirmParams(), m.width))
	case modeSettings:
		return m.overlayModal(m.settings.View())
	case modeServers:
		return m.overlayModal(m.serversView())
	case modeServerForm:
		return m.overlayModal(m.serverForm.View())
	default:
		return m.mainView(false)
	}
}

// mainView composes the header, the two columns, and the footer to exactly the
// terminal size. The left column stacks a top box — the profile list, or (once
// a profile's actions are revealed) the Actions menu — over Details; the right
// column is the Activity placeholder. When dim is true the top box renders
// unfocused (used as the dimmed backdrop behind a modal).
func (m model) mainView(dim bool) string {
	w, h := m.width, m.height
	if w == 0 {
		w, h = 80, 24 // pre-resize fallback so the view is never empty
	}
	bodyH := h - 2 // header + footer
	if bodyH < 3 {
		bodyH = 3
	}
	leftW := w / 3
	if leftW < 16 {
		leftW = 16
	}
	rightW := w - leftW

	// The top box (Profiles/Actions) takes a third of the body height; Details
	// gets the remaining two thirds so its roots/checkout/subpaths/contents fit.
	topH := bodyH / 3
	if topH < 3 {
		topH = 3
	}
	detailsH := bodyH - topH

	var topTitle, topBody, name string
	if m.sub == subActions {
		topTitle = "Actions"
		topBody = renderActions(m.profile.cursor, leftW-2, m.checks[m.profile.name], m.id, m.profileID(m.profile.name), m.running())
		name = m.profile.name
	} else {
		topTitle = "Profiles"
		topBody = m.list.view(leftW-2, topH-2)
		name, _ = m.list.selected()
	}
	// Resolve server-backed profiles for display so the Details box shows the
	// composed rsync:// URL rather than a blank RemoteRoot. Fall back to the
	// stored profile on resolve error so broken profiles still render (their
	// ConfigErr is surfaced in the Actions box).
	displayProfile := m.cfg.Profiles[name]
	if resolved, err := m.cfg.ResolveProfile(name); err == nil {
		displayProfile = resolved
	}
	detailsBody := renderDetails(name, displayProfile, m.checks[name], leftW-2)
	if m.sub == subActions {
		detailsBody += pendingBlock(m.profile.result)
		detailsBody += contentsBlock(m.profile.fileStats, m.profile.scanning, m.profile.statErr)
	}

	// The top box is focused except when the activity pane holds focus; the
	// Activity box is focused only then. Both go unfocused behind a modal (dim).
	topFocused := !dim && (m.sub != subActions || m.pane == paneActions)
	top := titledBox(topTitle, topBody, leftW, topH, topFocused)
	details := titledBox("Details", detailsBody, leftW, detailsH, false)
	left := lipgloss.JoinVertical(lipgloss.Left, top, details)
	activity := renderActivity()
	if m.sub == subActions {
		activity = renderStatus(m.profile, rightW-2)
		activity, _ = scrollWindow(activity, m.profile.statusScroll, bodyH-2)
	}
	activityFocused := !dim && m.sub == subActions && m.pane == paneActivity
	right := titledBox("Activity", activity, rightW, bodyH, activityFocused)
	panels := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	footer := renderFooter(w)
	if m.sub == subActions {
		footer = renderProfileFooter(w, m.pane == paneActivity, m.running(), m.profile.canceling)
		// A running sync owns the bottom row: counters + bar + the cancel hint.
		// Other streaming actions (checkout/check-in) never set progress.
		if m.profile.acting && m.profile.progress != nil {
			footer = renderProgressLine(w, m.profile.progress)
		}
	}
	view := renderHeader(w, m.version, m.identity) + "\n" + panels + "\n" + footer
	if m.err != nil {
		view += "\n" + errStyle.Render("save failed: "+m.err.Error())
	}
	return view
}

// overlayModal renders the main view dimmed (both panels unfocused) and
// composites box centered over it, so the modal reads as a floating window.
func (m model) overlayModal(box string) string {
	return placeCenter(m.mainView(true), box)
}

// scrollWindow returns the height visible lines of body starting at offset, and
// whether body is taller than height (i.e. scrolling applies). offset is clamped
// so the window never runs off the end; a body that already fits is returned
// whole with overflow=false.
func scrollWindow(body string, offset, height int) (string, bool) {
	if height < 1 {
		height = 1
	}
	lines := strings.Split(body, "\n")
	if len(lines) <= height {
		return body, false
	}
	max := len(lines) - height
	if offset > max {
		offset = max
	}
	if offset < 0 {
		offset = 0
	}
	return strings.Join(lines[offset:offset+height], "\n"), true
}

// activityGeometry returns the Activity box's inner width and height, mirroring
// mainView's layout math so scroll clamping in updateProfile stays in sync with
// what is actually rendered.
func (m model) activityGeometry() (innerWidth, innerHeight int) {
	w, h := m.width, m.height
	if w == 0 {
		w, h = 80, 24
	}
	bodyH := h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	leftW := w / 3
	if leftW < 16 {
		leftW = 16
	}
	rightW := w - leftW
	return rightW - 2, bodyH - 2
}

// step is a page scroll increment: the visible height, but at least one line so
// a tiny pane still scrolls.
func step(height int) int {
	if height < 1 {
		return 1
	}
	return height
}

// fastStep is the shift+arrow scroll increment: ten lines, a middle ground
// between the single-line arrows and the full-page PgUp/PgDn.
const fastStep = 10

// activityKey handles one key while the Activity panel is being monitored
// (focused via Tab, or locked during a run): arrows scroll by a line,
// shift+arrows by fastStep, PgUp/PgDn by a page, and ←→ cycle the operation
// filter. Unrecognised keys are ignored.
func (m *model) activityKey(key string) {
	switch key {
	case "up", "w":
		m.scrollActivity(-1)
	case "down", "s":
		m.scrollActivity(1)
	case "shift+up", "W":
		m.scrollActivity(-fastStep)
	case "shift+down", "S":
		m.scrollActivity(fastStep)
	case "pgup":
		_, ih := m.activityGeometry()
		m.scrollActivity(-step(ih))
	case "pgdown":
		_, ih := m.activityGeometry()
		m.scrollActivity(step(ih))
	case "left", "a":
		m.cycleOpFilter(-1)
	case "right", "d":
		m.cycleOpFilter(1)
	}
}

// cycleOpFilter steps the Activity operation filter through All → each
// operation group present in the current body (in display order) → All again;
// dir -1 cycles the other way. The scroll resets so the narrowed (or restored)
// list is read from the top. A body with no filterable groups clears any stale
// filter and stays put.
func (m *model) cycleOpFilter(dir int) {
	keys := m.profile.activityOpKeys()
	if len(keys) == 0 {
		m.profile.opFilter = ""
		return
	}
	// Position in the cycle: -1 is "All"; otherwise the index of the current key.
	cur := -1
	for i, k := range keys {
		if k == m.profile.opFilter {
			cur = i
			break
		}
	}
	// n+1 positions in the cycle (All + each group), stepped modulo.
	n := len(keys) + 1
	pos := (cur + 1 + dir + n) % n
	if pos == 0 {
		m.profile.opFilter = ""
	} else {
		m.profile.opFilter = keys[pos-1]
	}
	m.profile.statusScroll = 0
}

// scrollActivity moves the Activity viewport by delta lines (negative scrolls
// up), clamped to [0, statusMaxScroll]. Shared by the arrow-key line scroll and
// the PgUp/PgDn page scroll in updateProfile's activity pane.
func (m *model) scrollActivity(delta int) {
	s := m.profile.statusScroll + delta
	if max := m.statusMaxScroll(); s > max {
		s = max
	}
	if s < 0 {
		s = 0
	}
	m.profile.statusScroll = s
}

// statusMaxScroll is the largest valid statusScroll for the current Activity
// body: the rendered line count minus the visible height, floored at zero.
func (m model) statusMaxScroll() int {
	iw, ih := m.activityGeometry()
	lines := strings.Count(renderStatus(m.profile, iw), "\n") + 1
	if max := lines - ih; max > 0 {
		return max
	}
	return 0
}

// --- Server management ---

// openServers opens the servers list modal.
func (m model) openServers() (tea.Model, tea.Cmd) {
	m.refreshServers()
	m.mode = modeServers
	return m, nil
}

// refreshServers updates the servers list from the config.
func (m *model) refreshServers() {
	m.servers.setNames(sortedServerNames(m.cfg.Servers))
}

// updateServers handles the servers list modal: arrow keys to move, enter/e to
// edit, a to add, d to delete, esc to close.
func (m model) updateServers(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "esc", "q":
		m.mode = modeMain
		return m, nil
	case "a":
		m.serverForm = newServerForm("", config.Server{}, m.path)
		m.serverForm.setWidth(m.width)
		m.serverForm.rsyncBin = m.cfg.RsyncPath
		m.mode = modeServerForm
		return m, textinput.Blink
	case "e", "enter":
		if name, ok := m.servers.selected(); ok {
			m.serverForm = newServerForm(name, m.cfg.Servers[name], m.path)
			m.serverForm.setWidth(m.width)
			m.serverForm.rsyncBin = m.cfg.RsyncPath
			m.mode = modeServerForm
			return m, textinput.Blink
		}
		return m, nil
	case "d":
		if name, ok := m.servers.selected(); ok {
			m.serverRefs = serverRefCount(m.cfg, name)
			m.confirmName = name
			m.confirmKind = confirmDeleteServer
			m.confirmFocus = confirmFocusCancel
			m.mode = modeConfirm
			return m, nil
		}
		return m, nil
	case "up", "w":
		m.servers.moveUp()
		return m, nil
	case "down", "s":
		m.servers.moveDown()
		return m, nil
	}
	return m, nil
}

// updateServerForm handles the server form modal: focus movement, esc to cancel,
// enter/space to save. Mirrors updateSettings.
func (m model) updateServerForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if res, ok := msg.(serverCheckResultMsg); ok {
		return m.applyServerCheckResult(res)
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.serverForm, cmd = m.serverForm.update(msg)
		return m, cmd
	}
	if m.serverForm.checking {
		// While the connection probe is in flight only esc works: it cancels the
		// check (bumping checkSeq abandons the in-flight result) and returns to
		// editing.
		if key.String() == "esc" {
			m.serverForm.checking = false
			m.serverForm.checkSeq++
		}
		return m, nil
	}
	switch key.String() {
	case "esc":
		m.mode = modeServers
		return m, nil
	case "tab", "down":
		return m, m.serverForm.focusNext()
	case "shift+tab", "up":
		return m, m.serverForm.focusPrev()
	case "left":
		if m.serverForm.focus == m.serverForm.cancelSlot() {
			return m, m.serverForm.setFocus(m.serverForm.saveSlot())
		}
	case "right":
		if m.serverForm.focus == m.serverForm.saveSlot() {
			return m, m.serverForm.setFocus(m.serverForm.cancelSlot())
		}
	case "enter":
		if m.serverForm.focus == m.serverForm.cancelSlot() {
			m.mode = modeServers
			return m, nil
		}
		// On an input or on Save: submit.
		return m.submitServer()
	case " ":
		switch m.serverForm.focus {
		case m.serverForm.saveSlot():
			return m.submitServer()
		case m.serverForm.cancelSlot():
			m.mode = modeServers
			return m, nil
		}
		// On an input: fall through to type the space.
	}
	var cmd tea.Cmd
	m.serverForm, cmd = m.serverForm.update(msg)
	return m, cmd
}

// submitServer validates the edited server, then probes the connection before
// persisting anything: it kicks off the check off the UI thread and enters the
// checking state. The save itself happens in finalizeServerSave once the check
// comes back clean (see applyServerCheckResult); a failed check keeps the form
// open with the error and writes nothing.
func (m model) submitServer() (tea.Model, tea.Cmd) {
	if err := m.serverForm.validate(); err != nil {
		m.serverForm.err = err.Error()
		return m, nil
	}
	name, srv := m.serverForm.values()

	// Check for duplicate name when renaming
	if name != m.serverForm.origName {
		if _, exists := m.cfg.Servers[name]; exists {
			m.serverForm.err = "server \"" + name + "\" already exists"
			return m, nil
		}
	}

	// Probe with the credentials that would be saved, without persisting them yet.
	probeFile, cleanup, err := m.serverForm.probePasswordFile()
	if err != nil {
		m.serverForm.err = "prepare connection check: " + err.Error()
		return m, nil
	}
	d := threewayrsync.Daemon{Host: srv.Host, Port: srv.Port, User: srv.User, PasswordFile: probeFile}
	m.serverForm.err = ""
	m.serverForm.checking = true
	m.serverForm.checkSeq++
	return m, checkConnectionCmd(m.serverForm.checker(), d, cleanup, m.serverForm.checkSeq)
}

// applyServerCheckResult folds a finished connection probe back into the form.
// A stale result (esc-abandoned or superseded) is dropped by the seq stamp. A
// clean result finalizes the save; a failed one keeps the form open with the
// error.
func (m model) applyServerCheckResult(res serverCheckResultMsg) (tea.Model, tea.Cmd) {
	if !m.serverForm.checking || res.seq != m.serverForm.checkSeq {
		return m, nil
	}
	m.serverForm.checking = false
	if res.err != nil {
		m.serverForm.err = "connection check failed: " + res.err.Error()
		if errors.Is(res.err, fs.ErrNotExist) {
			m.serverForm.err += " (type the password to create it)"
		}
		return m, nil
	}
	return m.finalizeServerSave()
}

// finalizeServerSave writes the edited server (and any typed password) to disk
// and returns to the servers list. It assumes the form already validated and
// the connection check passed. On a write failure it keeps the modal open with
// an error.
func (m model) finalizeServerSave() (tea.Model, tea.Cmd) {
	name, srv := m.serverForm.values()
	origName := m.serverForm.origName

	// Resolve the password file (see passFileTarget). A typed password is written
	// to the target, replacing a stale managed file at the old location. Without
	// one, a changed location carries the existing file over — copied instead of
	// moved while another entry still uses the old path. undo reverses a move if
	// the config save below fails.
	password := m.serverForm.password()
	oldPath := m.serverForm.origPassFile
	finalPath := m.serverForm.passFileTarget()
	shared := oldPath != "" && passFileInUse(m.cfg, origName, oldPath)
	undo := func() {}
	switch {
	case password != "":
		if err := config.WriteServerPassword(config.ExpandRoot(finalPath), password); err != nil {
			m.serverForm.err = "write password file: " + err.Error()
			return m, nil
		}
		if oldPath != "" && oldPath != finalPath && !shared &&
			oldPath == config.ServerPasswordPath(m.path, origName) {
			_ = os.Remove(config.ExpandRoot(oldPath)) // best-effort: stale managed secret
		}
	case m.serverForm.movesPassFile(finalPath):
		from, to := config.ExpandRoot(oldPath), config.ExpandRoot(finalPath)
		if err := config.MovePasswordFile(from, to, shared); err != nil {
			m.serverForm.err = "move password file: " + err.Error()
			return m, nil
		}
		undo = func() {
			if shared {
				_ = os.Remove(to)
			} else {
				_ = config.MovePasswordFile(to, from, false)
			}
		}
	}
	srv.PasswordFile = finalPath

	// Save with rollback
	prev := cloneServers(m.cfg.Servers)
	if origName != "" && origName != name {
		delete(m.cfg.Servers, origName)
	}
	if m.cfg.Servers == nil {
		m.cfg.Servers = make(map[string]config.Server)
	}
	m.cfg.Servers[name] = srv

	if err := commitServers(m.path, m.cfg, prev); err != nil {
		undo()
		m.serverForm.err = "save failed: " + err.Error()
		return m, nil
	}

	m.refreshServers()
	m.mode = modeServers
	return m, nil
}

// deleteConfirmedServer removes m.confirmName from the config and persists it,
// rolling back in memory if the save fails.
func (m model) deleteConfirmedServer() (tea.Model, tea.Cmd) {
	// Capture the password file before removal so a dibs-managed one can be
	// cleaned up after a successful save (a bring-your-own path is left alone).
	deleted := m.cfg.Servers[m.confirmName]
	managed := config.ServerPasswordPath(m.path, m.confirmName)
	prev := cloneServers(m.cfg.Servers)
	delete(m.cfg.Servers, m.confirmName)
	if err := commitServers(m.path, m.cfg, prev); err != nil {
		m.err = err
		m.mode = modeServers
		return m, nil
	}
	if deleted.PasswordFile == managed {
		_ = os.Remove(managed) // best-effort: config already saved
	}
	m.refreshServers()
	m.mode = modeServers
	m.err = nil
	return m, nil
}

// serversView renders the servers list modal.
func (m model) serversView() string {
	width := 60
	if m.width > 0 && m.width-8 < width {
		width = m.width - 8
	}
	if width < 30 {
		width = 30
	}

	height := 20
	if len(m.servers.names) > height {
		height = len(m.servers.names)
	}
	if height < 3 {
		height = 3
	}
	if height > 30 {
		height = 30
	}

	var content strings.Builder
	if len(m.servers.names) == 0 {
		content.WriteString(helpTextStyle.Render("No servers configured"))
	} else {
		content.WriteString(m.servers.view(width-4, height))
	}

	content.WriteString("\n\n")
	sep := helpTextStyle.Render(" · ")
	content.WriteString(hint("a", "Add") + sep + hint("e/↵", "Edit") + sep + hint("d", "Delete") + sep + hint("esc", "Close"))

	if m.err != nil {
		content.WriteString("\n\n")
		content.WriteString(errStyle.Render(m.err.Error()))
	}

	body := lipgloss.NewStyle().Padding(0, 1).Render(content.String())
	return titledBox("Servers", body, width, lipgloss.Height(body)+2, true)
}
