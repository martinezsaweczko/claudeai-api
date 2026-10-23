// Package skills keeps a local, validated, versioned copy of a private git
// repository of Claude skills and exposes it as a directory the Claude CLI can
// use as its working directory.
//
// # How it works
//
// A Syncer maintains a bare cache clone under <skills-dir>/repo (git binary, no
// working tree, no hooks, no scripts ever executed), resolves the configured
// ref (branch, tag or full sha) to a commit, reads the tree with git plumbing
// (ls-tree + cat-file), validates every skill and materializes ONLY the
// validated skill directories into <skills-dir>/versions/<commit>/.claude/skills/<name>/.
// The version directory is written to a temp dir, made read-only (dirs 0555,
// files 0444, no executable bits, no symlinks) and renamed into place, so a
// request that is already running on an older version is never disturbed.
// Snapshot.Dir is the directory to use as the CLI cwd. The last three versions
// are kept.
//
// Layout discovery: .claude-plugin/marketplace.json is read and, for each
// plugin whose source is a relative path inside the repo, <source>/skills/<name>/
// is collected (remote sources and sources with "..", absolute or backslash
// paths are skipped and reported). Without a marketplace file a root skills/
// directory is used. Per-plugin manifests, hooks, MCP, agents and commands are
// never read or copied.
//
// Validation (invalid skills are skipped with a recorded reason, never fatal):
// directory name [a-z0-9-]{1,64}; SKILL.md with frontmatter name (equal to the
// directory name) and description (<=1024 bytes); only regular files (symlinks,
// submodules and special files reject the skill); <=256 KiB per file, <=64
// files and <=2 MiB per skill, <=100 skills. Duplicate names: the first one in
// marketplace order wins.
//
// # Spike findings (Claude Code 2.1.309, model haiku, 4 real `claude -p` calls)
//
// Baseline flags: -p --output-format stream-json --verbose --setting-sources ""
// --strict-mcp-config --no-chrome --permission-prompts none (see services/claude).
//
// A. Project skills (approach A, CHOSEN).
//
//   - `--setting-sources ""` does NOT load <cwd>/.claude/skills (init event
//     "skills" listed only the bundled ones; summarize was absent, call 1).
//   - `--setting-sources project` DOES load them. The init event lists
//     "summarize" first among "skills" (call 2) and the Skill tool runs
//     non-interactively with just `--tools Skill`: no --allowedTools and no
//     permission flag change were needed (`--permission-prompts none` stays).
//   - Removing `--disable-slash-commands` is required (it disables skills and
//     commands altogether). `--tools Skill` is the minimal tools value that
//     enables only the Skill tool (init event: "tools":["Skill"]).
//   - Evidence from the real private repo, materialized by this package
//     (call 4, prompt "Use the summarize skill to summarize ..."):
//     {"type":"tool_use","name":"Skill","input":{"skill":"summarize","args":"..."}}
//     followed by {"type":"tool_result","content":"Launching skill: summarize"}
//     and the final answer being a faithful one-sentence summary.
//   - A plain prompt "Summarize in one sentence: ..." (call 2) was answered
//     directly by haiku WITHOUT invoking the skill: invocation is the model's
//     choice, driven by the skill description. Gateway skills with sharper
//     descriptions (or an explicit "use the X skill" in the system prompt)
//     trigger more reliably.
//   - Winning flag set: `--tools Skill --setting-sources project
//     --strict-mcp-config --no-chrome --permission-prompts none`, cwd =
//     Snapshot.Dir, and NO --disable-slash-commands.
//   - Token cost: with skills enabled the prompt is ~7.8k tokens (call 1:
//     6206 cache-creation + 1581 cache-read; call 2 with summarize: 3458 +
//     4383) versus the 868-token baseline with skills disabled. Almost all of
//     the increase is Claude Code's ~22 BUNDLED skills (deep-research, design,
//     slides, ..., plugin-authoring) plus the Skill tool, not the gateway's
//     skills (summarize added ~54 tokens). Most of it is a cache read after
//     the first request. No flag found to drop the bundled skills while
//     keeping the Skill tool (--disable-slash-commands drops both).
//
// B. --plugin-dir (approach B, REJECTED for security).
//
//   - Works and is repeatable (`--plugin-dir A --plugin-dir B`), skills are
//     namespaced (pa:dup, pb:dup) and the init event lists the plugins.
//   - BUT a plugin loaded this way also loads its hooks, agents and commands
//     under the same isolation flags. A SessionStart hook in the plugin's
//     hooks/hooks.json ran (the marker file it touched was created): a
//     compromised skills repo would get code execution on the gateway host.
//     Its agents (pa:evilagent) and commands (pa:evilcmd) were registered too.
//     Only MCP servers were blocked (--strict-mcp-config: mcp_servers stayed
//     empty, the plugin's .mcp.json command never ran).
//   - Hence approach A: copy only validated skills/<name>/ trees, so hooks,
//     MCP, agents and commands in the synced repo are never on disk in the CLI
//     cwd and therefore never loaded.
//
// C. Naming.
//
//   - Plugin skills are namespaced <plugin>:<skill>. Two plugins with a skill
//     of the same name were both kept (pa:dup, pb:dup); the model's first call
//     with the bare name `dup` matched both, it then invoked both.
//   - With approach A all skills live in one namespace (.claude/skills/<name>),
//     so duplicates cannot coexist: this package keeps the first and skips the
//     rest with a reason. It also requires frontmatter name == directory name,
//     because the precedence between the two was not verified in the spike.
//
// D. Invocation: confirmed in call 4 (see A).
//
// # What remains exposed with approach A
//
//   - Skill prose is prompt content: a malicious skill can instruct the model
//     (prompt injection). With `--tools Skill` the model has no Bash, file or
//     network tool, so the blast radius is the text of the answer.
//   - `--setting-sources project` makes the CLI read the project scope of its
//     cwd and (not verified in the spike) its ancestors: .claude/settings*.json,
//     CLAUDE.md and .mcp.json in parent directories may be loaded. Snapshot.Dir only holds .claude/skills,
//     but the state directory's ancestors must not contain such files (keep
//     -skills-dir under a dedicated directory). User settings (~/.claude) stay
//     ignored because "user" is not in the list.
//   - Skill supporting files may mention scripts; they can only run if a
//     Bash-capable tool is enabled, which the gateway never does.
//   - Bundled skills still load (see token cost).
//
// # Operational notes
//
//   - Git access uses only paths from the config (ssh key, known_hosts). The
//     environment is non-interactive (GIT_TERMINAL_PROMPT=0, no askpass, no
//     ambient git config, GIT_ALLOW_PROTOCOL=file:ssh:https:http) and for ssh
//     URLs GIT_SSH_COMMAND pins the key (IdentitiesOnly, no agent), BatchMode
//     and StrictHostKeyChecking=yes; host key verification is never disabled.
//   - Each sync is bounded by -skills-sync-timeout; git runs in its own
//     process group that is killed on timeout or cancel.
//   - Failures keep the last good snapshot. After a failure Run retries with
//     exponential backoff (15s, doubling) capped by the refresh interval.
package skills
