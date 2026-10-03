package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ServerPasswordPath returns the default location dibs uses for a server's
// auto-created rsync password file: "<name>.pw" beside the config file
// (config path's directory), as an absolute path. A relative configPath (e.g.
// a relative --config) is resolved against the working directory so the path
// shown to the user and the file actually written always agree and are
// absolute. The server name is sanitized into a single safe filename segment —
// any character outside [A-Za-z0-9._-] becomes "_" — so a name with spaces or
// slashes still maps to one file next to the config.
func ServerPasswordPath(configPath, serverName string) string {
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, sanitizeFilename(serverName)+".pw")
}

// sanitizeFilename maps a server name to a safe single-segment filename by
// replacing every character outside [A-Za-z0-9._-] with "_".
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// WriteServerPassword writes secret to path as an rsync daemon password file:
// the parent directory is created mode 0700 and the file is written atomically
// mode 0600 — replacing any file already there, whatever its permissions — with
// the secret on a single line. rsync's --password-file expects exactly the
// password (no username), so the file holds just that, newline-terminated.
func WriteServerPassword(path, secret string) error {
	return writeFileAtomic(path, []byte(secret+"\n"))
}

// MovePasswordFile relocates the password file at from to to, creating to's
// parent mode 0700 and replacing any file already at to. With keep set the
// file is copied and from stays in place (something else still uses it). A
// plain rename is tried first; across filesystems it falls back to a 0600 copy
// followed by removing from.
func MovePasswordFile(from, to string, keep bool) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	if !keep && os.Rename(from, to) == nil {
		return nil
	}
	data, err := os.ReadFile(from) //nolint:gosec // G304: from is the password-file path the user configured for the server; no trust boundary is crossed.
	if err != nil {
		return err
	}
	if err := writeFileAtomic(to, data); err != nil {
		return err
	}
	if keep {
		return nil
	}
	return os.Remove(from)
}
