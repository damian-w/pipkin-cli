# Data sources and privacy

Pipkin reads subscription usage from the Codex and Claude accounts already
signed in on your computer. It does not require their CLIs or a separate login.
Provider compatibility can change when the desktop apps or services change.
Unavailable data is shown as unknown, never as a full allowance.
Claude sessions with zero utilization and an explicitly null reset have not
started yet; they start with the first message and have no reset countdown.

## Existing sign-ins

Claude prefers the account selected in Claude Desktop. An existing Claude Code
sign-in can be used when no Desktop sign-in is available. Codex uses its existing
local subscription sign-in and configured credential store. API keys are not used
for subscription readings.

On macOS, installation may ask for Keychain permission to read Claude's saved
sign-in. CLI updates started by version 1.2.0 or later also check this permission
from the newly installed executable before restarting the background helper.
Choose **Always Allow** for background access. If access is blocked or you cancel,
run `pipkin authorize`, then `pipkin restart` to retry. Updates started by an older
CLI may need this manual recovery.
Routine background reads do not open permission dialogs. Linux credential-store
access requires an already unlocked Secret Service store; basic storage and auth
files do not. Windows uses the current user's credential storage.

The provider apps manage their own sign-ins. If a session expires, open the
provider's app to renew it. Pipkin does not refresh or replace their credentials.

## Data handling

Subscription requests go directly to the relevant provider over HTTPS.
Credentials are read in memory, sent only to their provider, and never included
in Pipkin's files, terminal output, or display packets. Requests do not forward
credentials through redirects. There is no telemetry or Pipkin server.

Installation and `pipkin update` download CLI releases from GitHub. `pipkin flash`
checks and downloads firmware releases from GitHub, and downloads the pinned
Espressif flashing tool there when it is not already cached. These requests do not
include provider credentials or usage readings. See the [firmware guide](firmware.md).

Pipkin does not scan conversation logs or estimate token costs. Output identifies
accounts with an installation-specific opaque value, not an email address or
credential. Error messages omit provider response bodies and secrets.

Pipkin only reads usage. It does not submit model requests, redeem resets,
purchase credits, or change subscription settings.

## Output and display support

The [output reference](output.md) covers JSON fields, units, missing values,
provider states, freshness and errors.

Current USB firmware receives session, weekly, reset-time, and banked-reset
information. Additional metrics are available in JSON and require a compatible
firmware update to appear on the physical display. See the [display protocol](protocol.md).

## Compatibility

Pipkin supports macOS, Linux and Windows on AMD64 and ARM64. Automated tests run
on all three operating systems.

Locked or unsupported credential stores are reported as unavailable. Linux
supports an unlocked Secret Service store or Electron basic storage; KWallet-only
storage is unsupported. Codex's experimental encrypted secret backend and
app-bound Electron encryption are also unsupported.

Current macOS builds are unsigned or ad-hoc signed. Keychain may ask for a new
grant after an update; persistent permission across releases requires a stable
Developer ID signature.
