//go:build integration

package tui

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/candy-tools/dibs/internal/config"
	"github.com/candy-tools/dibs/libs/threewayrsync"
)

// startAuthDaemon launches a loopback rsync daemon exporting one module, "secret",
// that only user "alice" with password "s3cret" may read. It returns the port.
func startAuthDaemon(t *testing.T) int {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	lst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lst.Addr().(*net.TCPAddr).Port
	_ = lst.Close()

	dir := t.TempDir()
	module := filepath.Join(dir, "module")
	if err := os.MkdirAll(filepath.Join(module, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(dir, "rsyncd.secrets")
	if err := os.WriteFile(secrets, []byte("alice:s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "rsyncd.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf(
		"use chroot = false\npid file = %[1]s/rsyncd.pid\nlog file = %[1]s/rsyncd.log\n\n"+
			"[secret]\n  path = %[2]s\n  auth users = alice\n  secrets file = %[3]s\n",
		dir, module, secrets)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("rsync", "--daemon", "--no-detach", "--config="+conf, fmt.Sprintf("--port=%d", port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return port
		}
	}
	t.Fatalf("rsync daemon did not come up on port %d", port)
	return 0
}

// saveServerForm drives Save the way the TUI does: submit (validate + start the
// real connection check), run the check, and feed its result back.
func saveServerForm(t *testing.T, m model) model {
	t.Helper()
	next, cmd := m.submitServer()
	m = next.(model)
	if cmd == nil {
		t.Fatalf("submit did not start a connection check (form error: %q)", m.serverForm.err)
	}
	next, _ = m.applyServerCheckResult(cmd().(serverCheckResultMsg))
	m = next.(model)
	if m.serverForm.err != "" {
		t.Fatalf("save failed: %s", m.serverForm.err)
	}
	return m
}

// browse lists the auth module with a server's saved credentials, as the remote
// folder picker does.
func browse(t *testing.T, srv config.Server) {
	t.Helper()
	d := threewayrsync.Daemon{Host: srv.Host, Port: srv.Port, User: srv.User, Module: "secret",
		PasswordFile: config.ExpandRoot(srv.PasswordFile)}
	dirs, err := (&threewayrsync.Syncer{}).ListDirs(context.Background(), d, "")
	if err != nil {
		t.Fatalf("ListDirs with password file %q: %v", srv.PasswordFile, err)
	}
	if len(dirs) != 1 || dirs[0] != "photos" {
		t.Fatalf("ListDirs = %v, want [photos]", dirs)
	}
}

// TestIntegrationServerPasswordFileWithSpaces: on macOS the config (and so the
// default password file) lives under "Library/Application Support". Creating a
// server there, and later moving its password file to another spaced path, must
// both pass the real connection check and authenticate real rsync listings.
func TestIntegrationServerPasswordFileWithSpaces(t *testing.T) {
	port := startAuthDaemon(t)
	home := t.TempDir()
	cfgPath := filepath.Join(home, "Library", "Application Support", "dibs", "config.yaml")
	cfg := &config.Config{Servers: map[string]config.Server{}, Profiles: map[string]config.Profile{}}
	m := newModel(cfgPath, cfg)

	// Create: type the password, leave Password file blank (default location).
	m.serverForm = newServerForm("", config.Server{}, cfgPath)
	setServerFormFields(&m.serverForm, "Athena", "127.0.0.1", fmt.Sprint(port), "alice", "s3cret", "")
	m = saveServerForm(t, m)

	created := cfg.Servers["Athena"].PasswordFile
	if want := filepath.Join(home, "Library", "Application Support", "dibs", "Athena.pw"); created != want {
		t.Fatalf("PasswordFile = %q, want %q", created, want)
	}
	browse(t, cfg.Servers["Athena"])

	// Move: point Password file at another spaced path, password left blank.
	moved := filepath.Join(home, "My Secrets", "athena.pw")
	m.serverForm = newServerForm("Athena", cfg.Servers["Athena"], cfgPath)
	setServerFormFields(&m.serverForm, "Athena", "127.0.0.1", fmt.Sprint(port), "alice", "", moved)
	m = saveServerForm(t, m)

	if got := cfg.Servers["Athena"].PasswordFile; got != moved {
		t.Fatalf("PasswordFile after move = %q, want %q", got, moved)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("old password file still present after move (err=%v)", err)
	}
	browse(t, cfg.Servers["Athena"])

	// The saved config round-trips the moved path.
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Servers["Athena"].PasswordFile; got != moved {
		t.Fatalf("reloaded PasswordFile = %q, want %q", got, moved)
	}
	_ = m
}
