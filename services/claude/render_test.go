package claude

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRenderPrompt(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		want     string // exact; empty means check only err
		wantErr  error
		contains []string
	}{
		{
			name:     "single user message is passed verbatim",
			messages: []Message{{Role: RoleUser, Content: "Say hi"}},
			want:     "Say hi",
		},
		{
			name: "history becomes a transcript ending with the final user turn",
			messages: []Message{
				{Role: RoleUser, Content: "2+2?"},
				{Role: RoleAssistant, Content: "4"},
				{Role: RoleUser, Content: "and 3+3?"},
			},
			contains: []string{"[user]\n2+2?", "[assistant]\n4", "[user]\nand 3+3?"},
		},
		{name: "empty", messages: nil, wantErr: ErrInvalidRequest},
		{
			name:     "last must be user",
			messages: []Message{{Role: RoleUser, Content: "a"}, {Role: RoleAssistant, Content: "b"}},
			wantErr:  ErrInvalidRequest,
		},
		{
			name:     "unknown role",
			messages: []Message{{Role: "tool", Content: "a"}, {Role: RoleUser, Content: "b"}},
			wantErr:  ErrInvalidRequest,
		},
		{
			name:     "blank final user",
			messages: []Message{{Role: RoleUser, Content: "  "}},
			wantErr:  ErrInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderPrompt(tt.messages)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("rendered prompt %q missing %q", got, c)
				}
			}
			if len(tt.contains) > 0 {
				// The final user turn must come last.
				if !strings.HasSuffix(strings.TrimSpace(got), "and 3+3?") {
					t.Errorf("final user turn is not last: %q", got)
				}
			}
		})
	}
}

func TestBuildArgs(t *testing.T) {
	args := buildArgs("sonnet", "be brief", false, "")
	has := func(flag string) int {
		for i, a := range args {
			if a == flag {
				return i
			}
		}
		return -1
	}
	for _, f := range []string{"-p", "--verbose", "--include-partial-messages", "--no-session-persistence", "--strict-mcp-config", "--disable-slash-commands", "--no-chrome"} {
		if has(f) < 0 {
			t.Errorf("missing flag %s in %v", f, args)
		}
	}
	for flag, val := range map[string]string{
		"--output-format":      "stream-json",
		"--model":              "sonnet",
		"--system-prompt":      "be brief",
		"--tools":              "",
		"--setting-sources":    "",
		"--permission-prompts": "none",
	} {
		i := has(flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != val {
			t.Errorf("flag %s: want value %q in %v", flag, val, args)
		}
	}
	for _, a := range args {
		if a == "--bare" {
			t.Error("--bare must never be used (it ignores the OAuth token)")
		}
	}
}

func TestFilterEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-api",
		"ANTHROPIC_AUTH_TOKEN=tok",
		"CLAUDE_CODE_OAUTH_TOKEN=old",
		"HOME=/h",
	}
	tests := []struct {
		name  string
		token string
		want  []string
	}{
		{"keeps oauth from env", "", []string{"PATH=/usr/bin", "CLAUDE_CODE_OAUTH_TOKEN=old", "HOME=/h"}},
		{"config token overrides", "new", []string{"PATH=/usr/bin", "HOME=/h", "CLAUDE_CODE_OAUTH_TOKEN=new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterEnv(in, tt.token)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildArgsExactLists(t *testing.T) {
	head := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
	build := func(tools, allowed, sources string, slash bool) []string {
		a := append([]string{}, head...)
		a = append(a, "--tools", tools)
		if allowed != "" {
			a = append(a, "--allowedTools", allowed)
		}
		a = append(a, "--no-session-persistence", "--model", "m", "--system-prompt", "s",
			"--setting-sources", sources, "--strict-mcp-config")
		if slash {
			a = append(a, "--disable-slash-commands")
		}
		return append(a, "--no-chrome", "--permission-prompts", "none")
	}
	tests := []struct {
		name   string
		skills bool
		web    string
		want   []string
	}{
		// The first two are the pre-web invocations, byte for byte.
		{"no skills, web none", false, "none", build("", "", "", true)},
		{"no skills, web unset", false, "", build("", "", "", true)},
		{"skills, web none", true, "none", build("Skill", "", "project", false)},
		{"skills, web unset", true, "", build("Skill", "", "project", false)},
		{"no skills, search", false, "search", build("WebSearch", "WebSearch", "", true)},
		{"no skills, full", false, "full", build("WebSearch,WebFetch", "WebSearch,WebFetch", "", true)},
		{"skills, search", true, "search", build("Skill,WebSearch", "WebSearch", "project", false)},
		{"skills, full", true, "full", build("Skill,WebSearch,WebFetch", "WebSearch,WebFetch", "project", false)},
		{"unknown mode enables nothing", false, "bogus", build("", "", "", true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildArgs("m", "s", tt.skills, tt.web)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestComposeSystem(t *testing.T) {
	tests := []struct {
		name   string
		skills bool
		web    string
		want   string
	}{
		{"nothing", false, "none", "base"},
		{"skills only", true, "none", "base\n\n" + skillsHint},
		{"web only", false, "search", "base\n\n" + webHint},
		{"web full only", false, "full", "base\n\n" + webHint},
		{"skills then web", true, "search", "base\n\n" + skillsHint + "\n\n" + webHint},
	}
	for _, tt := range tests {
		if got := composeSystem("base", tt.skills, tt.web); got != tt.want {
			t.Errorf("%s: system = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestWebHint(t *testing.T) {
	for _, want := range []string{"search the web", "recent events", "news", "scores", "prices", "knowledge cutoff", "cite"} {
		if !strings.Contains(webHint, want) {
			t.Errorf("webHint %q does not mention %q", webHint, want)
		}
	}
	if strings.ContainsAny(webHint, "\n") {
		t.Fatal("webHint must be a single line")
	}
}

func TestSkillsHint(t *testing.T) {
	if !strings.Contains(skillsHint, "Skill tool") || !strings.Contains(skillsHint, "description") {
		t.Fatalf("skillsHint = %q", skillsHint)
	}
	if strings.ContainsAny(skillsHint, "\n") {
		t.Fatal("skillsHint must be a single line")
	}
}
