# JSON output reference

Use `pipkin usage --json` for a fresh reading or `pipkin status --json` for the
background helper's cached snapshot. Neither command needs a USB display.
Use `pipkin tasks --json` for local GUI task observations on macOS and Windows.
The schema is defined by [`Status`](../internal/pipkin/helper.go) and
[`Reading`](../internal/pipkin/readings.go), with task observations defined by
[`TaskSnapshot`](../internal/pipkin/tasks.go). Examples below use invented data.

## Commands and errors

| Command | Result |
| --- | --- |
| `pipkin usage --json` | Collects both providers independently. Initializes local configuration if needed; does not update the helper's cache, retry schedule or display. |
| `pipkin status --json` | Reads the saved snapshot and replaces `pid` with the current helper PID or zero. Does not contact providers or probe USB. |
| `pipkin tasks --json` | Reads existing local GUI task state with a bounded collection budget. Does not contact provider servers, read credentials, start chats or require an installed/running Pipkin helper. |
| `pipkin usage` | Human-readable fresh usage and provider errors. |
| `pipkin status` | Human-readable helper, display, cached allowance and local task summaries. Does not create settings or lock files. |
| `pipkin tasks` | Human-readable observed task counts, with partial coverage or unknown counts stated explicitly. |

Bare `pipkin` shows the short status view and a help hint when installed. It
shows the command menu when the installation is absent or its location cannot be
resolved. `pipkin help`, `-h` and `--help` always show the menu.

JSON commands write one indented object and a newline to stdout. Read the whole
object after the command exits and keep stderr separate. Text output and object
key order are not interfaces.

| Exit status | Meaning |
| --- | --- |
| `0` | Command completed. Provider data can still be unavailable; inspect `states` and errors for allowance or provider `coverage` for tasks. Cached status may have `pid: 0`. |
| `1` | Command failed, for example due to invalid arguments, unreadable configuration or a missing/malformed snapshot. stderr contains `pipkin: ...`; no JSON error envelope is emitted. |
| `2` | Unknown command; stderr contains an error and a suggested command or help. Suggestions never run a command. |

`status --json` reports a missing or unreadable snapshot as “no helper snapshot
yet” and suggests `usage --json`. Provider errors alone do not change the exit
status of `usage --json`.

The text views use aligned provider rows. `status` shows Session and Weekly;
`usage` adds reported plan slugs, model limits, credits, extra-usage spend and
limits, and banked resets. Unknown amounts remain unknown; confirmed zero
balances remain visible. A missing sign-in is a calm state, while expired or
mismatched sign-ins include a next step. These presentation changes do not alter
the JSON schema or which sources each command reads; error wording is not a
stable interface.

Reset countdowns omit zero units, for example `36m` or `7d`. Readings at least
five minutes old show their age. Failed reads with retained quota are labelled
"Last known reading"; expired resets say "awaiting refresh" without refilling the
allowance. `usage` prints a one-line progress message only to an interactive
stderr stream, and never in JSON mode.

Text status distinguishes not installed, stopped and starting. A missing snapshot
or one belonging to the previous helper can mean starting; malformed or unreadable
state remains an error. Display diagnostics use saved connection information and
do not probe USB. A saved port-open error describes the last connection attempt.
The local task summary uses only fresh saved observations, deduplicates task IDs,
and labels partial, unavailable or unsupported coverage. Stopped or starting
helpers do not supply current counts. Run `pipkin tasks` for a fresh local check
on macOS or Windows; the plain status view never opens a GUI connection.

## Snapshot

`schema_version` is `1`, independent of the software and USB protocol versions.
Ignore unknown optional keys within this schema; reject an unsupported schema
version. Providers are keyed as `codex` and `claude` and can appear independently.

| Field | Type / presence | Meaning |
| --- | --- | --- |
| `schema_version` | Integer, always | JSON schema version. |
| `version` | String, always | CLI version for fresh usage; writing helper's version for cached status. |
| `pid` | Integer, always | Fresh usage emits `0`; cached status reports the current helper PID or `0`. |
| `device` | Object or null, always | Fresh usage emits null. Helper snapshots contain `{}`, a connection or an error. |
| `states` | Provider → string; omitted when empty | Collection state for each provider that has completed a read. |
| `readings` | Provider → reading, always | Fresh usage includes both providers, with placeholders on failure. Helper startup may have `{}` or only one provider. |
| `codex_error`, `claude_error` | String; omitted when empty | Latest collection error. Wording is not a stable error code. |
| `updated` | Unix seconds, always | Collection start time for fresh usage; last changed snapshot time for the helper. |
| `tasks` | Task snapshot; omitted before the first local task scan and in fresh usage | Local GUI task observations, independent of subscription availability. See below. |

A connected `device` contains string fields `port` and `firmware`. A failed
port-open attempt can supply `error`. `{}` means no recorded connection or error;
null means USB was not inspected. A cached connection is last known, especially
when `pid` is zero. It does not indicate provider availability.

## Provider states and freshness

| State | Meaning |
| --- | --- |
| `available` | A subscription reading was collected. Individual metrics may still be missing; the provider app need not be open. |
| `unavailable` | Networking, permissions, unsupported storage, malformed data or another collection error. The helper retains its last quota and observation time. Fresh usage has no previous quota. |
| `signed_out` | No usable sign-in, an expired sign-in, incompatible auth mode or confirmed account mismatch. Previous quota and identity are cleared. |
| Key absent | The helper has not finished its first collection for this provider. |

Treat an unfamiliar state as unknown. JSON has no `stale`, `loading`, retry time
or in-flight flag. Use `observed` for reading age, `updated` for snapshot age and
`pid` for helper status. Reading a cache never makes its contents fresh. If a
reset has passed, retain the last allowance and show “Awaiting refresh.” Do not
refill a gauge until the provider confirms new usage.

The firmware marks readings stale after five minutes. A JSON consumer may use
that threshold too; failed refreshes warrant “Last known” even sooner. Missing
or future `observed` values mean unknown age or clock skew. A stopped helper can
leave a valid snapshot whose provider and USB states are historical.

The helper polls each provider about four minutes after a successful read.
Connection failures retry after thirty seconds; other failures wait at least
fifteen minutes, with longer delays for HTTP 429/503 `Retry-After`. Clock changes
and wakeups request fresh data while respecting cooldowns. Each provider read
has a 45-second budget; HTTP requests time out after 20 seconds. The two providers
need not share an observation time.

For a continuously visible UI, read cached status and update countdowns locally.
Repeated `usage --json` calls make new requests without the helper's backoff.

## Local GUI tasks

`pipkin tasks --json` returns one object with `schema_version: 1`, `observed_at`
(Unix seconds), and a `providers` object keyed by `claude` and `codex`. Each
provider has only `coverage` and `tasks`. Task records contain only:

| Field | Type / presence | Meaning |
| --- | --- | --- |
| `id` | String, always | Stable opaque provider-scoped task identity. Does not contain a title, path, prompt, PID or raw task ID. |
| `updated_at` | Unix seconds or null, always | Native source update time where known. May reflect activity rather than the exact status transition; never use it as a heartbeat or failure timeout. |
| `status` | String, always | `working`, `needs_attention`, `blocked`, `idle` or `unknown`. |

`coverage: "partial"` means these are observed tasks, not an exhaustive list.
Observed counts can omit tasks. Missing statuses do not establish zero counts.
`coverage: "unavailable"` means no usable
observations were collected, including an absent/closed GUI, denied access,
unsupported local storage/protocol, malformed state or an expired collection
budget. It does not distinguish those causes or establish an absent task.
`coverage: "unsupported"` applies to Linux task detection. Both unavailable and
unsupported providers emit `tasks: null`; they never emit an authoritative empty
list or synthetic zero counts.

Claude covers verified live local Code engines belonging to Claude Desktop.
Its native latest-turn `blocked`/`need_input` summaries, after applying a current
turn wrap-up's request for user input or clearing, mean human attention and map
to `needs_attention`, even when the engine is idle. They are model-derived
summaries, not proof of terminal failure. Ordinary Claude Chat and Cowork are
outside this source's verified scope. A stopped engine is not a blocked task.

Codex combines already-announced GUI streams with bounded existing-owner queries
for recent persisted root candidates. Catalog history alone is not live state.
Some background and ephemeral chats can be undiscovered, and unsupported or
ambiguous state remains unknown. Structured unanswered native question cards
can mean `needs_attention` even when runtime is idle and the turn is completed.
Questions can remain pending across later turns. Accepted replies clear that
signal; gaps in loaded history leave unresolved older questions unknown.
Skip and dismissal state is held only in the GUI renderer and is not available
through this source, so an unanswered dismissed card can still produce an
attention signal. Unread completion alone does not imply attention, and a plan
implementation offer is nonblocking.
Known approval/input waits also mean `needs_attention`. Only an explicit fresh
Codex runtime `systemError` means `blocked`; old failed turns, recoverable tool
errors, blocked goals, cancellation, missing streams and silence do not.
Terminal failure delivery has not been validated with a natural live error.

The helper scans local tasks about every 30 seconds while awake, independently
of USB and allowance polling. It caches the same object under `status.tasks`.
Wake and clock-generation changes invalidate cached task lists and discard
earlier in-flight results before publishing a fresh collection.
`status --json` invalidates task lists when the helper is stopped, the observation
is over 37 seconds old, or its time is in the future: coverage becomes unavailable
and tasks become null, with the original `observed_at` retained. Old helper
snapshots may omit tasks entirely. A direct task check does not update the cache
or the physical display. Existing installation and USB protocol are unchanged.

Example with invented data:

```json
{
  "schema_version": 1,
  "observed_at": 1800000000,
  "providers": {
    "claude": {"coverage": "unavailable", "tasks": null},
    "codex": {
      "coverage": "partial",
      "tasks": [
        {"id": "codex:opaque-example", "updated_at": null, "status": "needs_attention"}
      ]
    }
  }
}
```

Unknown future statuses should be treated as unknown. Task status and allowance
availability are separate; task collection never makes provider HTTPS requests.

## Reading fields

| Field | Type / presence | Meaning |
| --- | --- | --- |
| `source` | String; omitted when empty | Credential source, for diagnostics. |
| `plan` | String; omitted when empty | Provider's plan label; no fixed enumeration. |
| `account` | String, always | Opaque installation-scoped identity, or `codex`/`claude` when unknown. |
| `observed` | Unix seconds; omitted when zero | Local time when the subscription response was parsed. |
| `session`, `weekly` | Window or null, always | Session and weekly allowance. Null means no known window. |
| `session_no_cap` | Boolean; emitted only when true | Confirmed weekly-only Codex plan; `session` is null. |
| `models` | Name → window; omitted when empty | Additional model/scoped limits, which can overlap main limits. |
| `extra_usage` | Object; omitted when unavailable | Claude's provider-reported extra usage. |
| `credits` | Object; omitted when unavailable | Codex's provider-reported credits. |
| `reports_banked` | Boolean; emitted only when true | The reading supports reporting reset grants. |
| `banked` | Nonnegative integer; omitted when unknown | Available reset grants; zero is a known balance. |

Optional objects are omitted when unavailable; readers should also tolerate null.
Missing or null readings may occur in external or older snapshots. No missing
metric should be converted to zero.

### Source and account

Codex sources are `codex_auth_file` and `codex_keyring`. Claude sources are
`claude-desktop`, `claude-keychain` and `claude-auth-file`.

Account values hash an installation salt and provider identity; Claude Desktop
also includes the organization. Treat them as opaque. Reinstalling with a new
configuration or rotating a fallback token can change the value. Placeholders
cannot distinguish accounts. When identity changes, clear history for the old
account. Do not merge accounts across installations or use these values as names.

### Allowance windows

| Field | Type / presence | Meaning |
| --- | --- | --- |
| `used` | Integer, always | Tenths of a percent used, rounded and clamped to `0..1000`. |
| `reset` | Unix seconds; omitted when unknown | Absolute reset time, within `1577836800..4102444800`. |
| `seconds` | Positive integer; omitted when unknown | Window duration, not a countdown. Codex accepts up to 366 days. |
| `not_started` | Boolean; emitted only when true | Confirmed idle session. Takes precedence over `used`. |

Calculate the number and gauge from the same remaining value:

```text
remaining_percent = (1000 - used) / 10
remaining_ratio   = (1000 - used) / 1000
```

`used: 235` means 76.5% remaining; `0` means 100%; `1000` means 0%.
For `not_started: true`, show “Starts with your first message” without a numeric
gauge or countdown. Claude reports this only for exactly zero utilization and an
explicit null reset. Zero usage with a valid reset remains an active window.

Null session with `session_no_cap: true` means “No session cap”; other null
windows mean “Unknown.” There is no weekly no-cap flag. Use supplied durations
instead of deriving entitlements from plan names. Claude normalizes sessions to
18,000 seconds and weekly/model windows to 604,800 seconds; Codex classifies its
windows by returned duration.

Countdowns use `max(0, reset - nowUnixSeconds)`. Convert seconds to milliseconds
only where a UI API requires it. Missing reset means no countdown; passed reset
means “Awaiting refresh.” JSON does not expose a window ID.

Model keys are dynamic, such as `sonnet`, `opus` or `codex_spark_weekly`. Each
value follows the window contract. Show them separately; do not add percentages
or subtract model usage from the main limit.

### Claude extra usage

| Field | Type / presence | Meaning |
| --- | --- | --- |
| `enabled` | Boolean, always | Provider reports extra usage enabled; defaults to false when omitted upstream. |
| `used` | Number or null, always | Amount used in the named currency. |
| `limit` | Positive number or null, always | Reported cap. |
| `remaining` | Number or null, always | `max(0, limit - used)` when both are known. |
| `currency` | String; omitted when empty | Currently `USD`; amounts are already converted from cents to dollars. |
| `reset` | Unix seconds; omitted when unknown | Extra-usage reset, independent of allowance windows. |

A source cap of zero means uncapped and becomes null, as do missing/invalid
caps. JSON cannot distinguish these cases: show “No reported cap,” not a zero
cap or confirmed unlimited usage. Disabled extra usage should read “Off.”

### Codex credits and banked resets

Credits contain `has_credits` (boolean or null), `unlimited` (boolean) and
`balance` (nonnegative number or null), all always emitted. Balance is in provider
credit units, with no USD conversion. Unlimited takes precedence; otherwise show
the known balance, including zero. A reported absence of credits with no balance
normalizes to zero. Available credits with a null balance mean “Balance unknown.”

Banked resets are separate from credits and monetary usage. `reports_banked: true` without `banked` means an unknown balance; an absent flag does not prove
the plan has no resets. Codex sets the flag on successful reads. Claude sets it
when eligibility is reported, excludes expired grants and caps the sum at
1,000,000. The USB writer caps either provider's count at 1,000,000. Neither
credits nor reset grants have purchase or redemption controls in the CLI.

## Examples

Fresh usage with known optional metrics and a missing Claude weekly reset:

```json
{
  "schema_version": 1,
  "version": "1.0.0",
  "pid": 0,
  "device": null,
  "states": {"codex": "available", "claude": "available"},
  "readings": {
    "codex": {
      "source": "codex_auth_file",
      "plan": "pro",
      "account": "c1111111111111111",
      "observed": 1790683202,
      "session": {"used": 235, "reset": 1790686800, "seconds": 18000},
      "weekly": {"used": 412, "reset": 1791115200, "seconds": 604800},
      "models": {"codex_spark_weekly": {"used": 200, "seconds": 604800}},
      "credits": {"has_credits": true, "unlimited": false, "balance": 120.5},
      "reports_banked": true,
      "banked": 2
    },
    "claude": {
      "source": "claude-desktop",
      "plan": "max",
      "account": "a2222222222222222",
      "observed": 1790683203,
      "session": {"used": 133, "reset": 1790694000, "seconds": 18000},
      "weekly": {"used": 500, "seconds": 604800},
      "extra_usage": {"enabled": true, "used": 12.5, "limit": 50, "remaining": 37.5, "currency": "USD"},
      "reports_banked": true,
      "banked": 0
    }
  },
  "updated": 1790683200
}
```

Cached status after a Codex refresh error and Claude sign-out. Codex's observation
time remains unchanged; Claude's old quota is cleared. If the helper stops,
`status --json` returns the same cached fields with `pid: 0`.

```json
{
  "schema_version": 1,
  "version": "1.0.0",
  "pid": 4321,
  "device": {"port": "COM9", "firmware": "1.0.0"},
  "states": {"codex": "unavailable", "claude": "signed_out"},
  "readings": {
    "codex": {
      "source": "codex_auth_file",
      "plan": "pro",
      "account": "c1111111111111111",
      "observed": 1790683202,
      "session": {"used": 235, "reset": 1790686800, "seconds": 18000},
      "weekly": {"used": 412, "reset": 1791115200, "seconds": 604800},
      "models": {"codex_spark_weekly": {"used": 200, "seconds": 604800}},
      "credits": {"has_credits": true, "unlimited": false, "balance": 120.5},
      "reports_banked": true,
      "banked": 2
    },
    "claude": {"account": "claude", "session": null, "weekly": null}
  },
  "codex_error": "Can't reach Codex. Check your internet connection.",
  "claude_error": "Sign-in expired. Open Claude to sign in again.",
  "updated": 1790684400
}
```

Startup can emit empty `readings`, `{}` for `device` and no `states`. Show a
waiting state. Fresh failures emit provider placeholders with null session and
weekly values, corresponding states/errors and no `observed`.

## Display integrations

Show remaining allowance, reset countdown and age for each provider. Keep zero,
unknown, no cap, not started and last known distinct. Use text/icons alongside
colour and give screen readers the remaining percentage. Keep helper, USB and
provider status separate. Escape provider-controlled labels and errors.
For expired sign-ins, direct users to the provider app; on macOS, blocked
Keychain permission can be resolved explicitly with `pipkin authorize`.

JSON is not a USB packet. The helper sends session/weekly windows, account,
observation time and banked resets using the [v1 display protocol](protocol.md).
Plan, source, models, credits, extra usage and JSON errors stay on the computer.
Unknown serial keys are rejected by the firmware, so adding display metrics
requires a coordinated protocol/firmware update. See the
[shared packet fixture](../internal/pipkin/testdata/helper_packets.txt).
