// Package claude runs the Claude Code CLI headlessly and exposes it behind the
// small Backend interface. Every request is stateless: one fresh `claude`
// process per call, the full conversation is replayed in the prompt, and the
// process is killed (whole process group) on timeout or cancellation.
//
// # Spike findings (claude 2.x, Max subscription login, 2026-10-23)
//
// Invocation verified by real calls:
//
//		claude -p --output-format stream-json --verbose --include-partial-messages \
//		  --tools "" --no-session-persistence --model <m> --system-prompt <s> \
//		  --setting-sources "" --strict-mcp-config --disable-slash-commands \
//		  --no-chrome --permission-prompts none          # prompt on stdin
//
//	  - Prompt: read from stdin when no positional prompt is given (verified with
//	    a short prompt). Stdin avoids argv size limits and keeps the prompt out of
//	    the process list. Very long prompts were not exercised against the real
//	    CLI (the spike was limited to three calls); a pipe has no argv limit.
//	    --system-prompt stays on argv (verified); it is moved into the stdin
//	    transcript when larger than maxSystemArgBytes (Linux caps a single argv
//	    string at 128 KiB).
//	  - Config isolation is essential. Without --setting-sources "",
//	    --strict-mcp-config and --disable-slash-commands the child loaded the
//	    owner's user settings: SessionStart hooks ran, MCP servers connected,
//	    43 skills and 14 agents were registered and the first request cost
//	    38,623 cache-creation tokens (about USD 0.15). With the flags above the
//	    same request costs 868 tokens (about USD 0.004), no hooks, no MCP, no
//	    skills. The OAuth login keeps working with these flags; --bare must not
//	    be used because it ignores OAuth/keychain credentials.
//	  - `--tools ""` disables every tool, including the Skill tool, and
//	    --disable-slash-commands disables skills. See "Skills" below.
//	  - CLAUDE_CODE_OAUTH_TOKEN is not needed locally (init reported
//	    apiKeySource "none" and the stored login worked). It is still forwarded
//	    when configured, which is the container path. ANTHROPIC_API_KEY and
//	    ANTHROPIC_AUTH_TOKEN are removed from the child environment because they
//	    override the subscription login.
//	  - --input-format stream-json (replayed assistant turns) was not tested, so
//	    history is flattened into a transcript (see renderPrompt).
//
// Event stream (one JSON object per line on stdout; stderr was empty in all
// calls):
//
//	system/hook_started, system/hook_response   only without isolation flags
//	system/init        model (resolved id, e.g. claude-sonnet-5-5), tools, skills,
//	                   mcp_servers, apiKeySource, session_id, cwd, ...
//	system/status
//	rate_limit_event   rate_limit_info{status, rateLimitType, resetsAt, ...}
//	stream_event       event.type = message_start | content_block_start |
//	                   content_block_delta | content_block_stop |
//	                   message_delta | message_stop; text arrives as
//	                   content_block_delta with delta{type:"text_delta", text}
//	assistant          full message: model, content[], usage; stop_reason is null
//	                   here, the final one is on message_delta and on result
//	result             subtype ("success" even for failures), is_error, result
//	                   (final text or error text), stop_reason, usage{input_tokens,
//	                   output_tokens, cache_creation_input_tokens,
//	                   cache_read_input_tokens, ...}, total_cost_usd, modelUsage,
//	                   duration_ms, duration_api_ms, num_turns, session_id,
//	                   terminal_reason, api_error_status, ttft_ms
//
// Usage note for OpenAI mapping: input_tokens counts only uncached input; the
// prompt size is input + cache creation + cache read tokens.
//
// Errors observed: `--model nonexistent-model-xyz` exits with code 1, writes a
// one-line marker to stderr ("[claude-code:unrecognized_model] {...}") and
// emits a normal stream whose assistant message has model "<synthetic>" and a
// result with is_error=true, api_error_status=404, terminal_reason="api_error"
// and result="There's an issue with the selected model (...)". An
// unauthenticated run was NOT reproduced (the login was not touched); it is
// inferred from the documented behaviour: a result with is_error=true,
// api_error_status 401 and text such as "Invalid API key - Please run /login".
// The testdata files auth_error, rate_limited and truncated are synthetic
// derivations of the real success stream; success and invalid_model are real
// captures with ids and paths redacted.
//
// Error mapping lives in classifyResultError: 401/403 or login-ish text is
// ErrAuth, 429 or limit-ish text is ErrRateLimited, 404 mentioning a model is
// ErrInvalidModel, anything else is ErrCLI.
//
// # Skills
//
// WithSkills enables skills per run. The option takes a function so this
// package does not depend on the skills syncer; the app adapts the syncer's
// current snapshot. The function is called once before the process starts and
// its value is held for the whole run. When it reports a snapshot directory
// the invocation changes in exactly these ways, otherwise nothing changes:
//
//   - --tools "" becomes --tools Skill
//   - --setting-sources "" becomes --setting-sources project
//   - --disable-slash-commands is dropped (it also disables skills)
//   - the working directory is the snapshot directory, which contains only
//     .claude/skills/<name>/ (no settings, hooks, agents or commands; that is
//     why --plugin-dir is not used: plugin hooks would execute)
//   - the system prompt gets skillsHint appended (after the default prompt when
//     the caller gave none). The model chooses whether to call a skill and
//     skipped it on plain prompts, so the hint states the expectation.
//
// With skills the stream contains assistant tool_use blocks (name "Skill"),
// input_json_delta deltas, user tool_result lines and a second assistant turn
// holding the final answer (testdata/skill_invocation.stream.jsonl is a real
// capture, num_turns is 3). The parser ignores everything but text deltas, so
// tool JSON never reaches clients. Result.Text is the accumulated text of all
// assistant turns, which is exactly what a streaming client received; both
// Stream and Complete return it (the CLI's result.result only holds the last
// turn and is used only when no partial messages arrived). When a turn emits
// text after an earlier turn already emitted text, its first delta is prefixed
// with a blank line. Usage and cost come from the final result line and cover
// all turns.
//
// # Web tools (T10 spike, claude 2.1.309, Max login, haiku, 2026-10-23)
//
// Config -claude-web (none|search|full; unset means none). Verified with two
// real calls from a temp directory plus one end-to-end gateway call:
//
//   - Syntax: `--tools` is variadic and takes one comma-separated value,
//     `--tools WebSearch,WebFetch` (init listed both tools); with skills it is
//     `--tools Skill,WebSearch`. `--tools WebSearch` alone works with
//     `--setting-sources ""` (init tools: [WebSearch]).
//   - Permissions: `--tools` only makes a tool available. With
//     `--permission-prompts none` the call was DENIED automatically
//     (system/permission_denied, tool_result "no approval surface", result
//     permission_denials[0].tool_name WebSearch) and the model apologised.
//     Adding `--allowedTools WebSearch,WebFetch` made it run, so buildArgs
//     always sets both. Skill needs no --allowedTools (verified earlier).
//   - Stream shape: WebSearch is an ordinary client tool: an assistant
//     tool_use block {name:"WebSearch", input:{query, mode}}, input_json_delta
//     stream events, then a user tool_result line whose content is a text blob
//     "Web search results for query ... Links: [...]". There are no
//     server_tool_use / web_search_tool_result blocks in the CLI stream; the
//     parser still ignores them defensively. The Spanish Barcelona question
//     made 2 searches (num_turns 3) and the answer cited sources; the
//     testdata/web_search.stream.jsonl fixture is that capture (ids, paths
//     and signatures redacted, tool_result truncated).
//   - Counting: usage.server_tool_use.web_search_requests stayed 0 although
//     two searches ran; modelUsage.<model>.webSearchRequests was 2. The parser
//     takes the largest of modelUsage, usage.server_tool_use and the tool_use
//     blocks. WebFetch was not exercised against the real CLI (the model chose
//     not to fetch), so web_fetch_requests handling is unproven live.
//   - Cost: the same question cost USD 0.026 (haiku, 3 turns, 25 s,
//     usage 5.1k cache-creation + 6.2k cache-read tokens, modelUsage input
//     33.6k because the nested search calls are counted there) against about
//     USD 0.004 for a plain prompt; through the gateway with skills and web it
//     reported 31.2k prompt tokens, USD 0.039 and 28 s.
//   - Failure: the only failure observed is the denial above (no error exit,
//     is_error false). Unavailability of WebSearch for an account/model was
//     not forced; per the docs it depends on provider and plan.
//   - Where it runs (https://code.claude.com/docs/en/tools-reference, "WebSearch
//     tool behavior" and "WebFetch tool behavior"): WebSearch queries
//     Anthropic's web search backend and returns titles and URLs; WebFetch
//     fetches the page from the machine running the CLI and summarizes it with
//     a separate model call. The docs say WebFetch refuses localhost and
//     hostnames without a dot; they state the private/link-local/metadata
//     address denial only for the WebSocket tool, so WebFetch blocking of
//     private IPs is UNVERIFIED and treated as absent. In manual modes WebFetch
//     prompts per domain and a domain safety check sends the domain to
//     Anthropic (https://code.claude.com/docs/en/security). Headless runs have
//     no prompt surface, hence --allowedTools. That is why `full` is opt-in.
//
// When web tools are active the system prompt gets webHint after skillsHint.
//
// Close removes the private temp working directory; the app calls it at
// shutdown.
package claude
