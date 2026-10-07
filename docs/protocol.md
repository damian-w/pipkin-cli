# Display protocol, version 1

Pipkin receives allowance, clock and host status over serial at 115200 baud,
8N1. The CLI reads the providers and sends these records. Credentials, prompts, token totals and monetary
balances stay off the wire; the protocol has no reset-redemption commands.

Each record contains ASCII `key=value` pairs separated by one space, terminated
by LF or CRLF. State-changing keys may appear in any order; the identity request
uses the exact line shown below. The payload limit is 768 bytes,
excluding the line ending, with at most 32 pairs. Unknown or duplicate keys,
empty values, extra spaces, invalid numbers, and malformed records are rejected
atomically. Stream receivers must discard an oversized record through its line
ending; they must not parse its tail as a new record. Integers are decimal,
without a leading plus sign, fractional part, or exponent.

Every state-changing record requires `v=1` and `kind`. A positive unsigned 64-bit
`seq` is optional. When present, sequence numbers must increase across all
providers and message kinds for the current device boot; duplicates and lower
sequence numbers are rejected. Records without `seq` do not change the stored
sequence, so stateless senders can write without first querying the device.
Usage ordering never depends on `seq`: each window group rejects observations
older than its stored watermark, so replayed readings cannot regress. Invalid
records do not advance the sequence or change any state. Monotonic receipt
time must not move backward.

Senders that use `seq` must query device identity on reconnect and resume above
the reported sequence, and must serialize their records. Device reboot clears
volatile state and sequence; sequence checks are ordering safeguards within a
boot, not sender authentication or replay protection across boots.

## Identity

The hardware transport handles this read-only discovery request without
resetting the device or requiring a sequence:

```text
v=1 kind=identify
```

Its response includes `v=1 kind=identity product=pipkin`, firmware version,
board identity, the last accepted `seq`, current `clock_epoch`, and the device's
current `unix` time, or `null` before clock synchronization. A lower `seq` than
a sender last used indicates the device restarted and lost volatile state.
Identity replies do not authorize flashing; this protocol has no firmware-update
or rollback commands.

## Clock

```text
v=1 kind=clock seq=1 unix=1800000000 tz=480
```

`unix` is the host's current UTC epoch second, between 1577836800 and 4102444800
inclusive. `tz` is its current local UTC offset in minutes, from -840 through
840. The sender must send a new offset after timezone or daylight-saving
changes. Without synchronization, the display has no wall-clock time.

The device advances the clock using its monotonic clock. Corrections greater
than 300 seconds require explicit `rebase=1`; smaller corrections and offset
changes ordinarily do not. A source clock rollback that would reverse its
observation timestamps also requires a rebase. Clock records do not imply host
liveness. Changing the clock never reduces an existing reading's age. Reset
countdowns use the current synchronized wall clock; a passed reset timestamp means awaiting
source confirmation, never an automatic allowance refill.

An explicit rebase requires `seq` and starts a new observation epoch identified
by that clock record's `seq`. Subsequent usage records must include `clock_epoch`
with this value. The initial epoch is zero and can be omitted. An ordinary clock update
preserves the epoch. Old-epoch usage is rejected; existing readings retain
their independent monotonic ages. Within the new epoch, freshly observed source
timestamps can be earlier than those from the previous epoch. The sender must
re-observe the source, never retag cached readings as a new epoch.

## Host and application status

```text
v=1 kind=host seq=2 state=awake
v=1 kind=app seq=3 provider=codex state=available
```

Host states are `awake`, `asleep`, and `disconnected`. Only an explicit,
reliable host power report may request the sleep overlay. An `awake` report
counts as a genuine liveness observation for 60 seconds; expiration means
liveness is unconfirmed and does not request sleep or declare a disconnect.
Usage messages never refresh that timer. An accepted packet establishes
transport activity; `host state=disconnected` marks it disconnected. Receiving
another message after a disconnect leaves host power state unknown until a
host report arrives.

Screen power is independent from that liveness diagnostic. The display goes dark
after 90 seconds without host reports, or immediately after an explicit disconnect.
Before the first host report, accepted transport activity supplies the timeout;
with no activity, the timeout starts at firmware boot. Silence does not assign an
`asleep` or `disconnected` host state. An explicit `awake` transition, or recovery
after the 90-second timeout, gives a one-minute brightness grace while fresh usage
loads. Regular awake heartbeats do not extend that grace. Touch can temporarily
wake a screen darkened by missing traffic or stale readings; explicit `asleep`
ignores touch until an `awake` report arrives.

The CLI derives these reports from system sleep and shutdown, plus screen sleep
on macOS and Windows. Each sleep reason must clear before reporting `awake`.
Connection/resync and 30-second heartbeats report the current state. OS shutdown
cleanup preserves `asleep`; ordinary helper termination reports `disconnected`.
Before allowing system sleep/shutdown, the helper makes a bounded attempt to
confirm its sleep report using `identify` and the returned accepted sequence.
A matching sequence and clock epoch are required; other identity responses do not
acknowledge the report. Unavailable USB must never hold up host sleep indefinitely;
the heartbeat timeout covers missing reports.

Application states are `available`, `unavailable`, `unsupported`, and
`signed_out`. Availability is separate from host liveness and usage freshness.
Closing or losing an application may retain its last known readings.
`signed_out` clears that provider's account and all readings immediately.

## Usage snapshots and patches

```text
v=1 kind=usage seq=4 provider=codex mode=full account=sample-a observed=1800000000 session=metered session_id=period-a session_used=640 session_reset=1800003600 session_seconds=18000 weekly=metered weekly_id=week-a weekly_used=220 weekly_reset=1800604800 weekly_seconds=604800 banked=0
```

`provider` is `codex` or `claude`. `account` is a non-identifying opaque
reference containing 1–32 ASCII letters, digits, `.`, `_`, or `-`; `null` is
reserved. Do not transmit an email address, username, account credential, or
provider account identifier. The sender must change this reference when the
relevant account or workspace changes.

`observed` is when the source actually observed the included information, in
UTC epoch seconds with the same bounds as `unix`. Reading cached data again
must preserve its original observation time. When synchronized, an observation
more than 30 seconds ahead of device time is rejected. Before synchronization,
readings can be displayed, but their age is unknown and they are not fresh.
Use `observed=null` when the source does not provide an authoritative
observation time. Such readings remain of unknown age regardless of receipt
time; receiving a packet or fetching a cached value is not source observation.
Known timestamp and age bounds are retained internally across unknown
observations so a later replay cannot evade ordering or become younger.

`mode=full` replaces all usage information for that provider. Omitted window
groups and banked resets become unknown at this observation time.
`mode=patch` preserves omitted groups, including their original observation and
receipt times. Patches require an existing matching account; account changes
require a full snapshot and discard the prior account's readings. App status
is preserved for the same account and becomes unknown on an account change.

Each touched group records its own observation and receipt times. A patch to
weekly usage does not refresh session usage or banked resets. Regressing a
group's known observation timestamp in the same clock epoch is rejected,
including explicit `null` allowance values and full
snapshot omissions. Re-delivering the same observation cannot decrease its
age, even after a backward wall-clock adjustment. A reading becomes stale
after five minutes, independently of its underlying allowance state.

### Allowance groups

`session` and `weekly` accept these states:

| Value | Meaning | Additional fields |
| --- | --- | --- |
| `metered` | Authoritative percentage used | `_id`, `_used` required; `_reset`, `_seconds` optional |
| `no_cap` | Confirmed absence of a session cap | Session only; no other group fields |
| `not_started` | Source confirms the window has not started | `_id` required; `_reset`, `_seconds` optional |
| `unsupported` | Source explicitly cannot provide this information | No other group fields |
| `null` | Unknown or unavailable information | No other group fields |

Suffixes attach to the group name: for example `session_used` and
`weekly_reset`. `_used` is an integer percentage in tenths from 0 through 1000:
`640` means **64.0% used** on the wire. The display deliberately converts this
at the presentation boundary to **36.0% remaining**, using
`(1000 - _used) / 10` percent for both its number and gauge. Senders must not
pre-invert `_used`. Unknown, unsupported, no-cap, and not-started states must
not be converted into a numeric remaining allowance. `_id` identifies the source's
allowance window and uses the same bounded alphabet as `account`. `_reset`
is an epoch second within the clock bounds; `_seconds` is an authoritative
duration from 1 through 31622400 seconds. Optional values may be `null`.
Window labels must use supplied duration rather than infer a plan entitlement;
the display names the usual 5-hour session and 7-day week simply "Session" and
"Weekly", and spells out any other supplied duration.

Before Claude's first message, a confirmed idle session is sent as
`session=not_started session_id=current session_seconds=18000`, without
`session_used` or `session_reset`. Pipkin keeps the session visible and its detail
page explains that it starts on the first message. The next active reading
replaces it with `metered` usage and the source's reset time. A missing reset alone
does not confirm that a session has not started.

Updating any member replaces the whole window group and requires its state,
window ID where applicable, and percentage for metered usage. Omitted optional
members of a touched group become unknown, preventing old reset times or
durations from carrying into a new window. A state must accompany its group
members. `session=null` differs from omitting `session` in a patch:

```text
v=1 kind=usage seq=5 provider=codex mode=patch account=sample-a observed=1800000010 banked=null
v=1 kind=usage seq=6 provider=codex mode=patch account=sample-a observed=1800000020 session=null
```

The first clears only the reset balance. The second also clears session data;
weekly information remains unchanged in both.

### Banked resets and sign-out

`banked` is an authoritative integer count from 0 through 1000000 or `null`.
Zero is a known zero balance; null is unknown. Do not infer this count from
optional detail rows or purchased credits. Version 1 has no reset actions.

An explicit full snapshot with `account=null` clears every reading and marks
the application unavailable. No observation timestamp or usage fields are
allowed with it:

```text
v=1 kind=usage seq=7 provider=codex mode=full account=null
```

## Display behaviour

Pages are Overview (0), Codex (1), Claude Code (2) and status/About (3). A provider
is visible when it has metered usage, a confirmed no-cap or not-started window,
or a positive banked-reset count. Stale readings remain visible after an app
becomes unavailable. Unknown/unsupported-only data and a zero reset balance do
not make a provider visible.

Overview is always available. Swipe left/right or tap the right/left half of the
bottom strip to cycle through visible pages, wrapping at either end. Overview
provider cards open their detail pages. Losing a provider's readings returns its
page to Overview. A saved page preference may wait for initial data.

Horizontal swipes need 60 pixels of movement, at least twice the vertical
movement; taps and holds allow 12 pixels. Hold the header clock for 1.2 seconds
to open status. The whole header row accepts the hold because the clock position
varies by page. Releasing the opening hold does not close status; a later tap,
swipe or one minute without touch returns to the selected normal page.

Only changes to the selected normal page are persisted. Page entry and touch
times use the monotonic clock. Gauges animate for 650 ms after page changes or
reading changes; animation does not alter stored usage. Sleep temporarily
covers the selected page without changing it.

Status shows firmware version, Git revision, host connection and reading ages.
Firmware metadata is supplied locally and cannot be set by usage packets. Missing
metadata remains unknown. Source observation age and packet receipt age are
separate; an unknown observation age must not be described as fresh because a
packet just arrived. Percentages, resets, clock anchors and countdowns are volatile.
