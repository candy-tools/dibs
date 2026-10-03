package threewayrsync

import (
	"errors"
	"strconv"
	"strings"
)

// SSH describes reaching an endpoint over ssh. A zero Port and empty IdentityFile mean
// "use the ssh default".
type SSH struct {
	User         string
	Host         string
	Port         int
	IdentityFile string
}

// Daemon describes reaching an endpoint via the rsync daemon protocol (rsync:// URLs /
// "host::module" syntax). A zero Port means the daemon default (873). Authentication is
// the daemon's optional user/password scheme: PasswordFile is handed to rsync as
// --password-file; when empty, rsync still honors the RSYNC_PASSWORD environment
// variable, and modules without auth need neither.
type Daemon struct {
	Host         string
	Port         int
	User         string
	Module       string
	PasswordFile string
}

// Endpoint is one side of a sync: an absolute path, optionally reached over ssh or the
// rsync daemon protocol. With both SSH and Daemon nil the path is a local filesystem path
// (for example a mounted share). For a Daemon endpoint, Path is the path inside the
// module and may be empty (the module root).
type Endpoint struct {
	Path   string
	SSH    *SSH
	Daemon *Daemon
}

// remote reports whether the endpoint is reached over a network transport (ssh or the
// rsync daemon) rather than the local filesystem.
func (e Endpoint) remote() bool { return e.SSH != nil || e.Daemon != nil }

// rootURL returns the daemon's module-less root URL, "rsync://[user@]host[:port]/" —
// the address whose listing is the daemon's module list.
func (d Daemon) rootURL() string {
	host := d.Host
	if d.User != "" {
		host = d.User + "@" + host
	}
	if d.Port != 0 {
		host += ":" + strconv.Itoa(d.Port)
	}
	return "rsync://" + host + "/"
}

// render returns the rsync-syntax location: a plain path locally, "[user@]host:path" for
// an ssh target, or an "rsync://[user@]host[:port]/module[/path]" URL for a daemon target
// (the URL form carries a non-default port without extra flags).
func (e Endpoint) render() string {
	if e.Daemon != nil {
		d := e.Daemon
		loc := d.rootURL() + d.Module
		if p := strings.Trim(e.Path, "/"); p != "" {
			loc += "/" + p
		}
		return loc
	}
	if e.SSH == nil {
		return e.Path
	}
	host := e.SSH.Host
	if e.SSH.User != "" {
		host = e.SSH.User + "@" + host
	}
	return host + ":" + e.Path
}

// withTrailingSlash ensures the location ends in exactly one "/" so rsync copies the
// contents of the source rather than nesting a copy.
func withTrailingSlash(loc string) string {
	return strings.TrimRight(loc, "/") + "/"
}

// sshRsh builds the value for rsync's --rsh flag when a non-default port or identity file
// is set. It returns "" when ssh defaults suffice.
func sshRsh(s *SSH) string {
	var parts []string
	if s.Port != 0 {
		parts = append(parts, "-p", strconv.Itoa(s.Port))
	}
	if strings.TrimSpace(s.IdentityFile) != "" {
		parts = append(parts, "-i", s.IdentityFile)
	}
	if len(parts) == 0 {
		return ""
	}
	return "ssh " + strings.Join(parts, " ")
}

// validate checks a local/remote endpoint pair before any rsync invocation. rsync cannot
// transfer between two remote hosts, so at most one endpoint may be remote (ssh or
// daemon), and a single endpoint cannot be both.
func validate(local, remote Endpoint) error {
	for side, e := range map[string]Endpoint{"local": local, "remote": remote} {
		if e.SSH != nil && e.Daemon != nil {
			return errors.New(side + " endpoint sets both ssh and daemon")
		}
		// A daemon path is relative to the module and may be empty (module root).
		if e.Daemon == nil && strings.TrimSpace(e.Path) == "" {
			return errors.New(side + " path is required")
		}
	}
	if local.remote() && remote.remote() {
		return errors.New("at most one endpoint may be a remote (ssh or daemon) target")
	}
	for _, e := range []Endpoint{local, remote} {
		if err := validateSSH(e.SSH); err != nil {
			return err
		}
		if err := validateDaemon(e.Daemon); err != nil {
			return err
		}
	}
	return nil
}

// validateDaemon rejects daemon field values that rsync would misparse: a value beginning
// with "-" becomes an option, whitespace or "@"/":"/"/" would split or re-route the
// rendered "rsync://user@host:port/module" URL.
func validateDaemon(d *Daemon) error {
	return validateDaemonFields(d, true)
}

// validateDaemonFields is validateDaemon with the module requirement optional: listing a
// daemon's modules addresses the daemon root, where no module exists yet. The injection
// guards on the other fields apply either way.
func validateDaemonFields(d *Daemon, requireModule bool) error {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(d.Host) == "" {
		return errors.New("daemon host is required")
	}
	if requireModule && strings.TrimSpace(d.Module) == "" {
		return errors.New("daemon module is required")
	}
	// The password file may contain spaces (e.g. macOS "Application Support"): it is
	// handed to rsync as a single exec argument, never through a shell or the URL.
	if strings.HasPrefix(d.PasswordFile, "-") {
		return errors.New("daemon password file must not start with '-'")
	}
	if strings.ContainsAny(d.PasswordFile, "\n\r") {
		return errors.New("daemon password file must not contain line breaks")
	}
	for name, v := range map[string]string{"host": d.Host, "user": d.User, "module": d.Module} {
		if strings.HasPrefix(v, "-") {
			return errors.New("daemon " + name + " must not start with '-'")
		}
		if strings.ContainsAny(v, " \t\n\r") {
			return errors.New("daemon " + name + " must not contain whitespace")
		}
	}
	for name, v := range map[string]string{"host": d.Host, "user": d.User, "module": d.Module} {
		if strings.ContainsAny(v, "@:/") {
			return errors.New("daemon " + name + " must not contain '@', ':' or '/'")
		}
	}
	return nil
}

// validateSSH rejects ssh field values that ssh or rsync would misparse as options: a
// Host or User beginning with "-" becomes an option injection (e.g. a host of
// "-oProxyCommand=…" executes an arbitrary command), and whitespace or "@"/":" would
// split or re-route the rendered "user@host:path".
func validateSSH(s *SSH) error {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(s.Host) == "" {
		return errors.New("ssh host is required")
	}
	for name, v := range map[string]string{"host": s.Host, "user": s.User, "identity file": s.IdentityFile} {
		if strings.HasPrefix(v, "-") {
			return errors.New("ssh " + name + " must not start with '-'")
		}
		if strings.ContainsAny(v, " \t\n\r") {
			return errors.New("ssh " + name + " must not contain whitespace")
		}
	}
	if strings.ContainsAny(s.Host, "@:") || strings.ContainsAny(s.User, "@:") {
		return errors.New("ssh host and user must not contain '@' or ':'")
	}
	return nil
}
