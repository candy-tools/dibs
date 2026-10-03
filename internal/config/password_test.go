package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/candy-tools/dibs/internal/config"
)

func TestServerPasswordPath(t *testing.T) {
	cfgPath := "/home/u/.config/dibs/config.yaml"
	got := config.ServerPasswordPath(cfgPath, "nas")
	want := "/home/u/.config/dibs/nas.pw"
	if got != want {
		t.Fatalf("ServerPasswordPath = %q, want %q", got, want)
	}
	// Filesystem-unsafe characters in the name are sanitized so the path stays
	// a single file beside the config.
	got = config.ServerPasswordPath(cfgPath, "my nas/prod")
	want = "/home/u/.config/dibs/my_nas_prod.pw"
	if got != want {
		t.Fatalf("sanitized ServerPasswordPath = %q, want %q", got, want)
	}
	// A relative config path resolves to an absolute password-file path, so the
	// path shown/written is never relative.
	got = config.ServerPasswordPath("config.yaml", "nas")
	if !filepath.IsAbs(got) {
		t.Fatalf("ServerPasswordPath from relative config = %q, want absolute", got)
	}
	if filepath.Base(got) != "nas.pw" {
		t.Fatalf("ServerPasswordPath base = %q, want nas.pw", filepath.Base(got))
	}
}

func TestWriteServerPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "nas.pw")
	if err := config.WriteServerPassword(path, "s3cret"); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "s3cret\n" {
		t.Fatalf("content = %q, want %q", string(data), "s3cret\n")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
}

func TestMovePasswordFile(t *testing.T) {
	for name, keep := range map[string]bool{"move": false, "copy": true} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			from := filepath.Join(dir, "old.pw")
			to := filepath.Join(dir, "new dir", "new.pw")
			if err := config.WriteServerPassword(from, "s3cret"); err != nil {
				t.Fatal(err)
			}
			if err := config.MovePasswordFile(from, to, keep); err != nil {
				t.Fatalf("MovePasswordFile: %v", err)
			}
			if data, err := os.ReadFile(to); err != nil || string(data) != "s3cret\n" {
				t.Fatalf("destination content = %q err=%v", string(data), err)
			}
			if _, err := os.Stat(from); keep != (err == nil) {
				t.Fatalf("source exists = %v, want %v", err == nil, keep)
			}
		})
	}

	// A missing source is an error, not a silent no-op.
	dir := t.TempDir()
	if err := config.MovePasswordFile(filepath.Join(dir, "nope"), filepath.Join(dir, "to"), false); !os.IsNotExist(err) {
		t.Fatalf("missing source: err = %v, want not-exist", err)
	}
}

// TestPasswordFileReplacesLooseTarget: a file already at the destination with
// looser permissions never lends them to the secret — writing, moving and
// copying all leave a 0600 file.
func TestPasswordFileReplacesLooseTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	write := func(_, to string) error { return config.WriteServerPassword(to, "s3cret") }
	moveFrom := func(keep bool) func(dir, to string) error {
		return func(dir, to string) error {
			from := filepath.Join(dir, "old.pw")
			if err := config.WriteServerPassword(from, "s3cret"); err != nil {
				return err
			}
			return config.MovePasswordFile(from, to, keep)
		}
	}
	for name, run := range map[string]func(dir, to string) error{
		"write": write, "move": moveFrom(false), "copy": moveFrom(true),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			to := filepath.Join(dir, "new.pw")
			if err := os.WriteFile(to, []byte("old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(to, 0o644); err != nil { // regardless of the umask
				t.Fatal(err)
			}
			if err := run(dir, to); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if data, err := os.ReadFile(to); err != nil || string(data) != "s3cret\n" {
				t.Fatalf("content = %q err=%v", string(data), err)
			}
			fi, err := os.Stat(to)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
			}
		})
	}
}
