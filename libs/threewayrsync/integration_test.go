//go:build integration

package threewayrsync_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	tw "github.com/candy-tools/dibs/libs/threewayrsync"
)

func requireRsync(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not on PATH")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newSyncer(t *testing.T) (*tw.Syncer, tw.Endpoint, tw.Endpoint) {
	t.Helper()
	root := t.TempDir()
	local := filepath.Join(root, "local")
	remote := filepath.Join(root, "remote")
	for _, d := range []string{local, remote} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := tw.FileStore{Path: filepath.Join(root, "state", "base.json")}
	return tw.New(store), tw.Endpoint{Path: local}, tw.Endpoint{Path: remote}
}

func TestIntegrationFullCycle(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	// Local has files, remote and base empty => push both to remote.
	writeFile(t, filepath.Join(local.Path, "a.txt"), "A")
	writeFile(t, filepath.Join(local.Path, "sub", "b.txt"), "BB")

	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remote.Path, "a.txt")); err != nil {
		t.Fatalf("a.txt should have been pushed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remote.Path, "sub", "b.txt")); err != nil {
		t.Fatalf("sub/b.txt should have been pushed: %v", err)
	}

	// Second diff is in sync.
	plan, err := s.Diff(ctx, local, remote, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.InSync {
		t.Fatalf("expected in sync, got %+v", plan)
	}

	// Edit the remote (larger content => size change) => next sync pulls it local.
	writeFile(t, filepath.Join(remote.Path, "a.txt"), "A-EDITED")
	plan, err = s.Diff(ctx, local, remote, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Pull) != 1 || plan.Pull[0] != "a.txt" {
		t.Fatalf("expected a.txt pull, got %+v", plan)
	}
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local.Path, "a.txt"))
	if err != nil || string(got) != "A-EDITED" {
		t.Fatalf("local a.txt = %q err %v", string(got), err)
	}

	// Delete sub/b.txt locally (remote unchanged) => sync removes it remotely.
	if err := os.Remove(filepath.Join(local.Path, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, local, remote, tw.Options{AllowDeletes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(remote.Path, "sub", "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("remote sub/b.txt should be gone, stat err = %v", err)
	}
}

func TestIntegrationConflictAbort(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	// Establish a shared baseline for x.txt.
	writeFile(t, filepath.Join(local.Path, "x.txt"), "orig")
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	// Edit both sides to different sizes => conflict.
	writeFile(t, filepath.Join(local.Path, "x.txt"), "local-change")
	writeFile(t, filepath.Join(remote.Path, "x.txt"), "R")

	_, err := s.Sync(ctx, local, remote, tw.Options{Conflict: tw.Abort})
	var ce *tw.ConflictError
	if !errors.As(err, &ce) || len(ce.Paths) != 1 || ce.Paths[0] != "x.txt" {
		t.Fatalf("want ConflictError for x.txt, got %v", err)
	}
	// Abort changed nothing: remote still "R".
	got, _ := os.ReadFile(filepath.Join(remote.Path, "x.txt"))
	if string(got) != "R" {
		t.Errorf("remote must be untouched on Abort, got %q", string(got))
	}
}

// The scenario the safety work targets: after a successful sync the "remote" (a mounted
// share) disappears — replaced here by an empty directory, which is what an unmounted
// mount point looks like. The sync must refuse rather than wipe the local tree.
func TestIntegrationUnmountedRemoteDoesNotWipeLocal(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(local.Path, "a.txt"), "A")
	writeFile(t, filepath.Join(local.Path, "b.txt"), "B")
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}

	// "Unmount": the remote path now exists but is empty.
	if err := os.RemoveAll(remote.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remote.Path, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := s.Sync(ctx, local, remote, tw.Options{})
	var ee *tw.EmptyEndpointError
	if !errors.As(err, &ee) {
		t.Fatalf("want EmptyEndpointError, got %v", err)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, statErr := os.Stat(filepath.Join(local.Path, f)); statErr != nil {
			t.Errorf("local %s must survive: %v", f, statErr)
		}
	}

	// And a missing remote path fails even earlier, at the preflight stat.
	if err := os.RemoveAll(remote.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err == nil {
		t.Fatal("missing remote path must fail preflight")
	}
}

// startDaemon launches a loopback rsync daemon exporting moduleDir as module "data" on a
// free port, and kills it on test cleanup. The module is writable and chroot-less so the
// test can run unprivileged.
func startDaemon(t *testing.T, moduleDir string) int {
	t.Helper()
	lst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lst.Addr().(*net.TCPAddr).Port
	_ = lst.Close()

	dir := t.TempDir()
	conf := filepath.Join(dir, "rsyncd.conf")
	writeFile(t, conf, fmt.Sprintf(
		"use chroot = false\npid file = %s/rsyncd.pid\nlog file = %s/rsyncd.log\n\n[data]\n  path = %s\n  read only = false\n",
		dir, dir, moduleDir))

	cmd := exec.Command("rsync", "--daemon", "--no-detach", "--config="+conf, "--port="+fmt.Sprint(port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Wait for the daemon to accept connections.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return port
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("rsync daemon did not come up on port %d", port)
	return 0
}

// TestIntegrationDaemonFullCycle exercises the full three-way lifecycle against a real
// rsync daemon: initial checkout-style push, remote edit pulled back, and a local delete
// propagated to the daemon via --delete-missing-args (a daemon has no shell for rm).
func TestIntegrationDaemonFullCycle(t *testing.T) {
	requireRsync(t)
	root := t.TempDir()
	local := filepath.Join(root, "local")
	moduleDir := filepath.Join(root, "module")
	for _, d := range []string{local, moduleDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	port := startDaemon(t, moduleDir)
	s := tw.New(tw.FileStore{Path: filepath.Join(root, "state", "base.json")})
	lep := tw.Endpoint{Path: local}
	rep := tw.Endpoint{Daemon: &tw.Daemon{Host: "127.0.0.1", Port: port, Module: "data"}}
	ctx := context.Background()

	// Push two files to the daemon.
	writeFile(t, filepath.Join(local, "a.txt"), "A")
	writeFile(t, filepath.Join(local, "sub", "b.txt"), "BB")
	if _, err := s.Sync(ctx, lep, rep, tw.Options{}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	for _, f := range []string{"a.txt", "sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(moduleDir, f)); err != nil {
			t.Fatalf("%s should be in the module: %v", f, err)
		}
	}

	// Edit inside the module (server side) => pulled local.
	writeFile(t, filepath.Join(moduleDir, "a.txt"), "A-REMOTE")
	if _, err := s.Sync(ctx, lep, rep, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(local, "a.txt")); err != nil || string(got) != "A-REMOTE" {
		t.Fatalf("local a.txt = %q err %v", string(got), err)
	}

	// Delete locally => removed from the module through the daemon protocol.
	if err := os.Remove(filepath.Join(local, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Sync(ctx, lep, rep, tw.Options{AllowDeletes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied.RemoteDeletes) != 1 || res.Applied.RemoteDeletes[0] != "sub/b.txt" {
		t.Fatalf("RemoteDeletes = %v", res.Applied.RemoteDeletes)
	}
	if _, err := os.Stat(filepath.Join(moduleDir, "sub", "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("module sub/b.txt should be gone, stat err = %v", err)
	}

	// Idempotent: a rerun applies nothing.
	res, err = s.Sync(ctx, lep, rep, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Applied.Push) + len(res.Applied.Pull) + len(res.Applied.LocalDeletes) + len(res.Applied.RemoteDeletes); n != 0 {
		t.Errorf("rerun should be a no-op, applied %d ops: %+v", n, res.Applied)
	}
}

// TestIntegrationDaemonMakeDir creates directories on a live daemon: a folder at
// the module root, then a nested one under it, then a no-op re-create.
func TestIntegrationDaemonMakeDir(t *testing.T) {
	requireRsync(t)
	root := t.TempDir()
	moduleDir := filepath.Join(root, "module")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	port := startDaemon(t, moduleDir)
	s := tw.New(tw.FileStore{Path: filepath.Join(root, "state", "base.json")})
	d := tw.Daemon{Host: "127.0.0.1", Port: port, Module: "data"}
	ctx := context.Background()

	if err := s.MakeDir(ctx, d, "brand"); err != nil {
		t.Fatalf("MakeDir(brand): %v", err)
	}
	if info, err := os.Stat(filepath.Join(moduleDir, "brand")); err != nil || !info.IsDir() {
		t.Fatalf("brand should exist as a dir: err=%v", err)
	}

	// A nested folder whose parent now exists.
	if err := s.MakeDir(ctx, d, "brand/new"); err != nil {
		t.Fatalf("MakeDir(brand/new): %v", err)
	}
	if info, err := os.Stat(filepath.Join(moduleDir, "brand", "new")); err != nil || !info.IsDir() {
		t.Fatalf("brand/new should exist as a dir: err=%v", err)
	}

	// Idempotent: re-creating an existing directory is a no-op, not an error.
	if err := s.MakeDir(ctx, d, "brand/new"); err != nil {
		t.Fatalf("re-creating brand/new should be a no-op: %v", err)
	}
}

// TestIntegrationScopedSyncPreservesBase checks the scope contract end to end: a scoped
// sync moves only in-scope files, and alternating scoped and full syncs neither loses
// out-of-scope base entries nor invents phantom changes.
func TestIntegrationScopedSyncPreservesBase(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(local.Path, "keep", "a.txt"), "A")
	writeFile(t, filepath.Join(local.Path, "keep", "deep", "b.txt"), "B")
	writeFile(t, filepath.Join(local.Path, "skip", "c.txt"), "C")
	writeFile(t, filepath.Join(local.Path, "top.txt"), "T")

	// Full sync establishes the base for everything.
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}

	// Scoped sync after edits on both in- and out-of-scope files.
	writeFile(t, filepath.Join(local.Path, "keep", "a.txt"), "A-EDIT")
	writeFile(t, filepath.Join(local.Path, "skip", "c.txt"), "C-EDIT")
	res, err := s.Sync(ctx, local, remote, tw.Options{Scope: []string{"keep"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied.Push) != 1 || res.Applied.Push[0] != "keep/a.txt" {
		t.Fatalf("scoped push = %v, want only keep/a.txt", res.Applied.Push)
	}
	if got, _ := os.ReadFile(filepath.Join(remote.Path, "skip", "c.txt")); string(got) != "C" {
		t.Fatalf("out-of-scope remote file must be untouched, got %q", string(got))
	}

	// A full diff afterwards sees exactly the out-of-scope edit — nothing phantom.
	plan, err := s.Diff(ctx, local, remote, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Push) != 1 || plan.Push[0] != "skip/c.txt" {
		t.Fatalf("full diff push = %+v, want only skip/c.txt", plan)
	}
	if len(plan.Pull)+len(plan.LocalDeletes)+len(plan.RemoteDeletes)+len(plan.Conflicts) != 0 {
		t.Fatalf("full diff must contain no phantom ops: %+v", plan)
	}
}

// TestIntegrationSSHStyleRemoteDelete exercises the unified delete path shape against a
// local endpoint pair (the rsync mechanics of --delete-missing-args are shared; transport
// differs only in URL/rsh) — a large delete set must work in one pass.
func TestIntegrationManyDeletes(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	// The anchor survives the mass delete so the local endpoint never lists empty (which
	// would — correctly — trip the EmptyEndpointError valve).
	writeFile(t, filepath.Join(local.Path, "anchor.txt"), "keep")
	for i := 0; i < 50; i++ {
		writeFile(t, filepath.Join(local.Path, "d", fmt.Sprintf("f%02d.txt", i)), "x")
	}
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(local.Path, "d")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Sync(ctx, local, remote, tw.Options{AllowDeletes: true})
	if err != nil {
		t.Fatal(err)
	}
	// 50 files plus the emptied d/ dir itself.
	if len(res.Applied.RemoteDeletes) != 51 {
		t.Fatalf("RemoteDeletes = %d, want 51", len(res.Applied.RemoteDeletes))
	}
	if _, err := os.Stat(filepath.Join(remote.Path, "d")); !os.IsNotExist(err) {
		t.Fatalf("remote d/ should be gone, stat err = %v", err)
	}
}

// TestIntegrationDirectoryLifecycle covers directory tracking end to end on a
// filesystem pair: an empty dir propagates, a deleted dir (with its files) is removed
// on the other side including the dir itself, and a dir that gained an untracked file
// after planning survives with its content.
func TestIntegrationDirectoryLifecycle(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	// A brand-new empty dir (plus an anchor file) propagates to the remote.
	writeFile(t, filepath.Join(local.Path, "anchor.txt"), "keep")
	if err := os.MkdirAll(filepath.Join(local.Path, "artwork"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if st, err := os.Stat(filepath.Join(remote.Path, "artwork")); err != nil || !st.IsDir() {
		t.Fatalf("empty dir should have been pushed: %v", err)
	}

	// Fill it, sync, then delete the whole folder locally: the files AND the folder
	// itself must disappear remotely (the user-reported bug).
	writeFile(t, filepath.Join(local.Path, "artwork", "cover.png"), "PNG")
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(local.Path, "artwork")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Sync(ctx, local, remote, tw.Options{AllowDeletes: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(remote.Path, "artwork")); !os.IsNotExist(err) {
		t.Fatalf("remote artwork/ should be gone, stat err = %v", err)
	}
	if len(res.Applied.RemoteDeletes) != 2 {
		t.Errorf("RemoteDeletes = %v, want the file and the dir", res.Applied.RemoteDeletes)
	}

	// In sync afterwards.
	plan, err := s.Diff(ctx, local, remote, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.InSync {
		t.Fatalf("expected in sync, got %+v", plan)
	}
}

// A dir that gained an untracked file on the target side is never deleted — neither
// the plan-level guard nor rsync may remove content the sync does not know about.
func TestIntegrationDirDeleteSkipsNonEmpty(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(local.Path, "anchor.txt"), "keep")
	writeFile(t, filepath.Join(local.Path, "d", "tracked.txt"), "x")
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	// Locally drop the folder; remotely someone added an untracked file into it.
	if err := os.RemoveAll(filepath.Join(local.Path, "d")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(remote.Path, "d", "surprise.txt"), "untracked")
	res, err := s.Sync(ctx, local, remote, tw.Options{AllowDeletes: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(remote.Path, "d", "surprise.txt")); err != nil || string(got) != "untracked" {
		t.Fatalf("untracked file must survive: %q err %v", string(got), err)
	}
	// The tracked file is deleted; the dir survives and is reported, resurfacing later.
	if _, err := os.Stat(filepath.Join(remote.Path, "d", "tracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("tracked file should be gone, stat err = %v", err)
	}
	if !slices.Contains(res.Conflicts, "d") {
		t.Errorf("the kept dir must be reported unresolved: %+v", res)
	}
}

// A base recorded before directory tracking (no dir entries) must upgrade cleanly: the
// first sync sees every existing dir as a both-side add that has converged — a no-op
// that enters the base — with no phantom transfers or conflicts.
func TestIntegrationPreDirBaseUpgrades(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(local.Path, "sub", "b.txt"), "BB")
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	// Rewrite the stored base without dir entries (the pre-upgrade format).
	base, ok, err := s.Store.LoadBase()
	if err != nil || !ok {
		t.Fatalf("load base: ok=%v err=%v", ok, err)
	}
	stripped := tw.Manifest{}
	for p, st := range base {
		if !st.IsDir {
			stripped[p] = st
		}
	}
	if err := s.Store.SaveBase(stripped); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Diff(ctx, local, remote, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.InSync {
		t.Fatalf("pre-dir base must diff clean, got %+v", plan)
	}
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	base, _, err = s.Store.LoadBase()
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := base["sub"]; !ok || !st.IsDir {
		t.Errorf("dir must enter the base on the first post-upgrade sync: %+v", base)
	}
}

// TestIntegrationDaemonDirDeletes exercises nested directory deletion through the
// daemon protocol: a two-level tree removal needs one --delete-missing-args call per
// depth level, and a non-empty dir is refused by rsync and reported, not forced.
func TestIntegrationDaemonDirDeletes(t *testing.T) {
	requireRsync(t)
	root := t.TempDir()
	local := filepath.Join(root, "local")
	moduleDir := filepath.Join(root, "module")
	for _, d := range []string{local, moduleDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	port := startDaemon(t, moduleDir)
	s := tw.New(tw.FileStore{Path: filepath.Join(root, "state", "base.json")})
	lep := tw.Endpoint{Path: local}
	rep := tw.Endpoint{Daemon: &tw.Daemon{Host: "127.0.0.1", Port: port, Module: "data"}}
	ctx := context.Background()

	writeFile(t, filepath.Join(local, "anchor.txt"), "keep")
	writeFile(t, filepath.Join(local, "p", "q", "deep.txt"), "x")
	if _, err := s.Sync(ctx, lep, rep, tw.Options{}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(local, "p")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Sync(ctx, lep, rep, tw.Options{AllowDeletes: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moduleDir, "p")); !os.IsNotExist(err) {
		t.Fatalf("module p/ (nested tree) should be gone, stat err = %v", err)
	}
	if len(res.Applied.RemoteDeletes) != 3 {
		t.Errorf("RemoteDeletes = %v, want deep.txt + q + p", res.Applied.RemoteDeletes)
	}

	// Non-empty refusal through the daemon: an untracked server-side file keeps the dir.
	writeFile(t, filepath.Join(local, "full", "tracked.txt"), "y")
	if _, err := s.Sync(ctx, lep, rep, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(local, "full")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(moduleDir, "full", "surprise.txt"), "untracked")
	res, err = s.Sync(ctx, lep, rep, tw.Options{AllowDeletes: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moduleDir, "full", "surprise.txt")); err != nil {
		t.Fatalf("untracked file must survive the daemon delete: %v", err)
	}
	if !slices.Contains(res.Conflicts, "full") {
		t.Errorf("the kept dir must be reported unresolved: %+v", res)
	}
}

func TestIntegrationResumeIsIdempotent(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(local.Path, "a.txt"), "A")
	if _, err := s.Sync(ctx, local, remote, tw.Options{}); err != nil {
		t.Fatal(err)
	}
	// Running again re-derives from live state: everything has converged => no-op.
	res, err := s.Sync(ctx, local, remote, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied.Push)+len(res.Applied.Pull)+len(res.Applied.RemoteDeletes)+len(res.Applied.LocalDeletes) != 0 {
		t.Errorf("second sync should apply nothing, got %+v", res.Applied)
	}
}

// TestIntegrationCancelMidSyncThenResume covers the cancel/resume contract end to end:
// a context canceled mid-apply — from inside the first progress event, while rsync is
// still streaming itemize output — surfaces as ctx.Err(), commits no base, and destroys
// nothing on the source side; a re-run with a live context converges from whatever
// partially landed and commits the base exactly once.
func TestIntegrationCancelMidSyncThenResume(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)

	files := []string{"a.txt", "b.txt", "sub/c.txt", "sub/d.txt", "e.txt"}
	for i, f := range files {
		writeFile(t, filepath.Join(local.Path, f), fmt.Sprintf("content-%d", i))
	}

	// Cancel from the first event: the push rsync is killed mid-transfer, or — if it
	// already finished — the post-apply re-listing fails on the dead context. Both roads
	// must end in ctx.Err() with the base uncommitted.
	ctx, cancel := context.WithCancel(context.Background())
	res, err := s.Sync(ctx, local, remote, tw.Options{
		OnEvent: func(tw.Event) { cancel() },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sync err = %v, want context.Canceled", err)
	}
	if res.BaseSaved {
		t.Error("canceled sync must not report a saved base")
	}
	if _, ok, err := s.Store.LoadBase(); err != nil || ok {
		t.Fatalf("canceled sync must not commit a base: ok=%v err=%v", ok, err)
	}
	// The source side survives untouched: a cancel may leave the destination partial,
	// never damage the origin.
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(local.Path, f)); err != nil {
			t.Errorf("local %s must survive the cancel: %v", f, err)
		}
	}

	// Resume: a fresh run re-derives the plan from live state (files that landed before
	// the kill have converged and drop out) and completes.
	res, err = s.Sync(context.Background(), local, remote, tw.Options{})
	if err != nil {
		t.Fatalf("resume sync: %v", err)
	}
	if !res.BaseSaved {
		t.Error("resume must commit the base")
	}
	for i, f := range files {
		got, err := os.ReadFile(filepath.Join(remote.Path, f))
		if err != nil || string(got) != fmt.Sprintf("content-%d", i) {
			t.Errorf("remote %s after resume = %q err %v", f, string(got), err)
		}
	}
	plan, err := s.Diff(context.Background(), local, remote, tw.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.InSync {
		t.Fatalf("resumed sync must converge, got %+v", plan)
	}
}

// TestIntegrationEventsMatchApplied verifies the live progress stream against a real
// rsync run: every applied operation — push, pull, and both delete directions — is
// announced by exactly one correctly-typed event, and nothing else is announced.
// Transfer events are parsed live from rsync's itemize stream (not echoed from the
// plan), so this pins the --out-format contract on a real binary, including the
// dir-created event for a pushed directory.
func TestIntegrationEventsMatchApplied(t *testing.T) {
	requireRsync(t)
	s, local, remote := newSyncer(t)
	ctx := context.Background()

	var events []tw.Event
	opts := func(allowDeletes bool) tw.Options {
		events = nil
		return tw.Options{
			AllowDeletes: allowDeletes,
			OnEvent:      func(e tw.Event) { events = append(events, e) },
		}
	}
	eventPaths := func(op string) []string {
		var out []string
		for _, e := range events {
			if e.Op == op {
				out = append(out, e.Path)
			}
		}
		slices.Sort(out)
		return out
	}
	assertOp := func(op string, applied []string) {
		t.Helper()
		want := append([]string(nil), applied...)
		slices.Sort(want)
		if got := eventPaths(op); !slices.Equal(got, want) {
			t.Errorf("%s events = %v, want %v", op, got, want)
		}
	}

	// Initial push: two files plus the sub/ dir rsync creates for one of them.
	writeFile(t, filepath.Join(local.Path, "a.txt"), "A")
	writeFile(t, filepath.Join(local.Path, "keep.txt"), "K")
	writeFile(t, filepath.Join(local.Path, "sub", "b.txt"), "BB")
	res, err := s.Sync(ctx, local, remote, opts(false))
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	assertOp("push", res.Applied.Push)
	if !slices.Contains(eventPaths("push"), "sub") {
		t.Errorf("pushing into a new dir must announce the dir: %v", eventPaths("push"))
	}
	for _, op := range []string{"pull", "delete-local", "delete-remote"} {
		assertOp(op, nil)
	}

	// One sync exercising all four ops: a remote edit and a remote add (pull), a local
	// add (push), a local file removal (delete-remote), a remote removal (delete-local).
	writeFile(t, filepath.Join(remote.Path, "a.txt"), "A-EDITED")
	writeFile(t, filepath.Join(remote.Path, "n.txt"), "N")
	writeFile(t, filepath.Join(local.Path, "m.txt"), "M")
	if err := os.Remove(filepath.Join(local.Path, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(remote.Path, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	res, err = s.Sync(ctx, local, remote, opts(true))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertOp("pull", res.Applied.Pull)
	assertOp("push", res.Applied.Push)
	assertOp("delete-remote", res.Applied.RemoteDeletes)
	assertOp("delete-local", res.Applied.LocalDeletes)
	applied := len(res.Applied.Pull) + len(res.Applied.Push) +
		len(res.Applied.LocalDeletes) + len(res.Applied.RemoteDeletes)
	if len(events) != applied {
		t.Errorf("event count = %d, want %d (one per applied op): %+v", len(events), applied, events)
	}
}

func TestIntegrationFileHelpers(t *testing.T) {
	requireRsync(t)
	s, _, remote := newSyncer(t)
	ctx := context.Background()

	// Fetch of a missing file reports not-found without error.
	dst := filepath.Join(t.TempDir(), "fetched.json")
	found, err := s.FetchFile(ctx, remote, ".dibs.json", dst)
	if err != nil {
		t.Fatalf("fetch missing: %v", err)
	}
	if found {
		t.Fatal("missing file must report found=false")
	}

	// Put, fetch back, delete, fetch again.
	src := filepath.Join(t.TempDir(), "marker.json")
	writeFile(t, src, `{"who":"me"}`)
	if err := s.PutFile(ctx, remote, ".dibs.json", src); err != nil {
		t.Fatalf("put: %v", err)
	}
	found, err = s.FetchFile(ctx, remote, ".dibs.json", dst)
	if err != nil || !found {
		t.Fatalf("fetch after put: found=%v err=%v", found, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != `{"who":"me"}` {
		t.Fatalf("fetched content = %q err=%v", string(got), err)
	}
	if err := s.DeleteFile(ctx, remote, ".dibs.json"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	found, err = s.FetchFile(ctx, remote, ".dibs.json", dst)
	if err != nil || found {
		t.Fatalf("fetch after delete: found=%v err=%v", found, err)
	}
}

// startMultiModuleDaemon launches a loopback rsync daemon exporting two modules — "data"
// (with a comment) and "backup" — plus an auth-required module "secret" whose only valid
// credential is user "alice" with password "s3cret". It returns the port and the two
// module directories.
func startMultiModuleDaemon(t *testing.T) (port int, dataDir, backupDir string) {
	t.Helper()
	lst, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = lst.Addr().(*net.TCPAddr).Port
	_ = lst.Close()

	root := t.TempDir()
	dataDir = filepath.Join(root, "data")
	backupDir = filepath.Join(root, "backup")
	secretDir := filepath.Join(root, "secret")
	for _, d := range []string{dataDir, backupDir, secretDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	secretsFile := filepath.Join(dir, "rsyncd.secrets")
	writeFile(t, secretsFile, "alice:s3cret\n")
	if err := os.Chmod(secretsFile, 0o600); err != nil { // rsyncd refuses world-readable secrets
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "rsyncd.conf")
	writeFile(t, conf, fmt.Sprintf(
		"use chroot = false\npid file = %s/rsyncd.pid\nlog file = %s/rsyncd.log\n\n"+
			"[data]\n  path = %s\n  comment = first module\n  read only = false\n\n"+
			"[backup]\n  path = %s\n\n"+
			"[secret]\n  path = %s\n  auth users = alice\n  secrets file = %s\n",
		dir, dir, dataDir, backupDir, secretDir, secretsFile))

	cmd := exec.Command("rsync", "--daemon", "--no-detach", "--config="+conf, "--port="+fmt.Sprint(port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return port, dataDir, backupDir
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("rsync daemon did not come up on port %d", port)
	return 0, "", ""
}

// TestIntegrationListModules pins the real module-list output format the unit fixtures
// assume: one "name\tcomment" line per module, name padded, MOTD-free by default.
func TestIntegrationListModules(t *testing.T) {
	requireRsync(t)
	port, _, _ := startMultiModuleDaemon(t)
	s := tw.New(tw.FileStore{Path: filepath.Join(t.TempDir(), "base.json")})

	mods, err := s.ListModules(context.Background(), tw.Daemon{Host: "127.0.0.1", Port: port})
	if err != nil {
		t.Fatalf("ListModules: %v", err)
	}
	got := map[string]string{}
	for _, m := range mods {
		got[m.Name] = m.Comment
	}
	if got["data"] != "first module" {
		t.Errorf(`module "data" comment = %q, want "first module" (all: %+v)`, got["data"], mods)
	}
	if _, ok := got["backup"]; !ok {
		t.Errorf(`module "backup" missing (all: %+v)`, mods)
	}
	if _, ok := got["secret"]; !ok {
		t.Errorf(`module "secret" missing (all: %+v)`, mods)
	}
}

// TestIntegrationListDirs pins the real --list-only output format: directories only,
// "." skipped, names with spaces intact, nested paths listable, missing paths erroring.
func TestIntegrationListDirs(t *testing.T) {
	requireRsync(t)
	port, dataDir, _ := startMultiModuleDaemon(t)
	for _, d := range []string{"alpha", "beta/nested", "dir with space"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dataDir, "file.txt"), "F")
	s := tw.New(tw.FileStore{Path: filepath.Join(t.TempDir(), "base.json")})
	d := tw.Daemon{Host: "127.0.0.1", Port: port, Module: "data"}
	ctx := context.Background()

	root, err := s.ListDirs(ctx, d, "")
	if err != nil {
		t.Fatalf("ListDirs(root): %v", err)
	}
	want := []string{"alpha", "beta", "dir with space"}
	if !slices.Equal(root, want) {
		t.Errorf("ListDirs(root) = %v, want %v", root, want)
	}

	sub, err := s.ListDirs(ctx, d, "beta")
	if err != nil {
		t.Fatalf("ListDirs(beta): %v", err)
	}
	if !slices.Equal(sub, []string{"nested"}) {
		t.Errorf("ListDirs(beta) = %v, want [nested]", sub)
	}

	if _, err := s.ListDirs(ctx, d, "nope"); err == nil {
		t.Error("ListDirs on a missing path must error")
	}
}

// TestIntegrationListDirsAuthModule pins the auth failure shape the browser surfaces:
// listing inside an auth-required module without credentials fails with an rsync error
// (@ERROR: auth failed), while the right password file succeeds.
func TestIntegrationListDirsAuthModule(t *testing.T) {
	requireRsync(t)
	port, _, _ := startMultiModuleDaemon(t)
	s := tw.New(tw.FileStore{Path: filepath.Join(t.TempDir(), "base.json")})
	ctx := context.Background()

	// No credentials: the daemon refuses.
	_, err := s.ListDirs(ctx, tw.Daemon{Host: "127.0.0.1", Port: port, Module: "secret"}, "")
	if err == nil {
		t.Fatal("ListDirs on an auth module without credentials must error")
	}

	// The right user + password file succeeds, even from a path with a space (the
	// macOS default lives under "Library/Application Support").
	pwDir := filepath.Join(t.TempDir(), "Application Support")
	if err := os.MkdirAll(pwDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(pwDir, "pw")
	writeFile(t, pw, "s3cret\n")
	if err := os.Chmod(pw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDirs(ctx, tw.Daemon{Host: "127.0.0.1", Port: port, Module: "secret", User: "alice", PasswordFile: pw}, ""); err != nil {
		t.Fatalf("ListDirs with valid credentials: %v", err)
	}
}
