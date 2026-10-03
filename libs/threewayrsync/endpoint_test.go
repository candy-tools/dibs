package threewayrsync

import "testing"

func TestEndpointRender(t *testing.T) {
	cases := []struct {
		name string
		ep   Endpoint
		want string
	}{
		{"local", Endpoint{Path: "/data"}, "/data"},
		{"ssh host only", Endpoint{Path: "/data", SSH: &SSH{Host: "nas"}}, "nas:/data"},
		{"ssh user host", Endpoint{Path: "/data", SSH: &SSH{User: "a", Host: "nas"}}, "a@nas:/data"},
		{"daemon module root", Endpoint{Daemon: &Daemon{Host: "nas", Module: "data"}}, "rsync://nas/data"},
		{"daemon subpath", Endpoint{Path: "proj/x", Daemon: &Daemon{Host: "nas", Module: "data"}}, "rsync://nas/data/proj/x"},
		{"daemon slashed path", Endpoint{Path: "/proj/", Daemon: &Daemon{Host: "nas", Module: "data"}}, "rsync://nas/data/proj"},
		{"daemon user port", Endpoint{Path: "p", Daemon: &Daemon{Host: "nas", Port: 8730, User: "u", Module: "m"}}, "rsync://u@nas:8730/m/p"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ep.render(); got != c.want {
				t.Errorf("render = %q, want %q", got, c.want)
			}
		})
	}
}

func TestWithTrailingSlashIdempotent(t *testing.T) {
	if got := withTrailingSlash("/a/b"); got != "/a/b/" {
		t.Errorf("got %q", got)
	}
	if got := withTrailingSlash("/a/b/"); got != "/a/b/" {
		t.Errorf("got %q", got)
	}
}

func TestSSHRsh(t *testing.T) {
	if got := sshRsh(&SSH{Host: "h"}); got != "" {
		t.Errorf("defaults should yield no rsh, got %q", got)
	}
	if got := sshRsh(&SSH{Host: "h", Port: 2222, IdentityFile: "/k"}); got != "ssh -p 2222 -i /k" {
		t.Errorf("rsh = %q", got)
	}
}

func TestValidate(t *testing.T) {
	local := Endpoint{Path: "/l"}
	remote := Endpoint{Path: "/r"}
	cases := map[string]struct {
		local, remote Endpoint
		wantErr       bool
	}{
		"both local":    {local, remote, false},
		"remote ssh ok": {local, Endpoint{Path: "/r", SSH: &SSH{Host: "h"}}, false},
		"local ssh ok":  {Endpoint{Path: "/l", SSH: &SSH{Host: "h"}}, remote, false},
		"both ssh":      {Endpoint{Path: "/l", SSH: &SSH{Host: "h"}}, Endpoint{Path: "/r", SSH: &SSH{Host: "h"}}, true},
		"empty local":   {Endpoint{Path: ""}, remote, true},
		"empty remote":  {local, Endpoint{Path: ""}, true},
		"ssh no host":   {local, Endpoint{Path: "/r", SSH: &SSH{Host: ""}}, true},
		// Option-injection hardening: a host/user/identity leading with "-" would reach
		// ssh/rsync as an option (e.g. -oProxyCommand=... => command execution).
		"dash host":       {local, Endpoint{Path: "/r", SSH: &SSH{Host: "-oProxyCommand=evil"}}, true},
		"dash user":       {local, Endpoint{Path: "/r", SSH: &SSH{Host: "h", User: "-x"}}, true},
		"dash identity":   {local, Endpoint{Path: "/r", SSH: &SSH{Host: "h", IdentityFile: "-x"}}, true},
		"space in host":   {local, Endpoint{Path: "/r", SSH: &SSH{Host: "h x"}}, true},
		"at in host":      {local, Endpoint{Path: "/r", SSH: &SSH{Host: "a@b"}}, true},
		"colon in user":   {local, Endpoint{Path: "/r", SSH: &SSH{Host: "h", User: "a:b"}}, true},
		"normal identity": {local, Endpoint{Path: "/r", SSH: &SSH{Host: "h", IdentityFile: "/home/u/.ssh/id"}}, false},
		// Daemon endpoints: same remote-count and option-injection rules.
		"remote daemon ok":        {local, Endpoint{Daemon: &Daemon{Host: "h", Module: "m"}}, false},
		"daemon with path ok":     {local, Endpoint{Path: "sub/dir", Daemon: &Daemon{Host: "h", Module: "m"}}, false},
		"daemon password file ok": {local, Endpoint{Daemon: &Daemon{Host: "h", Module: "m", PasswordFile: "/home/u/.rsyncpw"}}, false},
		"ssh and daemon both set": {local, Endpoint{Path: "/r", SSH: &SSH{Host: "h"}, Daemon: &Daemon{Host: "h", Module: "m"}}, true},
		"ssh plus daemon remotes": {Endpoint{Path: "/l", SSH: &SSH{Host: "h"}}, Endpoint{Daemon: &Daemon{Host: "h", Module: "m"}}, true},
		"both daemon":             {Endpoint{Daemon: &Daemon{Host: "h", Module: "m"}}, Endpoint{Daemon: &Daemon{Host: "h", Module: "m"}}, true},
		"daemon no host":          {local, Endpoint{Daemon: &Daemon{Module: "m"}}, true},
		"daemon no module":        {local, Endpoint{Daemon: &Daemon{Host: "h"}}, true},
		"daemon dash host":        {local, Endpoint{Daemon: &Daemon{Host: "-h", Module: "m"}}, true},
		"daemon dash password":    {local, Endpoint{Daemon: &Daemon{Host: "h", Module: "m", PasswordFile: "-x"}}, true},
		"daemon password space":   {local, Endpoint{Daemon: &Daemon{Host: "h", Module: "m", PasswordFile: "/Users/u/Library/Application Support/dibs/a.pw"}}, false},
		"daemon password newline": {local, Endpoint{Daemon: &Daemon{Host: "h", Module: "m", PasswordFile: "/pw\nx"}}, true},
		"daemon slash in module":  {local, Endpoint{Daemon: &Daemon{Host: "h", Module: "m/sub"}}, true},
		"daemon colon in host":    {local, Endpoint{Daemon: &Daemon{Host: "h:1", Module: "m"}}, true},
		"daemon space in module":  {local, Endpoint{Daemon: &Daemon{Host: "h", Module: "m x"}}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := validate(c.local, c.remote)
			if (err != nil) != c.wantErr {
				t.Errorf("validate err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}
