package claude

import (
	"fmt"
	"slices"
	"strings"
)

// maxSystemArgBytes is the largest system prompt passed via argv. Linux limits
// a single argv string to 128 KiB; larger prompts are moved into the stdin
// transcript instead.
const maxSystemArgBytes = 100 << 10

const defaultSystemPrompt = "You are a helpful assistant."

const transcriptHeader = "Below is the conversation so far. Reply only with the assistant's next message, without any role label."

// renderPrompt flattens the OpenAI-style history into the text sent on stdin.
// A single user message is passed verbatim; longer histories become a labelled
// transcript ending with the final user turn. Role labels are plain text, so a
// message that imitates them can only confuse the model, never the process.
func renderPrompt(messages []Message) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("%w: no messages", ErrInvalidRequest)
	}
	for _, m := range messages {
		if m.Role != RoleUser && m.Role != RoleAssistant {
			return "", fmt.Errorf("%w: unsupported role %q", ErrInvalidRequest, m.Role)
		}
	}
	last := messages[len(messages)-1]
	if last.Role != RoleUser || strings.TrimSpace(last.Content) == "" {
		return "", fmt.Errorf("%w: the last message must be a non-empty user message", ErrInvalidRequest)
	}
	if len(messages) == 1 {
		return last.Content, nil
	}

	var b strings.Builder
	b.WriteString(transcriptHeader)
	for _, m := range messages {
		b.WriteString("\n\n[")
		b.WriteString(m.Role)
		b.WriteString("]\n")
		b.WriteString(m.Content)
	}
	return b.String(), nil
}

// buildArgs returns the CLI arguments (the prompt is sent on stdin). Every flag
// was verified against `claude --help` and real calls (see the package doc).
func buildArgs(model, system string) []string {
	return []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--tools", "",
		"--no-session-persistence",
		"--model", model,
		"--system-prompt", system,
		// Isolation: do not load the host's user/project settings, hooks, MCP
		// servers, skills or Chrome integration into chat requests.
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-chrome",
		"--permission-prompts", "none",
	}
}

// filterEnv removes variables that override the subscription login and, when a
// token is configured, sets CLAUDE_CODE_OAUTH_TOKEN explicitly.
func filterEnv(env []string, oauthToken string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "ANTHROPIC_API_KEY" || key == "ANTHROPIC_AUTH_TOKEN" {
			continue
		}
		if oauthToken != "" && key == "CLAUDE_CODE_OAUTH_TOKEN" {
			continue
		}
		out = append(out, kv)
	}
	if oauthToken != "" {
		out = append(out, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
	}
	return slices.Clip(out)
}
