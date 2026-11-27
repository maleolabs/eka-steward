# ACP client spike — steward ↔ Agent Client Protocol

**Work item:** `eka/sto:acp-client-spike` (milestone M7, repo `eka-steward`)
**Status:** spike complete — evidence from live agents + a scripted mock.
**Consumers:** `adr:steward-acp-bridge` (draft at the end), `internal/render`
(context handoff), the future `internal/acp` client.

This is a **report**, not production code. It records the ACP methods actually
exercised, the raw JSON-RPC frames observed (secrets redacted), the v1↔v2
differences that matter to the steward, the permission-callback behaviour, and
the structured-context handoff. Every claim below is either a pasted frame or an
explicit "not observed".

Ground truth: the ACP specification snapshot in `/tmp/opencode/acp/`
(`v1_*.md`, `v2_*.md`, and `v2_migration.md`). Real-agent observations were
taken on this machine on 2026-11-27.

---

## 1. Environment and method

| Tool | Version | ACP entry point |
| - | - | - |
| `opencode` | 2.4.1 | `opencode acp` (stdio JSON-RPC) |
| `gemini` | 0.42.0 | `gemini --acp` (`--experimental-acp` deprecated) |
| `claude` / `codex` / `aider` | — | do **not** speak ACP (not used) |

All traffic is JSON-RPC 2.0, one object per line, over the child's stdin/stdout.
The probe harness is a throwaway Python stdio client; it spawns the agent,
writes requests, drains stdout on a reader thread, and records every frame after
redacting `sk-…`, `AIza…`, `ghp_…`, and `Bearer …` tokens (none appeared).

Repository changes: this document plus the harness under `internal/acp/probe/`
(non-Go `.py` scripts only — `probe.py`, `init_probe.py`, `methods_probe.py`,
`turn_probe.py`, `context_probe.py`; `mock_agent.py` + `mock_drive.py` for the
permission demo). No `.go` file, no `os/exec` import, no new top-level
directory, and no Go package was added: `internal/acp/probe/` contains no `.go`
files, so `go build/vet/test ./...` never compiles it and no boundary test sees
it. Build/test proof is in §10.

---

## 2. Real-agent handshakes (`initialize`)

### 2.1 opencode — full request and response

Request (v1 client shape; `clientCapabilities` + `clientInfo`):

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true},"clientInfo":{"name":"eka-steward-probe","version":"0.0.1"}}}
```

Response (actual bytes):

```json
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true,"sse":false},"promptCapabilities":{"embeddedContext":true,"image":true},"sessionCapabilities":{"additionalDirectories":{},"close":{},"delete":{},"fork":{},"list":{},"resume":{}},"_meta":{"opencode/child-session-updates":true}},"authMethods":[{"description":"Run `opencode auth login` in the terminal","name":"Login with opencode","id":"opencode-login"}],"_meta":{"steering":{"supported":true}},"agentInfo":{"name":"OpenCode","version":"2.4.1"}}}
```

Notes:
- It answers **`protocolVersion: 1`** even when the client requests `2` (see §2.3). It is a v1 agent that happens to implement several v2-named session methods (`session/resume`, `session/close`, `session/list`, `session/fork`) under v1 field names and v1 capability booleans.
- It advertises `authMethods` (terminal login). We did **not** run `authenticate`; the machine already had a usable model configured, so prompt turns completed anyway (§4).
- `promptCapabilities.embeddedContext: true` — a `resource` content block is accepted in `session/prompt` (used in §7).

### 2.2 gemini — full response

```json
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"authMethods":[{"id":"oauth-personal","name":"Log in with Google","description":"Log in with your Google account"},{"id":"gemini-api-key","name":"Gemini API key","description":"Use an API key with Gemini Developer API","_meta":{"api-key":{"provider":"google"}}},{"id":"vertex-ai","name":"Vertex AI","description":"Use an API key with Vertex AI GenAI API"},{"id":"gateway","name":"AI API Gateway","description":"Use a custom AI API Gateway","_meta":{"gateway":{"protocol":"google","restartRequired":"false"}}}],"agentInfo":{"name":"gemini-cli","title":"Gemini CLI","version":"0.42.0"},"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true,"audio":true,"embeddedContext":true},"mcpCapabilities":{"http":true,"sse":true}}}}
```

gemini also answers `protocolVersion: 1` to a `protocolVersion: 2` request. It is
v1-only on the wire.

### 2.3 Version negotiation observed

| Client asks | opencode returns | gemini returns |
| - | - | - |
| `protocolVersion: 1` | `1` | `1` |
| `protocolVersion: 2` (v2-shaped `capabilities`/`info`) | `1` (ignored the v2 fields) | `1` |

This is exactly the v1 rule ("respond with the latest version it supports") and
the migration note that a v1 agent reads a v2 request without the v1
`clientCapabilities`/`clientInfo`, so fs/terminal support is silently lost. A
**real v2 agent was not available on this machine**; the v2 frames in this
report are produced by the mock in §6 and cite `v2_migration.md` for shapes.

**Consequence for the steward:** it must send the highest version it supports
(2 once implemented), then act on the returned `protocolVersion` per connection,
and must send a v1-shaped handshake when the agent selects `1` (otherwise it
loses `embeddedContext`/fs capability negotiation — see §7).

---

## 3. Method exercise table

"v1 present / v2 present" = defined by the spec revision (per
`/tmp/opencode/acp/v1_*.md` and `v2_migration.md`). "Observed" = actually driven
against `opencode acp` (real), `gemini --acp` (real), or the scripted mock.

| method | v1 present | v2 present | observed | notes |
| - | - | - | - | - |
| `initialize` | yes | yes (restructured) | yes (opencode, gemini, mock) | v2 renames `clientCapabilities/clientInfo`→`capabilities/info`; objects not booleans |
| `authenticate` (v1) / `auth/login` (v2) | yes | yes (renamed) | no | opencode/gemini advertise `authMethods`; model already usable, login not needed |
| `logout` (v1) / `auth/logout` (v2) | optional | yes (required when `authMethods` non-empty) | no | not exercised |
| `session/new` | yes (baseline) | yes (baseline) | yes (opencode) | returns `sessionId` (+ `configOptions`); v2 `mcpServers` optional |
| `session/load` | optional (`loadSession`) | **removed** | yes (opencode) | replayed 2 `session/update` chunks before the response; v2 folds this into `session/resume` + `replayFrom` |
| `session/resume` | optional (`sessionCapabilities.resume`) | yes (baseline) | yes (opencode) | returned `configOptions`; v1 resume does not replay history |
| `session/list` | optional (`sessionCapabilities.list`) | yes (baseline) | yes (opencode) | returned prior sessions with `cwd`/`title`/`updatedAt` |
| `session/close` | optional | yes (baseline) | yes (opencode) | `result: {}` |
| `session/delete` | optional (`session.delete`) | optional (`session.delete`) | yes (opencode) | `result: {}` |
| `session/fork` | draft only | draft only | no | advertised by opencode; not driven (draft, out of scope) |
| `session/prompt` | yes (baseline) | yes (baseline, **semantics changed**) | yes (opencode) | v1: response is the turn; v2: response = `messageId` ack, work reported via `session/update` |
| `session/cancel` (notification) | yes | yes | yes (opencode) | opencode returned `stopReason:"cancelled"` |
| `session/update` (notification) | yes | yes (variants changed) | yes (opencode) | observed variants in §4 |
| `session/set_mode` | optional | **removed** | no | modes become config options in v2 |
| `session/set_config_option` | optional | required if config advertised | yes (opencode) | opencode exposes `model`, `effort`, `mode` config options |
| `session/request_permission` (agent→client) | yes (client baseline) | yes (client baseline, params restructured) | **mock only** | opencode auto-approved every tool action and never asked (§6.1) |
| `$/cancel_request` (either direction) | yes | yes | mock only | client used it to bound a permission wait (§6.3) |
| `tool_call/cancel`, `terminal/cancel` | — | **new in v2** | no | background-work controls; not exercised |
| `fs/read_text_file`, `fs/write_text_file` | optional (client) | **removed** | no | no agent called the client for files |
| `terminal/create|output|release|wait_for_exit|kill` | optional (client) | **removed** | no | not exercised |
| `elicitation/create`, `elicitation/complete` | optional | optional | no | not exercised |

---

## 4. Prompt-turn lifecycle, observed (real opencode)

### 4.1 A normal turn (v1)

Request and response (abridged to the turn-defining frames):

```json
{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_de3dba9f…","prompt":[{"type":"text","text":"Reply with exactly the word PONG and nothing else."}]}}
```

```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_de3dba9f…","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_21c245c36001…","content":{"type":"text","text":"PONG"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_de3dba9f…","update":{"sessionUpdate":"usage_update","used":22890,"size":200000,"cost":{"amount":0,"currency":"USD"}}}}
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn","usage":{"inputTokens":22886,"outputTokens":4,"totalTokens":22890},"_meta":{}}}
```

Key v1 fact reproduced: **the `session/prompt` response is the end of the turn**;
`stopReason` rides the response. The client keeps reading `session/update`
notifications while the request is pending.

Observed `sessionUpdate` variants from real opencode turns:
`available_commands_update`, `agent_thought_chunk`, `agent_message_chunk`,
`tool_call`, `tool_call_update`, `usage_update`.

### 4.2 Cancellation (v1)

Long prompt, then `session/cancel` after ~3 s:

```json
{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"ses_de3d6b06…"}}
{"jsonrpc":"2.0","id":8,"result":{"stopReason":"cancelled","_meta":{}}}
```

### 4.3 `session/load` replay (v1)

`session/load` replayed the prior exchange as ordinary `session/update`s before
its response (this is what v2 turns into `session/resume` + replay):

```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_de3d6b06…","update":{"sessionUpdate":"user_message_chunk","messageId":"msg_21c296b74001…","content":{"type":"text","text":"Say DONE."}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_de3d6b06…","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_21c296bfc001…","content":{"type":"text","text":"DONE"}}}}
{"jsonrpc":"2.0","id":6,"result":{"configOptions":[ … ]}}
```

Note: opencode replays message chunks **without** a preceding empty-content
primer. In v2 that would double content unless the client keys by `messageId`;
`v2_migration.md` requires retained user messages to preserve the prompt's
`messageId` and advises an empty-content primer when replaying as chunks.

### 4.4 `session/new` shape (opencode)

```json
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"ses_de3d6b068ffeV0QkKMNWGAcS2U","configOptions":[{"id":"model",…},{"id":"effort",…},{"id":"mode",…}]}}
```

`configOptions` ids observed: `model`, `effort`, `mode` (with `mode` carrying
opencode agent profiles). v2 renames `id`→`configId`, drops `modes`, and requires
`configOptions` in every `session/new`/`session/resume` response.

### 4.5 gemini session setup failure (honest record)

`gemini --acp` initializes fine, but `session/new` fails:

```json
{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"This client is no longer supported for Gemini Code Assist for individuals. To continue using Gemini, please migrate to the Antigravity suite of products: https://antigravity.google"}}
```

So **no gemini prompt turn completed**. `-32000` is the ACP "authentication /
agent-side failure" class. This is a product/gating failure, not a protocol
mismatch; the v1 handshake itself is healthy.

---

## 5. v1 vs v2 differences (explicit)

Grounded in `v2_migration.md` plus the capabilities the real agents actually
advertise.

### 5.1 `initialize`
- v1: `params.{protocolVersion, clientCapabilities, clientInfo}` → result `{protocolVersion, agentCapabilities, agentInfo, authMethods}`. Booleans (`image: true`) and objects (`list: {}`) are mixed.
- v2: `params.{protocolVersion, capabilities, info}` (role-agnostic; `info` **required**) → same field names from the agent. Every support marker is an **object**: presence = supported.
- Version selection: v1 agent returns its latest; v2 agent returns the highest supported **≤ requested**, else rejects with `-32022` and `data.{requested, supported}`. Repeating `initialize` is allowed in v2 before the first other request.
- Observed: both real agents are v1 and returned `1` for a `2` request.

### 5.2 Session setup
- v1: `session/new{cwd, mcpServers}` and optional `session/load{sessionId}` (replay, gated by `loadSession`) / `session/resume` (no replay, gated by `sessionCapabilities.resume`).
- v2: `session/load` **removed**; `session/resume{sessionId, cwd, mcpServers, replayFrom?}` does both (`replayFrom:{type:"start"}` = replay; omit = reattach), plus a mandatory `replay_complete` update at the end of a replay. `session/new` `mcpServers` becomes optional. `session/list` and `session/close` are baseline (no markers). `modes` is gone.
- Observed: opencode (v1) supports both `session/load` (replay) and `session/resume` (no replay); a v2 client must stop calling `session/load`.

### 5.3 Prompt-turn lifecycle (the big one)
| signal | v1 | v2 |
| - | - | - |
| prompt accepted | implicit | `session/prompt` response `{messageId}` |
| user message in history | the request itself | `user_message`/`user_message_chunk` update with agent `messageId` |
| work running | prompt request stays pending | `state_update{state:"running"}` |
| work waiting on user | pending permission request | `state_update{state:"requires_action"}` |
| work ended | prompt response `{stopReason}` | `state_update{state:"idle", stopReason}` |
| cancelled | prompt response `{stopReason:"cancelled"}` | idle `state_update{stopReason:"cancelled"}` |
| failure after insertion | error response on prompt | idle `state_update{stopReason:"error", error}` |

Also: `session/prompt` gains `idempotencyKey`; a held prompt's response stays
pending until insertion; `refusal` no longer means "drop subsequent messages";
`error` is a new stop reason.

Observed v1: `{stopReason:"end_turn"}` on the response. Mock v2: immediate
`{messageId}` ack, then `state_update` running → requires_action → idle (§6).

### 5.4 Tool calls and permission
- v1 `session/request_permission.params` = `{sessionId, toolCall, options}`; the
  bare `toolCall` doubles as prompt copy.
- v2 = `{sessionId, title (required), description?, subject?, options}` where
  `subject` is a tagged union: `{type:"tool_call", toolCall}` or
  `{type:"command", command, cwd, toolCallId?, terminalId?}`. Prompt text no
  longer mutates the tool-call title.
- `session/update` tool variant: v1 has `tool_call` (create) + `tool_call_update`
  (patch). v2 **removes** `tool_call`; the first `tool_call_update` for an id
  creates it, and patch semantics are explicit (omit = unchanged, `null` =
  clear). v2 adds `cancelled`/`paused` statuses, `tool_call_content_chunk`, and
  the Agent-owned `terminal_update`/`terminal_output_chunk`.
- v2 adds `tool_call/cancel` and `terminal/cancel`, and the `$/
  cancel_request` notification for cancelling a single request.
- Observed: opencode emitted v1 `tool_call` + `tool_call_update` and **never**
  sent `session/request_permission` (§6.1).

### 5.5 Capability changes that affect the steward
- v1 `promptCapabilities.{image,audio,embeddedContext}` → v2 `capabilities.session.prompt.{image,audio,embeddedContext}` as objects.
- v1 `loadSession` removed; v1 `sessionCapabilities.{list,resume,close}` markers removed (advertising `session:{}` implies all baseline methods).
- Client `fs.*` and `terminal` capabilities/methods removed in v2; expose file/command tools via MCP instead.
- v1 `plan` capability + `plan` variant → v2 `plan_update`/`plan_removed` (no capability).
- Notices/compaction/subagents lose their client opt-in markers in v2.
- ID renames: auth `id`→`methodId`, config `id`→`configId`, group→`groupId`.

### 5.6 Transport and errors
- Both: stdio, one JSON object per line, no batches.
- v2: request IDs may not be `null`; no JSON-RPC batches; ordered delivery;
  client shutdown = close agent stdin.
- Error codes: `-32002` unknown session (now a MUST), `-32601` unimplemented,
  `-32600` request before init, `-32800` request cancelled, `-32022` unsupported
  protocol version, `-32000` auth/agent failure (observed from gemini).

---

## 6. Permission-callback path (allow / deny / bounded timeout)

### 6.1 Real agents did not request permission

Across four tool-triggering prompts against `opencode acp` — a shell `echo`, a
`rm -rf` command that its own config maps to `ask`, a read of `/etc/hostname`
(outside the workspace root), and an external-directory access — opencode
executed the tool and streamed `tool_call` / `tool_call_update` **without ever
sending `session/request_permission`**. A project-local `opencode.json` setting
`permission.bash/* = ask` did not change this. Inspection of the installed
opencode bundle shows the ACP server *does* wire `permission.asked` →
`connection.requestPermission(...)` with options
`[{optionId:"once",kind:"allow_once"},{optionId:"always",kind:"allow_always"},{optionId:"reject",kind:"reject_once"}]`,
so the gate that did not fire is opencode's own permission resolution in ACP
mode (it resolves to allow), not a missing ACP path. **Honest conclusion:
permission round-trips are not observable with opencode 2.4.1 as configured.**

Because of that, the permission path is demonstrated against a **scripted mock
ACP agent** that emits `session/request_permission` on a fixed cue. The mock is
a real stdio JSON-RPC server (`internal/acp/probe/mock_agent.py`); it is not an
LLM. It speaks v1 or v2 depending on the negotiated `protocolVersion`. The
client answers per policy.

### 6.2 v2 allow branch — exact frames

Client request (v2 initialize, then `session/new`, then `session/prompt`):

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"capabilities":{},"info":{"name":"eka-steward-probe","title":"EKA Steward Probe","version":"0.0.1"}}}
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":2,"capabilities":{"session":{"prompt":{"embeddedContext":{}},"mcp":{"stdio":{},"http":{}}}},"info":{"name":"mock-acp-agent","title":"Mock ACP Agent","version":"0.0.1"},"authMethods":[]}}
{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp/opencode/acp-probe/work","mcpServers":[]}}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess_mock_1"}}
```

Prompt is acknowledged (insertion), then work is reported via updates:

```json
{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"sess_mock_1","prompt":[{"type":"text","text":"run the setup script"}]}}
{"jsonrpc":"2.0","id":3,"result":{"messageId":"msg_user_1"}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"user_message","messageId":"msg_user_1","content":[{"type":"text","text":"run the setup script"}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"state_update","state":"running"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_perm_1","title":"Run setup command","kind":"execute","status":"pending"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"state_update","state":"requires_action"}}}
```

Permission request (v2 shape), answer, and outcome:

```json
{"jsonrpc":"2.0","id":"mock-1001","method":"session/request_permission","params":{"sessionId":"sess_mock_1","title":"Run this command?","description":"The agent wants to execute `echo hello-world`.","subject":{"type":"tool_call","toolCall":{"toolCallId":"call_perm_1","title":"Run setup command","kind":"execute","status":"pending"}},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"reject-once","name":"Reject","kind":"reject_once"}]}}
{"jsonrpc":"2.0","id":"mock-1001","result":{"outcome":{"outcome":"selected","optionId":"allow-once"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_perm_1","status":"in_progress"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_perm_1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"hello-world"}}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"agent_message","messageId":"msg_a1","content":[{"type":"text","text":"Command ran."}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}}}
```

### 6.3 v2 deny branch

Same up to the permission request. Client answers:

```json
{"jsonrpc":"2.0","id":"mock-1001","result":{"outcome":{"outcome":"selected","optionId":"reject-once"}}}
```

Agent outcome (no execution):

```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_perm_1","status":"failed","content":[{"type":"content","content":{"type":"text","text":"Permission denied"}}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}}}
```

### 6.4 Bounded timeout branch (client never hangs)

Policy `timeout` means the client keeps the request but does not answer it. It
enforces a hard bound and then aborts the request instead of waiting forever:

```
PERMISSION TIMEOUT: waited 4.00s (bound 4.0s) then sent $/cancel_request requestId=mock-1001
```

Frame the client emits:

```json
{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"mock-1001"}}
```

The v2 agent, on `$/cancel_request` for its pending request, resolves it as
`cancelled` (the spec's "sender stopped waiting" path) and finishes the turn:

```json
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_perm_1","status":"failed","content":[{"type":"content","content":{"type":"text","text":"Permission denied"}}]}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_mock_1","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"cancelled"}}}
```

Measured client behaviour: it resolved in **4.00 s** (the configured bound) and
did not block; total wall time stayed under the 20 s harness deadline. This is
the property the steward needs: a permission request that is never answered
degrades to "cancelled", never to a hung worker.

### 6.5 v1 permission frames (shape contrast)

v1 request uses a bare `toolCall` and no `title`/`subject`:

```json
{"jsonrpc":"2.0","id":"mock-1001","method":"session/request_permission","params":{"sessionId":"sess_mock_1","toolCall":{"toolCallId":"call_perm_1","title":"Run setup command","kind":"execute","status":"pending"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"reject-once","name":"Reject","kind":"reject_once"}]}}
```

v1 ends the turn on the prompt response:

```json
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}
```

v1 deny ends with `{"stopReason":"refusal"}` from the mock. v1 has **no**
`$/cancel_request`-driven idle state; the client's bound would instead have to
answer `cancelled` and the agent returns `{stopReason:"cancelled"}`.

---

## 7. Structured-context handoff (shown, against a real agent)

The steward's payload is the deterministic markdown from `internal/render`
(`internal/render/testdata/context.golden` is the pinned representative output).
It was delivered to **real `opencode acp`** as a `resource` content block
(`embeddedContext`, which opencode advertises) plus the task as a baseline
`text` block, in `session/prompt`:

```json
{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_…","prompt":[
  {"type":"resource","resource":{"uri":"file:///tmp/opencode/acp-probe/context-payload.md","mimeType":"text/markdown","text":"# eka/sto:context-renderer — bounded context payload\n\n## Instructions\n\n- Render deterministically.\n- Never write EKA.\n\n## Constraints\n\n- stdlib only.\n- No new dependency.\n\n## Acceptance\n\n- Byte-stable output.\n- Budget enforced.\n\n## Knowledge\n\ndocs/context.md describes the payload shape.\n\n## Note\n\nThe caller maps EKA knowledge into Input.\n"}},
  {"type":"text","text":"Using ONLY the attached context document, reply with the acceptance criteria as a single line."}
]}}
```

Observed result — the agent read the `resource` block and answered from its
`## Acceptance` section:

```json
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn","usage":{…},"_meta":{}}}
```

Agent text: `Byte-stable output; budget enforced.`

**Decision: SHOWN, not deferred.** The handoff mechanism is:
1. Render the payload with `internal/render.Render` (already byte-bounded, 8 KiB default).
2. Send it as `{"type":"resource","resource":{uri,mimeType:"text/markdown",text}}` when the agent advertises `promptCapabilities.embeddedContext` (both real agents do).
3. Fall back to a `{"type":"text"}` block carrying the same bytes when it does not — `text` and `resource_link` are baseline in both v1 and v2.
4. Task instruction is a separate `text` block so the payload stays a pure document. In v2 the allowed set is gated by `capabilities.session.prompt.embeddedContext` and an unadvertised type must be rejected with `-32602`; the client must choose the block type from the negotiated capability.

No MCP server is needed for context: it is a one-shot document, not a tool.
`systemPrompt` on `session/new` was not used because opencode does not advertise
`systemPrompt`, and it is not baseline in either version.

---

## 8. Security and transport notes relevant to the steward

- **Trust:** the agent is a subprocess; it may ask the client to execute
  filesystem/terminal operations (v1) or ask for permission to run tools (both).
  The steward is an orchestrator, so a default-allow policy is unacceptable.
- **Permission policy (proposed, see ADR):** deny-by-default; allow only
  `read`-class tool calls inside the session workspace root; escalate
  `execute`/`edit`/`delete`/`move` to an explicit allow decision; never select
  `allow_always` without a human; answer `cancelled` to any request that is
  superseded by `session/cancel`; enforce a wall-clock bound on every
  permission wait.
- **Subprocess boundary:** the actual `os/exec` spawn must stay in
  `internal/escapehatch` (or a new audited adapter), because
  `cmd/boundary_arch_test.go` forbids `os/exec` elsewhere in non-test code. The
  ACP client can be built on `escapehatch.Run` only if it grows a streaming
  stdio pipe; the current `Run` captures whole stdout, so a streaming client
  will need a small, equally audited adapter. This spike did not change that
  boundary.
- **Redaction:** frames in this report were scrubbed of `sk-…`/`AIza…`/Bearer
  tokens; none were present in ACP traffic (auth material stays in the agent).
- **Errors as data:** both `-32000` (gemini gating) and `stopReason:"cancelled"`
  must be surfaced without crashing the steward loop.

---

## 9. What could not be determined

- **No real v2 agent exists on this machine.** Both real agents negotiate
  `protocolVersion: 1`. All v2 frames are from the mock and the spec; v2 wire
  behaviour against a production agent is unverified.
- **Real permission round-trip** could not be triggered from opencode 2.4.1
  (auto-approves in ACP mode). Demonstrated against the mock instead.
- **`auth/login` / `authenticate`** was not exercised; opencode had a usable
  model already and gemini is gated server-side.
- **`session/fork`**, `tool_call/cancel`, `terminal/cancel`, `elicitation/*`,
  `fs/*`, `terminal/*` were not exercised.
- **Gemini prompt turns** are blocked by a product gate (`-32000`), not protocol.
- **Replay/`replay_complete`** was only observed in its v1 form (`session/load`
  chunks, no completion marker); the v2 `replay_complete` marker is spec-only
  here.

---

## 10. Verification (repo changes: this doc + non-Go probe scripts)

```
export GOPROXY=off GOSUMDB=off
gofmt -l .                                   # (empty)
go build ./...                               # BUILD_OK
go vet ./...                                 # VET_OK
go test ./...                                # all packages ok
grep -rn "os/exec" --include=*.go . | grep -v _test
# ./internal/escapehatch/escapehatch.go:25:	"os/exec"   ← only occurrence
```

### Reproduce

```bash
# real handshakes (v1 and v2-shaped requests)
python3 internal/acp/probe/init_probe.py

# real opencode method surface (session/new, set_config_option, list, prompt,
# load, resume, cancel, close, delete)
python3 internal/acp/probe/methods_probe.py

# real prompt turn (edit the prompt/agent via argv/env in the script)
AGENT=opencode python3 internal/acp/probe/turn_probe.py /tmp/acp-work allow "Reply with PONG."

# gemini (initialize succeeds; session/new returns -32000 product gate)
AGENT=gemini python3 internal/acp/probe/turn_probe.py /tmp/acp-work allow "Reply with PONG."

# structured-context handoff to real opencode
python3 internal/acp/probe/context_probe.py

# permission allow / deny / bounded-timeout against the mock (v1 and v2)
python3 internal/acp/probe/mock_drive.py allow   2
python3 internal/acp/probe/mock_drive.py deny    2
PERM_BOUND=4 python3 internal/acp/probe/mock_drive.py timeout 2
python3 internal/acp/probe/mock_drive.py allow   1
```

(`init_probe.py` / `methods_probe.py` / `turn_probe.py` / `context_probe.py`
and the `probe.py` library are the harness; `mock_agent.py` and `mock_drive.py`
are the mock agent and its driver. They live under `internal/acp/probe/`, are
non-Go, are never compiled or imported by repo Go code, and never spawn from
repo Go code.)

---

## 11. ADR draft — `adr:steward-acp-bridge`

**Title:** Steward ACP bridge — target version, method set, permission policy
**Status:** Proposed
**Context:**
`eka-steward` is an orchestrator that hands a bounded, deterministic context
payload (`internal/render`) to a coding agent and drives one prompt turn. It
must do this over the Agent Client Protocol without becoming a second EKA write
path and without spawning subprocesses outside the audited escape hatch. ACP v2
is published as a breaking revision; the agents actually installed here
(`opencode` 2.4.1, `gemini` 0.42.0) negotiate **v1 only**, and opencode
implements several v2-named session methods under v1 shapes. v2 moves completion
out of the `session/prompt` response into `state_update`, removes `session/load`,
removes client `fs`/`terminal`, and restructures permission requests.

**Decision:**
1. **Version.** Speak **ACP v1 (`protocolVersion: 1`) as the shipping target
   now**, behind a per-connection version dispatcher. Send the highest version
   the client supports (initially `1`); if a future agent returns `2`, route to
   a v2 handler. Do **not** request `2` until a v2 handler exists, because a v2
   request against a v1 agent silently drops `clientCapabilities` (losing
   `embeddedContext`). Treat v2 as additive per `v2_migration.md`.
2. **Method set (v1 today).** `initialize`; `session/new`; `session/prompt`;
   consume `session/update`; `session/cancel`; answer `session/request_permission`;
   optional `session/load`/`session/resume` for reattach; optional
   `session/set_config_option` (model/effort/mode); `session/close`. When v2 is
   enabled: same names except `session/load`→`session/resume`+`replayFrom`,
   `session/set_mode`→`session/set_config_option`, and completion via idle
   `state_update`.
3. **Structured context.** Deliver the `internal/render` payload in
   `session/prompt` as a `resource` block when the agent advertises
   `embeddedContext`, else a `text` block; keep the task instruction as a
   separate `text` block. No `systemPrompt` (not baseline, not advertised).
4. **Permission policy.** Deny by default. Allow `read`-class tool calls only
   inside the session workspace root. Escalate `execute`/`edit`/`delete`/`move`
   to an explicit decision; never auto-select `allow_always`. Bound every
   permission wait (default 120 s, configurable); on expiry or on `session/cancel`,
   answer `cancelled` (v2: also send `$/cancel_request` for the pending request)
   so the worker never hangs. Never treat an unknown `optionId`/`outcome` as
   approval.
5. **Subprocess boundary.** The stdio spawn stays in the audited
   `internal/escapehatch` family; a streaming-stdio adapter is added there (or a
   sibling audited package) rather than importing `os/exec` anywhere else.

**Consequences:**
- Ships against the agents developers actually have; no v2 dead-end.
- The renderer's 8 KiB byte budget fits comfortably in one `resource` block,
  and the handoff was verified end-to-end against opencode.
- v2 adoption is a handler swap, not a rewrite, but requires implementing the
  new prompt lifecycle (`state_update`), message IDs, upsert tool calls, and
  `$/cancel_request` before flipping the advertised version.
- Permission prompts are bounded and default-deny, which trades some autonomy
  for safety; operators can widen the allow-list explicitly.
- opencode's ACP mode currently auto-approves tools, so the steward cannot rely
  on the agent to surface every dangerous action; the steward's own command
  policy and workspace-root checks remain load-bearing.
- `escapehatch.Run` currently captures whole output; a streaming pipe is a new
  (small, audited) surface to add before implementing the client.

**Open questions:** wire behaviour against a real v2 agent; whether to expose
file access via an MCP server (v2 direction) for richer context; exact
`allow_always` scoping semantics per agent.