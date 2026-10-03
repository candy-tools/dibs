package tui

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/candy-tools/dibs/internal/config"
)

// TestSubmitServerWritesPasswordFile: typing a password on a new server writes
// it to the default managed location beside the config and points PasswordFile
// there, without persisting the secret in the config.
func TestSubmitServerWritesPasswordFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{Servers: map[string]config.Server{}, Profiles: map[string]config.Profile{}}
	m := newModel(path, cfg)
	m.serverForm = newServerForm("", config.Server{}, path)
	setServerFormFields(&m.serverForm, "nas", "nas.local", "", "bob", "hunter2", "")

	if _, _ = m.finalizeServerSave(); m.serverForm.err != "" {
		t.Fatalf("submit error: %s", m.serverForm.err)
	}

	pwPath := filepath.Join(dir, "nas.pw")
	data, err := os.ReadFile(pwPath)
	if err != nil {
		t.Fatalf("password file not written: %v", err)
	}
	if string(data) != "hunter2\n" {
		t.Fatalf("password file content = %q", string(data))
	}
	if got := cfg.Servers["nas"].PasswordFile; got != pwPath {
		t.Fatalf("PasswordFile = %q, want %q", got, pwPath)
	}
}

// TestSubmitServerBlankPasswordKeepsFile: editing with a blank password does not
// touch the existing password file.
func TestSubmitServerBlankPasswordKeepsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	pwPath := config.ServerPasswordPath(path, "nas")
	if err := config.WriteServerPassword(pwPath, "original"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Servers:  map[string]config.Server{"nas": {Host: "h", PasswordFile: pwPath}},
		Profiles: map[string]config.Profile{},
	}
	m := newModel(path, cfg)
	m.serverForm = newServerForm("nas", cfg.Servers["nas"], path)
	// Re-set fields without touching the (blank) password field.
	setServerFormFields(&m.serverForm, "nas", "h", "", "", "", pwPath)

	if _, _ = m.finalizeServerSave(); m.serverForm.err != "" {
		t.Fatalf("submit error: %s", m.serverForm.err)
	}
	data, err := os.ReadFile(pwPath)
	if err != nil || string(data) != "original\n" {
		t.Fatalf("password file changed: data=%q err=%v", string(data), err)
	}
}

// TestChangePassFileMovesFile: pointing the Password file field somewhere new
// without retyping the password moves the existing file there; the connection
// check probes the original file, since that is what save will move.
func TestChangePassFileMovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	oldPw := config.ServerPasswordPath(path, "nas")
	if err := config.WriteServerPassword(oldPw, "sec"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Servers:  map[string]config.Server{"nas": {Host: "h", PasswordFile: oldPw}},
		Profiles: map[string]config.Profile{},
	}
	m := newModel(path, cfg)
	m.serverForm = newServerForm("nas", cfg.Servers["nas"], path)
	newPw := filepath.Join(dir, "elsewhere", "nas.pw")
	setServerFormFields(&m.serverForm, "nas", "h", "", "", "", newPw)

	if probe, _, err := m.serverForm.probePasswordFile(); err != nil || probe != oldPw {
		t.Fatalf("probe = %q err=%v, want the original file %q", probe, err, oldPw)
	}
	if _, _ = m.finalizeServerSave(); m.serverForm.err != "" {
		t.Fatalf("submit error: %s", m.serverForm.err)
	}
	if _, err := os.Stat(oldPw); !os.IsNotExist(err) {
		t.Fatal("old password file was not moved")
	}
	if data, err := os.ReadFile(newPw); err != nil || string(data) != "sec\n" {
		t.Fatalf("moved file content = %q err=%v", string(data), err)
	}
	if got := cfg.Servers["nas"].PasswordFile; got != newPw {
		t.Fatalf("PasswordFile = %q, want %q", got, newPw)
	}
}

// TestChangePassFileCopiesSharedFile: when a profile still uses the old file it
// is copied to the new location rather than moved out from under the profile.
func TestChangePassFileCopiesSharedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	oldPw := filepath.Join(dir, "shared.pw")
	if err := config.WriteServerPassword(oldPw, "sec"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Servers:  map[string]config.Server{"nas": {Host: "h", PasswordFile: oldPw}},
		Profiles: map[string]config.Profile{"p": {RsyncdPasswordFile: oldPw}},
	}
	m := newModel(path, cfg)
	m.serverForm = newServerForm("nas", cfg.Servers["nas"], path)
	newPw := filepath.Join(dir, "nas.pw")
	setServerFormFields(&m.serverForm, "nas", "h", "", "", "", newPw)

	if _, _ = m.finalizeServerSave(); m.serverForm.err != "" {
		t.Fatalf("submit error: %s", m.serverForm.err)
	}
	for _, p := range []string{oldPw, newPw} {
		if data, err := os.ReadFile(p); err != nil || string(data) != "sec\n" {
			t.Fatalf("%s content = %q err=%v", p, string(data), err)
		}
	}
}

// TestChangePassFileToExistingFile: pointing at a file that already exists uses
// it as-is and leaves the old file alone.
func TestChangePassFileToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	oldPw := config.ServerPasswordPath(path, "nas")
	existing := filepath.Join(dir, "existing.pw")
	for p, secret := range map[string]string{oldPw: "old", existing: "existing"} {
		if err := config.WriteServerPassword(p, secret); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Servers:  map[string]config.Server{"nas": {Host: "h", PasswordFile: oldPw}},
		Profiles: map[string]config.Profile{},
	}
	m := newModel(path, cfg)
	m.serverForm = newServerForm("nas", cfg.Servers["nas"], path)
	setServerFormFields(&m.serverForm, "nas", "h", "", "", "", existing)

	if probe, _, _ := m.serverForm.probePasswordFile(); probe != existing {
		t.Fatalf("probe = %q, want %q", probe, existing)
	}
	if _, _ = m.finalizeServerSave(); m.serverForm.err != "" {
		t.Fatalf("submit error: %s", m.serverForm.err)
	}
	for p, want := range map[string]string{oldPw: "old\n", existing: "existing\n"} {
		if data, err := os.ReadFile(p); err != nil || string(data) != want {
			t.Fatalf("%s content = %q err=%v, want %q", p, string(data), err, want)
		}
	}
}

// TestProbeNoPasswordFile: a server with neither a password nor a password file
// is probed without one — matching what save persists.
func TestProbeNoPasswordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	s := newServerForm("", config.Server{}, path)
	setServerFormFields(&s, "nas", "h", "", "", "", "")
	if probe, _, err := s.probePasswordFile(); err != nil || probe != "" {
		t.Fatalf("probe = %q err=%v, want no password file", probe, err)
	}
}

// TestMissingPassFileHint: a connection check failing on a missing password file
// keeps the form open and tells the user how to create it.
func TestMissingPassFileHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	m := newModel(path, &config.Config{Servers: map[string]config.Server{}, Profiles: map[string]config.Profile{}})
	m.serverForm = newServerForm("", config.Server{}, path)
	m.serverForm.checking = true
	m.serverForm.checkSeq = 1

	res := serverCheckResultMsg{seq: 1, err: fmt.Errorf("password file: %w", fs.ErrNotExist)}
	got, _ := m.applyServerCheckResult(res)
	if msg := got.(model).serverForm.err; !strings.Contains(msg, "type the password to create it") {
		t.Fatalf("form error = %q, want the create-it hint", msg)
	}
}

// TestDeleteServerRemovesManagedFile: deleting a server removes its managed
// password file but leaves a bring-your-own file untouched.
func TestDeleteServerRemovesManagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	managed := config.ServerPasswordPath(path, "nas")
	if err := config.WriteServerPassword(managed, "sec"); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "custom.pw")
	if err := config.WriteServerPassword(custom, "sec"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Servers: map[string]config.Server{
			"nas": {Host: "h", PasswordFile: managed},
			"byo": {Host: "h", PasswordFile: custom},
		},
		Profiles: map[string]config.Profile{},
	}
	m := newModel(path, cfg)

	m.confirmName = "nas"
	if _, _ = m.deleteConfirmedServer(); m.err != nil {
		t.Fatalf("delete error: %v", m.err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatal("managed password file was not removed")
	}

	m.confirmName = "byo"
	if _, _ = m.deleteConfirmedServer(); m.err != nil {
		t.Fatalf("delete error: %v", m.err)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("bring-your-own password file should survive delete: %v", err)
	}
}

// TestRenameServerMovesManagedFile: renaming a server whose managed file the
// user did not redirect moves the file to the new name's managed location.
func TestRenameServerMovesManagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	oldPw := config.ServerPasswordPath(path, "nas")
	if err := config.WriteServerPassword(oldPw, "sec"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Servers:  map[string]config.Server{"nas": {Host: "h", PasswordFile: oldPw}},
		Profiles: map[string]config.Profile{},
	}
	m := newModel(path, cfg)
	m.serverForm = newServerForm("nas", cfg.Servers["nas"], path)
	// Rename to "backup"; blank password; path field still shows the old managed path.
	setServerFormFields(&m.serverForm, "backup", "h", "", "", "", oldPw)

	if _, _ = m.finalizeServerSave(); m.serverForm.err != "" {
		t.Fatalf("submit error: %s", m.serverForm.err)
	}

	newPw := config.ServerPasswordPath(path, "backup")
	if _, err := os.Stat(oldPw); !os.IsNotExist(err) {
		t.Fatal("old managed file was not moved")
	}
	data, err := os.ReadFile(newPw)
	if err != nil || string(data) != "sec\n" {
		t.Fatalf("moved file content = %q err=%v", string(data), err)
	}
	if got := cfg.Servers["backup"].PasswordFile; got != newPw {
		t.Fatalf("PasswordFile = %q, want %q", got, newPw)
	}
	if _, ok := cfg.Servers["nas"]; ok {
		t.Fatal("old server name still present after rename")
	}
}
