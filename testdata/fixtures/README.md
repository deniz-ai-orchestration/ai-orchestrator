# Provider CLI fixtures (PC2, Ubuntu 24.04)

These are real headless runs of the four coding-agent CLIs, captured on 2026-10-02 to build orch's provider parsers.

Each case lives in `<cli>/<case>/` and holds these files:

- `argv.txt`: the exact argv, any prompt sent on stdin, extra env (secrets redacted) and the files the run changed in the scratch repo
- `stdout`, `stderr`: captured separately
- `exit_code`: 137 means the harness killed the run with `timeout -s KILL`
- `out.json` (codex only): the `-o/--output-last-message` file

All runs used a scratch git repo at `/tmp/orch-scratch` with a 7-line `main.go`. Before each case the repo was reset to the same commit. Every CLI ran under `env -i` with only `HOME`, `PATH`, `TERM=dumb` and `LANG`, so no parent-session variables leaked in. `schema.json` is the structured-output schema used by the claude, codex and agy cases.

Prompts:

- edit: "In main.go, add a function sub(a, b int) int … Then reply with one sentence."
- ro: "Read main.go and explain in one sentence what it prints. Do not modify any files."
- question: "Before changing anything, I need you to ask me which package name I want. Do not edit any files; just ask the question and stop."

## Versions

| CLI | version | model used |
|---|---|---|
| claude | 2.1.287 (Claude Code) | `sonnet` (init reports `claude-sonnet-5-5`) |
| codex | codex-cli 0.160.0 | `gpt-6.1-sol`, `CODEX_HOME=$HOME/.config/orch/codex` |
| agy | 1.2.14 | `gemini-3.1-pro-high` (full list in `agy/models.txt`) |
| opencode | 1.18.34 | `opencode-go/kimi-k2.7-code` |

## Scrubbing

These replacements were made in every captured file:

- `/home/<user>` became `$HOME`.
- Every UUID (session_id, thread_id, conversation_id, message uuids) became `00000000-0000-4000-8000-00000000000N`. The mapping is consistent within a file.
- Ids prefixed `ses_`, `msg_`, `prt_`, `req_`, `err_` and `toolu_` became `<prefix>_XXX…N`.
- `cf-ray` values were zeroed.
- The names of the claude.ai connectors attached to the account, and their MCP tool names, became `claude.ai Connector` / `mcp__claude_ai_Connector__…`.

I grepped the result for `sk-`, `ghp_`, `github_pat`, `Bearer`, `@` and `token`. The only remaining hits are `*_tokens` usage fields, `@builtin` plugin ids and `@@` diff hunks.

## claude

The base command is `claude -p --model sonnet --output-format stream-json --verbose --permission-mode bypassPermissions --max-turns 5`. The prompt goes on stdin.

Stream shape: one JSON object per line (NDJSON).

1. The run opens with `system/init`, which lists tools, MCP servers, skills, plugins and `claude_code_version`.
2. Next comes `system/commands_changed`, then the conversation as `assistant` / `user` lines (tool_use and tool_result live inside `message.content`).
3. The last line is always `type:"result"`, except on timeout.

Result fields: `subtype`, `is_error`, `result` (text), `structured_output`, `errors[]`, `terminal_reason`, `stop_reason`, `permission_denials[]`, `total_cost_usd`, `usage` and `modelUsage`.

**Usage and quota:** claude emits `rate_limit_event` lines. Each one has `rate_limit_info.status` (`allowed`), `rateLimitType` (`five_hour`), `resetsAt` (unix seconds), `overageStatus`, and `unifiedWindows.{five_hour,seven_day}.utilization` (0–1) plus `resetsAt`. This is the only CLI here that reports subscription quota.

| case | exit | what to parse |
|---|---|---|
| success | 0 | `result/success`, `is_error:false`, main.go modified |
| json_schema (`--json-schema '<schema>'`) | 0 | `structured_output` is the parsed object; `result` is the same object as a JSON string |
| read_only (`--disallowedTools Edit,Write,NotebookEdit`) | 0 | Still `subtype:"success"` and `permission_denials:[]`. The model explains in prose that Edit is disabled. **Bash is not disallowed**, so this is not a real sandbox. Add `Bash` or use `--permission-mode plan` if you need a guarantee. |
| question | 0 | A plain success whose `result` text is a question. There is no machine-readable "blocked" signal without a schema. |
| bad_model | 1 | stderr: `[claude-code:unrecognized_model] {...}`. Result is `subtype:"success"` with `is_error:true` and `terminal_reason:"api_error"`, and the text names the model. |
| auth_fail (HOME pointed at an empty dir) | 1 | `subtype:"success"`, `is_error:true`, `result:"Not logged in · Please run /login"`, `terminal_reason:"api_error"` |
| max_turns (`--max-turns 1`) | 1 | `subtype:"error_max_turns"`, `is_error:true`, `errors:["Reached maximum number of turns (1)"]`, `terminal_reason:"max_turns"` |
| timeout (killed at 4s) | 137 | No result line; the stream just ends mid-conversation |

Surprises:

- **Use `is_error`, not `subtype`.** On api errors (bad model, not logged in) `subtype` stays `"success"`. Check `is_error` and `terminal_reason` (`completed` / `api_error` / `max_turns`).
- The default auth on this box is the `~/.claude` login. These runs did not set `CLAUDE_CODE_OAUTH_TOKEN`.
- **The account's environment leaks into every run.** Without isolation, the user's claude.ai MCP connectors (including ones in `needs-auth`), user skills and plugins all load. orch should pass `--strict-mcp-config` (with an empty or explicit `--mcp-config`) and probably `--setting-sources project`. In the question case the model also spontaneously told the user to authorize connectors.
- Init carries `memory_paths.auto`, which points at `$HOME/.claude/projects/<cwd>/memory/`, so auto-memory is on by default in headless runs.

## codex

The base command is `codex exec -m gpt-6.1-sol --json --sandbox <mode> --output-schema schema.json -o out.json -`. The prompt goes on stdin.

Stream shape: one JSON object per line.

- The run starts with `thread.started` (`thread_id`) and `turn.started`.
- Work appears as `item.started` / `item.completed` with `item.type` set to `agent_message`, `command_execution` (command, aggregated_output, exit_code, status), `file_change` (`changes[].path` and `kind`) or `error`.
- The run ends with `turn.completed` (carrying `usage`) or `turn.failed` (carrying `error.message`).

**Usage and quota:** usage appears only in `turn.completed.usage`, as `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens` and `reasoning_output_tokens`. There is no quota, rate-limit or cost info in the stream.

| case | exit | what to parse |
|---|---|---|
| read_only | 0 | The final `agent_message.text` is the schema JSON as a string. `out.json` holds the same JSON. |
| workspace_write | 0 | `file_change` item, plus a `go run` it chose to do; main.go modified |
| edit_in_read_only | 0 | The model returns `{"status":"blocked",...}` and does not attempt the write. It inferred read-only from its instructions. |
| question | 0 | `{"status":"blocked","summary":"Which package name would you like?"}`. The schema's `blocked` value is a usable question signal. |
| no_schema | 0 | Same stream; the final `agent_message.text` is prose |
| bad_model | 1 | First an `item.completed` of type `error` ("Model metadata … not found. Defaulting to fallback"), then `error` and `turn.failed`. The message is a JSON string with `status:400` and `invalid_request_error`, "…not supported when using Codex with a ChatGPT account". |
| auth_fail (empty temp `CODEX_HOME`) | 1 | About 15s of retries: `error` events "Reconnecting... N/5 (unexpected status 401 Unauthorized …)" over wss, a fallback to HTTPS, five more retries, then `turn.failed`. stderr has a WARNING plus tracing ERROR lines. |
| timeout (killed at 5s) | 137 | Only `thread.started` and `turn.started` |

Surprises:

- **Codex retries on 401.** Auth failure costs about 15s of retries before failing. Match on `401 Unauthorized` in the `error` events to fail fast.
- Bad model is not rejected locally; it comes back as a 400 from the server.
- The `error` and `item.type:"error"` events are not fatal by themselves. Only `turn.failed` is terminal.
- With a temp `CODEX_HOME` under /tmp, codex warns "Refusing to create helper binaries under temporary dir".
- Rate limits never occurred in these runs, so there is no fixture for them.

## agy (Antigravity)

The base command is `agy -p "<prompt>" --model gemini-3.1-pro-high --output-format stream-json`.

Stream shape: NDJSON keyed by `event`, not `type`.

1. The run opens with `{"event":"init","init":{model,cwd,tools[],...}}`.
2. Next come `{"event":"step_update","step_update":{step_index,state:ACTIVE|DONE,step_type:user_input|agent_response|tool,...}}` events. Tool steps carry `tool_name` and `tool_info.parameters`. Agent responses stream through `text_delta`, and each DONE step has its own `usage`.
3. The run ends with `{"event":"result","result":{status:SUCCESS|ERROR,response,error,num_turns,duration_seconds,usage,structured_output?}}`.

**Usage and quota:** usage appears per step and in total, as `input_tokens`, `output_tokens`, `thinking_tokens`, `cache_read_tokens` and `total_tokens`. There is no quota or rate-limit info. `agy models` lists ids with display names.

| case | exit | what to parse |
|---|---|---|
| success (`--dangerously-skip-permissions`) | 0 | `status:SUCCESS`; main.go modified via `replace_file_content` |
| no_skip_perms | 0 | **It edited the file anyway.** Print mode does not block on permissions here. |
| read_only | 0 | Plain success |
| json_schema (`--json-schema '<schema>'`) | 0 | `result.structured_output` is clean. `response` holds the JSON (with extra `toolAction`/`toolSummary` keys) **plus** a prose sentence after it, so parse `structured_output`, not `response`. The result also echoes `json_schema`. |
| question | 0 | A plain success whose response is a question |
| bad_model | 1 | stderr: `error: invalid model selection … Available models: …`. stdout: `result` with `status:"ERROR"` and the same text in `error`. |
| print_timeout (`--print-timeout 3s`) | **0** | stderr: `[agy] print timeout after 3s with turn in progress; returning partial output`. The result says `status:"SUCCESS"` with an empty `response` and zero usage. **Treat this as a timeout, not success.** |
| stdin_dash (`-p -`) | 0 | agy does not read stdin. It took `-` literally as the prompt and answered "Hello! How can I help you today?" |
| timeout (killed at 5s) | 137 | Only the first `step_update` |
| empty_home_still_authed (HOME pointed at an empty dir) | 0 | Still authenticated, because the login is in the system keyring, not HOME |

Surprises:

- **No auth-failure fixture.** I could not simulate an auth failure without touching the real keyring, so I did not. The `empty_home_still_authed` case documents that HOME isolation does not log agy out.
- **The prompt must be argv.** When `-p` is followed by another flag, agy errors (exit 2): `-p took "--model" as its prompt…`. Prompt size is therefore bounded by ARG_MAX, and the prompt is visible in `ps`.
- `--print-timeout` exists and is the cleanest timeout, but it exits 0 with SUCCESS (see above).
- Model ids are `gemini-3.1-pro-high` / `-low`, `gemini-3.{6,7,8}-flash-{high,medium,low}`, `claude-sonnet-4-6`, `claude-opus-4-6-thinking` and `gpt-oss-120b-medium`.
- Responses contain `file://` markdown links with absolute paths.

## opencode

The base command is `opencode run --format json --model opencode-go/kimi-k2.7-code "<prompt>"`.

Stream shape: NDJSON with `type` set to `step_start`, `tool_use`, `text`, `step_finish` or `error`. Every event has `timestamp`, `sessionID` and `part`. A `tool_use` part has `tool`, `state.status`, `state.input`, `state.output` and `metadata`. There is **no terminal result event**: the run is over when you see the last `step_finish` with `part.reason:"stop"` (intermediate steps say `tool-calls`).

**Usage and quota:** usage appears per `step_finish` in `part.tokens` (`total`, `input`, `output`, `reasoning`, `cache.read`, `cache.write`). `part.cost` is in USD. There is no quota info.

| case | exit | what to parse |
|---|---|---|
| success | 0 | Edit tool with a diff in metadata; main.go modified |
| read_only | 0 | Plain success |
| plan_agent_edit (`--agent plan`, edit prompt) | 0 | The model refuses to edit ("I'm in read-only plan mode…") and main.go is unchanged. This is the read-only mode for opencode. |
| question | 0 | A plain success whose text is a question |
| stdin_prompt (no positional prompt) | 0 | **opencode reads the prompt from stdin.** It worked. |
| bad_model / bad_model_unprefixed | 1 | A single `error` event: `UnknownError` "Unexpected server error. Check server logs for details." with an `err_` ref. A bare `kimi-k2.7-code` fails the same way, so the prefix is required. |
| auth_fail (`OPENCODE_API_KEY=invalid` + empty `XDG_DATA_HOME`) | 1 | `error` event `APIError` with `statusCode:401`, "Invalid API key.", `isRetryable:false`, `responseBody` and `metadata.url` |
| no_auth (empty `XDG_DATA_HOME`, no key) | 1 | The same opaque `UnknownError` as bad_model |
| env_key_invalid_but_stored_auth (`OPENCODE_API_KEY=invalid` only) | 0 | **Stored auth in `$XDG_DATA_HOME/opencode` takes precedence over the env key.** The run succeeded. |
| timeout (killed at 5s) | 137 | Partial: a `step_finish` with `reason:"tool-calls"` and no final stop |

Surprises:

- **Errors can be opaque.** A bad model and missing auth produce the same `UnknownError`. Only a wrong key gives a clean 401.
- **orch must isolate opencode's data dir.** Because stored auth beats `OPENCODE_API_KEY`, orch should set `XDG_DATA_HOME`, or the key in the env is silently ignored.
- **Errors go to stdout.** Errors exit 1 with the error on stdout, not stderr. In every case, stderr was empty.
- There is no `--json-schema` / output-schema flag in `opencode run --help`.
