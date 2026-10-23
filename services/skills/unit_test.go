package skills

import (
	"strings"
	"testing"
)

func TestBuildSSHCommand(t *testing.T) {
	tests := []struct {
		name       string
		key, hosts string
		want       string
	}{
		{"nothing configured", "", "", "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes"},
		{"key and known hosts", "/k/id", "/k/kh",
			"ssh -i '/k/id' -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o 'UserKnownHostsFile=/k/kh' -o StrictHostKeyChecking=yes"},
		{"key only uses system known hosts", "/k/id", "",
			"ssh -i '/k/id' -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o StrictHostKeyChecking=yes"},
		{"known hosts only", "", "/k/kh",
			"ssh -o BatchMode=yes -o 'UserKnownHostsFile=/k/kh' -o StrictHostKeyChecking=yes"},
		{"paths with spaces", "/my keys/id", "/my keys/kh",
			`ssh -i '/my keys/id' -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o 'UserKnownHostsFile="/my keys/kh"' -o StrictHostKeyChecking=yes`},
		{"single quote in path", "/it's/id", "",
			`ssh -i '/it'\''s/id' -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o StrictHostKeyChecking=yes`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSSHCommand(tt.key, tt.hosts)
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
			if strings.Contains(got, "StrictHostKeyChecking=no") {
				t.Error("must never disable host key checking")
			}
		})
	}
}

func TestIsSSHRepo(t *testing.T) {
	tests := map[string]bool{
		"git@github.com:o/r.git":      true,
		"ssh://git@github.com/o/r":    true,
		"https://github.com/o/r.git":  false,
		"file:///srv/r.git":           false,
		"/srv/r.git":                  false,
		"./r:with-colon":              false,
		"../relative/repo":            false,
		"host:path/repo":              false,
		"user@host.example.com:a/b.c": true,
	}
	for repo, want := range tests {
		if got := isSSHRepo(repo); got != want {
			t.Errorf("isSSHRepo(%q) = %v, want %v", repo, got, want)
		}
	}
}

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"simple", "---\nname: a\ndescription: Does a thing.\n---\nbody", map[string]string{"name": "a", "description": "Does a thing."}, false},
		{"crlf", "---\r\nname: a\r\ndescription: x\r\n---\r\n", map[string]string{"name": "a", "description": "x"}, false},
		{"bom", "\xef\xbb\xbf---\nname: a\n---\n", map[string]string{"name": "a"}, false},
		{"quoted", "---\nname: \"a\"\ndescription: 'it''s ok: yes'\n---\n", map[string]string{"name": "a", "description": "it's ok: yes"}, false},
		{"folded", "---\nname: a\ndescription: >\n  first line\n  second line\n---\n", map[string]string{"name": "a", "description": "first line second line"}, false},
		{"literal strip", "---\nname: a\ndescription: |-\n  one\n  two\n---\n", map[string]string{"name": "a", "description": "one two"}, false},
		{"plain continuation", "---\nname: a\ndescription: starts here\n  and continues\n---\n", map[string]string{"name": "a", "description": "starts here and continues"}, false},
		{"comments and blanks", "---\n# note\n\nname: a\n---\n", map[string]string{"name": "a"}, false},
		{"colon in value", "---\nname: a\ndescription: Use when: summarizing\n---\n", map[string]string{"name": "a", "description": "Use when: summarizing"}, false},
		{"no opening", "name: a\n", nil, true},
		{"unterminated", "---\nname: a\n", nil, true},
		{"invalid utf8", "---\nname: \xff\n---\n", nil, true},
		{"empty", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFrontmatter([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestCleanRelativeSource(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"./plugins/core", "plugins/core", false},
		{"plugins/core/", "plugins/core", false},
		{".", "", false},
		{"./", "", false},
		{"../outside", "", true},
		{"a/../../outside", "", true},
		{"a/../b", "", true},
		{"/etc", "", true},
		{`a\b`, "", true},
		{"", "", true},
		{"a/\x00b", "", true},
	}
	for _, tt := range tests {
		got, err := cleanRelativeSource(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("cleanRelativeSource(%q) = %q, %v; want %q (err %v)", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestSanitize(t *testing.T) {
	in := "Load key \"/home/u/.config/key\": bad\x00\x1b[31m red " + strings.Repeat("z", 5000)
	got := sanitize(in, "/home/u/.config/key")
	if strings.Contains(got, "/home/u/.config/key") {
		t.Error("secret path not redacted")
	}
	if strings.ContainsAny(got, "\x00\x1b") {
		t.Errorf("control characters not stripped: %q", got)
	}
	if len(got) > maxErrBytes+16 {
		t.Errorf("message not capped: %d bytes", len(got))
	}
	if sanitize("plain", "") != "plain" {
		t.Error("empty secret must be ignored")
	}
}
