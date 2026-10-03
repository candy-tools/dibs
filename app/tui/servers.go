package tui

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/candy-tools/dibs/internal/config"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// serverFormModel is the server add/edit form: fixed labeled underline inputs
// for Name, Host, Port, User, Password, and Password file, followed by Save and
// Cancel action buttons. Modeled on settingsModel. Save first probes the server
// connection (see servercheck.go) and only then persists: a typed Password is
// written to the Password file (see finalizeServerSave); the field itself is
// never persisted — only the file path lives in the config.
type serverFormModel struct {
	inputs       []textinput.Model
	focus        int
	err          string
	width        int
	origName     string // the original name when editing (empty when adding)
	origPassFile string // the server's saved password-file path when editing (may be empty)
	configPath   string // config file path, used to derive the default password-file location

	// Connection-check state (see servercheck.go). On Save the form probes the
	// server before persisting; checking is true while that probe is in flight,
	// checkSeq stamps it so a result abandoned by esc (or superseded) is dropped.
	checking  bool
	checkSeq  int
	rsyncBin  string      // rsync binary override for the probe; "" => rsync from PATH
	checkConn connChecker // test seam; nil => a Syncer-backed checker
}

// Field indexes for serverFormModel.inputs.
const (
	srvName = iota
	srvHost
	srvPort
	srvUser
	srvPassword // the secret; typed here, written to srvPassFile's path on save
	srvPassFile // path to the rsync password file
)

func (s serverFormModel) saveSlot() int   { return len(s.inputs) }
func (s serverFormModel) cancelSlot() int { return len(s.inputs) + 1 }
func (s serverFormModel) numSlots() int   { return len(s.inputs) + 2 }

// onInput reports whether focus is on a text input (as opposed to a button).
func (s serverFormModel) onInput() bool { return s.focus < len(s.inputs) }

// newServerForm builds the form for a server: one input per field, prefilled
// with the given server values. The first input is focused. origName is the
// server name when editing (used to allow renaming), or empty when adding.
// configPath is the config file's path, used to show where a typed password
// will be stored (the default password-file location beside the config).
func newServerForm(origName string, srv config.Server, configPath string) serverFormModel {
	inputs := make([]textinput.Model, 6)
	for i := range inputs {
		inputs[i] = textinput.New()
		inputs[i].CharLimit = 128
		inputs[i].Prompt = ""
		inputs[i].Placeholder = ""
	}

	// Prefill values. The Password field is always blank: on edit, blank means
	// "keep the existing file", and the secret is never rendered back.
	inputs[srvName].SetValue(origName)
	inputs[srvHost].SetValue(srv.Host)
	if srv.Port > 0 {
		inputs[srvPort].SetValue(strconv.Itoa(srv.Port))
	}
	inputs[srvUser].SetValue(srv.User)
	inputs[srvPassFile].SetValue(srv.PasswordFile)

	// Mask the secret as it is typed.
	inputs[srvPassword].EchoMode = textinput.EchoPassword
	inputs[srvPassword].EchoCharacter = '•'

	inputs[srvPort].Placeholder = "873 (default)"
	inputs[srvUser].Placeholder = "none"
	inputs[srvPassword].Placeholder = "none"

	s := serverFormModel{inputs: inputs, origName: origName, origPassFile: srv.PasswordFile, configPath: configPath}
	s.refreshPassFilePlaceholder()
	if len(s.inputs) > 0 {
		s.inputs[0].Focus()
	}
	return s
}

// password returns the typed secret, or "" when the field is blank. A
// whitespace-only entry counts as blank (no password); a real password is
// returned verbatim (not trimmed), so intentional characters are preserved.
func (s serverFormModel) password() string {
	v := s.inputs[srvPassword].Value()
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return v
}

// passFileTarget is the password-file path the config will hold after save: the
// Password file field, except that a managed file left untouched across a
// rename follows the new name, and a typed password with a blank field goes to
// the default managed location. "" means the server uses no password file.
func (s serverFormModel) passFileTarget() string {
	name, srv := s.values()
	path := srv.PasswordFile
	if s.origName != "" && s.origName != name {
		oldManaged := config.ServerPasswordPath(s.configPath, s.origName)
		if path == oldManaged && s.origPassFile == oldManaged {
			return config.ServerPasswordPath(s.configPath, name)
		}
	}
	if path == "" && s.password() != "" {
		return config.ServerPasswordPath(s.configPath, name)
	}
	return path
}

// movesPassFile reports whether saving carries the existing password file over
// to target: no new secret was typed, the location changed, and nothing is at
// target yet (an existing file there is the user's pick and is used as-is).
func (s serverFormModel) movesPassFile(target string) bool {
	if s.password() != "" || s.origPassFile == "" || target == "" || target == s.origPassFile {
		return false
	}
	_, err := os.Stat(config.ExpandRoot(target))
	return errors.Is(err, fs.ErrNotExist)
}

// probePasswordFile resolves a password file to authenticate the connection
// check with, without touching the server's real files. A typed password is
// written to a private temp file (removed by the returned cleanup); a blank
// password probes the file the server will use after save — the original file
// when save is going to move it (see movesPassFile). "" means no password file.
// The cleanup is always safe to call.
func (s serverFormModel) probePasswordFile() (string, func(), error) {
	noop := func() {}
	if pw := s.password(); pw != "" {
		f, err := os.CreateTemp("", "dibs-probe-*.pw") // 0600 by default
		if err != nil {
			return "", noop, err
		}
		path := f.Name()
		cleanup := func() { _ = os.Remove(path) }
		if _, err := f.WriteString(pw + "\n"); err != nil {
			_ = f.Close()
			cleanup()
			return "", noop, err
		}
		if err := f.Close(); err != nil {
			cleanup()
			return "", noop, err
		}
		return path, cleanup, nil
	}
	path := s.passFileTarget()
	if s.movesPassFile(path) {
		path = s.origPassFile
	}
	return config.ExpandRoot(path), noop, nil
}

// passFileInUse reports whether any config entry other than server still points
// at the password file path — another server, or a profile's own
// rsyncd_password_file — so it must not be moved or removed from under it.
func passFileInUse(cfg *config.Config, server, path string) bool {
	path = config.ExpandRoot(path)
	for name, srv := range cfg.Servers {
		if name != server && config.ExpandRoot(srv.PasswordFile) == path {
			return true
		}
	}
	for _, p := range cfg.Profiles {
		if config.ExpandRoot(p.RsyncdPasswordFile) == path {
			return true
		}
	}
	return false
}

// refreshPassFilePlaceholder sets the Password file field's greyed placeholder
// to the default location a typed password would be written to for the name
// currently in the form — so the user can see where the secret will land. When
// the name is blank it shows the "<name>.pw" pattern instead of a concrete path.
func (s *serverFormModel) refreshPassFilePlaceholder() {
	name := strings.TrimSpace(s.inputs[srvName].Value())
	if name == "" {
		// Absolute dir so the empty-name pattern matches the concrete (absolute)
		// path shown once a name is typed.
		dir := filepath.Dir(s.configPath)
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		s.inputs[srvPassFile].Placeholder = filepath.Join(dir, "<name>.pw")
		return
	}
	s.inputs[srvPassFile].Placeholder = config.ServerPasswordPath(s.configPath, name)
}

// modalWidth caps the form window's width. The cap matches the profile form so
// a full password-file path (an absolute path shown in the Password file field
// / its default-location placeholder) fits without truncation.
func (s serverFormModel) modalWidth() int {
	w := s.width - 8
	if w > 100 {
		w = 100
	}
	if w < 30 {
		w = 30
	}
	return w
}

// fieldWidth is the display width of an input's underline.
func (s serverFormModel) fieldWidth() int {
	w := s.modalWidth() - 4
	if w < 6 {
		w = 6
	}
	return w
}

// setWidth records the terminal width and sizes each input to fit within its
// underline.
func (s *serverFormModel) setWidth(w int) {
	s.width = w
	iw := s.fieldWidth() - 1
	if iw < 6 {
		iw = 6
	}
	for i := range s.inputs {
		s.inputs[i].Width = iw
	}
}

// setFocus moves focus to slot i (wrapping), focusing the matching input and
// blurring the rest.
func (s *serverFormModel) setFocus(i int) tea.Cmd {
	n := s.numSlots()
	i = (i%n + n) % n
	s.focus = i
	var cmd tea.Cmd
	for j := range s.inputs {
		if j == i {
			cmd = s.inputs[j].Focus()
		} else {
			s.inputs[j].Blur()
		}
	}
	return cmd
}

func (s *serverFormModel) focusNext() tea.Cmd { return s.setFocus(s.focus + 1) }
func (s *serverFormModel) focusPrev() tea.Cmd { return s.setFocus(s.focus - 1) }

// values returns the parsed (name, server) pair from the form's inputs.
// Trims all fields; port is parsed via strconv.Atoi (empty → 0).
func (s serverFormModel) values() (string, config.Server) {
	name := strings.TrimSpace(s.inputs[srvName].Value())
	host := strings.TrimSpace(s.inputs[srvHost].Value())
	portStr := strings.TrimSpace(s.inputs[srvPort].Value())
	user := strings.TrimSpace(s.inputs[srvUser].Value())
	passFile := strings.TrimSpace(s.inputs[srvPassFile].Value())

	port := 0
	if portStr != "" {
		port, _ = strconv.Atoi(portStr)
	}

	return name, config.Server{
		Host:         host,
		Port:         port,
		User:         user,
		PasswordFile: passFile,
	}
}

// validate runs validation on the form values: name via ValidateName, port
// must be numeric if non-empty, and server fields via ValidateServer.
func (s serverFormModel) validate() error {
	name, srv := s.values()

	if err := config.ValidateName(name); err != nil {
		return err
	}

	// Check for non-numeric port
	portStr := strings.TrimSpace(s.inputs[srvPort].Value())
	if portStr != "" {
		if _, err := strconv.Atoi(portStr); err != nil {
			return errors.New("port must be a number")
		}
	}

	return config.ValidateServer(srv)
}

// update forwards a message to the focused input (no-op on the buttons). Typing
// in the Name field refreshes the Password file placeholder so it always shows
// where a typed password would be stored for the current name.
func (s serverFormModel) update(msg tea.Msg) (serverFormModel, tea.Cmd) {
	if !s.onInput() {
		return s, nil
	}
	var cmd tea.Cmd
	i := s.focus
	s.inputs[i], cmd = s.inputs[i].Update(msg)
	if i == srvName {
		s.refreshPassFilePlaceholder()
	}
	return s, cmd
}

// underline renders input i as its value over a single bottom-border line,
// accent-coloured when focused, dim otherwise.
func (s serverFormModel) underline(i int) string {
	c := colDim
	if s.focus == i {
		c = colAccent
	}
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, true, false).
		BorderForeground(c).
		Width(s.fieldWidth()).
		Render(s.inputs[i].View())
}

func (s serverFormModel) View() string {
	labels := []string{"Name", "Host", "Port", "User", "Password", "Password file"}
	var content strings.Builder
	for i, label := range labels {
		if i > 0 {
			content.WriteString("\n")
		}
		labelSt := labelStyle
		if s.focus == i {
			labelSt = focusLabelStyle
		}
		content.WriteString(labelSt.Render(label))
		content.WriteString("\n")
		content.WriteString(s.underline(i))
	}

	// Centered Save / Cancel action row.
	content.WriteString("\n\n")
	actions := lipgloss.JoinHorizontal(lipgloss.Top,
		confirmButton("Save", s.focus == s.saveSlot()), "   ",
		confirmButton("Cancel", s.focus == s.cancelSlot()))
	content.WriteString(lipgloss.NewStyle().Width(s.modalWidth() - 4).Align(lipgloss.Center).Render(actions))

	if s.checking {
		content.WriteString("\n\n")
		content.WriteString(helpTextStyle.Render("Checking connection… (esc to cancel)"))
	}

	if s.err != "" {
		content.WriteString("\n\n")
		content.WriteString(errStyle.Render(s.err))
	}

	content.WriteString("\n\n")
	sep := helpTextStyle.Render(" · ")
	content.WriteString(hint("tab", "Move") + sep + hint("enter/space", "Activate") + sep + hint("esc", "Cancel"))

	title := "Add server"
	if s.origName != "" {
		title = "Edit server"
	}
	body := lipgloss.NewStyle().Padding(0, 1).Render(content.String())
	return titledBox(title, body, s.modalWidth(), lipgloss.Height(body)+2, true)
}

// serverRefCount returns the names of profiles that reference the given server,
// used to warn before deleting a server still in use.
func serverRefCount(cfg *config.Config, server string) []string {
	var refs []string
	for name, p := range cfg.Profiles {
		if p.Server == server {
			refs = append(refs, name)
		}
	}
	return refs
}

// cloneServers makes a shallow copy of the servers map for rollback on save failure.
func cloneServers(s map[string]config.Server) map[string]config.Server {
	out := make(map[string]config.Server, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// commitServers saves cfg to path. On failure it restores prev into
// cfg.Servers so in-memory state never diverges from what is on disk.
func commitServers(path string, cfg *config.Config, prev map[string]config.Server) error {
	if err := config.Save(path, cfg); err != nil {
		cfg.Servers = prev
		return err
	}
	return nil
}

// sortedServerNames returns the server names in sorted order.
func sortedServerNames(servers map[string]config.Server) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
