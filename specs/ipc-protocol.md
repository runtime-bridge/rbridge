# rbridge IPC protocol, version 1

The daemon listens on a Unix domain socket, `bridge.sock` in its runtime directory (Linux `$XDG_RUNTIME_DIR/rbridge/`, macOS `$TMPDIR/rbridge-<uid>/`, Windows `%LocalAppData%\rbridge\run\`; `RBRIDGE_HOME/run/` when set). The directory is mode 0700 and the socket 0600, so only the user who started the daemon can connect; there is no token. Windows 10 and later support Unix domain sockets natively.

## Framing

JSON-RPC 2.0, one message per line, UTF-8, `\n` after each message, no newlines inside a message. Requests may be sent without waiting for earlier answers; responses carry the request id.

## Handshake

The first request must be `initialize`:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"client":"my-app","protocol_version":1}}
```

`client` names the application, 1 to 64 letters, digits, `.`, `-` or `_` (default `default`). Sessions are visible only to the client that opened them. The result is `{"protocol_version":1,"version":"<rbridge version>"}`.

## Methods

Parameters and results are the same JSON objects as the HTTP API (`specs/http-api.yaml`).

| Method | Params | Result |
|---|---|---|
| `daemon.status` | none | version, pid, started_at, sessions_open |
| `providers.list` | `refresh` | provider status list |
| `providers.get` | `id`, `refresh` | provider status |
| `skills.list` | none | skill records |
| `skills.get` | `id` | skill record |
| `skills.add` | `path` (absolute) or `url` + `rev` (+ `subdir`), `link`, or `catalog` (`catalog:id` or `id`) | add result |
| `skills.add_many` | `paths` (absolute folders, each a skill or a folder of skills) or `url` + `rev` (+ `subdirs`, none meaning the whole repository); `only`, `exclude` (names or `*` patterns), `link`, `dry_run`, `trust` | `{"items":[{"id","source","status","result","reason"}],"warnings":[...]}`; `status` is `added`, `updated`, `unchanged`, `would_add`, `skipped` or `failed`, and one failed skill does not stop the rest |
| `skills.search` | `query` | `{"skills":[...],"problems":[...]}` from the configured catalogs |
| `skills.trust` | `id`, `trust` | skill record |
| `skills.remove` | `id` | `{"removed":true}` |
| `sessions.open` | `task`, `model`, `mode`, `input` | session record |
| `sessions.resume` | `id`, `model`, `input` | session record |
| `sessions.list` | none | this client's records |
| `sessions.get` | `id` | record |
| `sessions.subscribe` | `id`, `after` | `{"subscribed":true}`, then notifications |
| `sessions.unsubscribe` | `id` | `{"unsubscribed":true}` |
| `sessions.prompt` | `id`, `prompt` (string or content blocks), `wait` (default true) | result, or `{"started":true}` |
| `sessions.cancel` | `id` | `{"cancelled":true}` |
| `sessions.close` | `id` | `{"closed":true}` |
| `sessions.approvals` | `id` | pending approval requests |
| `approvals.answer` | `session_id`, `approval_id`, `allow` or `option_id`, `remember` | `{"answered":true}` |
| `sessions.modes` | `id` | `{"current":...,"modes":[...]}` |
| `sessions.set_mode` | `id`, `mode` | modes after the change |
| `sessions.options` | `id` | runtime settings |
| `sessions.set_option` | `id`, `option_id`, `value` (string or boolean) | settings after the change |
| `sessions.inputs` | `id` | pending input requests |
| `inputs.answer` | `session_id`, `input_id`, `action` (accept, decline, cancel), `content` | `{"answered":true}` |
| `run` | `task`, `model` | result (open, one turn, close) |
| `audit.list` | `since` (RFC 3339) | audit records |

## Notifications

After `sessions.subscribe`, the daemon sends every buffered event with a sequence number above `after`, then new events as they happen:

```json
{"jsonrpc":"2.0","method":"session.event","params":{"event":{"seq":7,"type":"message.delta","session_id":"ses_...","data":{"text":"..."}}}}
```

When the session closes the daemon sends `{"jsonrpc":"2.0","method":"session.end","params":{"id":"ses_..."}}`. A subscriber that falls more than 256 events behind is dropped; it can subscribe again with the last sequence number it saw.

`approval.required` events carry an approval request (`id`, `title`, `kind`, `tool`, `options`). Answer with `approvals.answer`; a request nobody answers within `policy.approval_timeout` is denied. Requests the policy decides on its own produce only `approval.resolved`. With `remember`, the same tool and kind is allowed for the rest of the session; it never overrides a policy denial.

`input.required` events carry a question from the runtime (`id`, `message`, `mode` form or url, `schema` or `url`). They are sent only to sessions opened with `input: true`; other sessions have the runtime's questions declined at once. Answer with `inputs.answer`. rbridge never opens a URL itself. Every question ends with `input.resolved`.

`sessions.modes` and `sessions.options` expose the native modes and settings of ACP runtimes. Claude Code sessions return `capability_unavailable`, because its permission modes could bypass policy; use profiles instead.

## Errors

```json
{"jsonrpc":"2.0","id":5,"error":{"code":-32000,"message":"...","data":{"code":"session_not_found","message":"...","details":{}}}}
```

`error.data.code` is one of rbridge's stable error codes. `error.data.retryable` is true when the same request may succeed later, such as when every session slot is busy; the SDKs retry such errors when opening a session.

A task may bring `files` (absolute paths): rbridge copies them into a working folder of their own (the workspace, when the task gives none, removed when the session closes), writes a text copy of each Word file beside it, and tells the runtime about them in the first turn. A task's `skills` may name folders by absolute path: every skill in the folder is added to the library and trusted as reviewed, unless a trust decision was already made for it.

A task may set `slot_wait` (nanoseconds): how long its session queues for a free slot and, in the daemon, for a place under `daemon.max_sessions_per_client`. Negative waits until one frees; zero uses `execution.slot_wait`. A task's `model` may be `latest`, `auto`, a name from `providers.<id>.models`, or a model the runtime accepts; results carry `model`, the model the runtime reported running. JSON-RPC codes: -32700 parse error, -32601 unknown method, -32602 invalid handshake parameters, -32002 not initialized, -32000 every rbridge error.

## Lifetime

Closing the connection cancels turns started on it with `wait` true and drops its subscriptions. Sessions stay open in the daemon and can be used from a new connection by the same client until they are closed, the daemon stops, or they are idle for `daemon.session_idle_timeout`.
