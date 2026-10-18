# Data sources and privacy

Pipkin reads subscription usage from the Codex and Claude accounts already
signed in on your computer. It does not require their CLIs or a separate login.
Provider compatibility can change when the desktop apps or services change.
Unavailable data is shown as unknown, never as a full allowance.
Claude sessions with zero utilization and an explicitly null reset have not
started yet; they start with the first message and have no reset countdown.

## Existing sign-ins

On Linux and Windows, Claude prefers the account selected in Claude Desktop.
Pipkin reads Desktop's saved access token and account/organization selection,
including its selection cookie when available, and decrypts encrypted data in
memory. An existing Claude Code sign-in can be used when Desktop has no saved
sign-in. Codex uses its existing
local subscription sign-in from its configured auth file or OS credential store.
API keys are not used for subscription readings.

On macOS, Pipkin first tries an accessible Claude Code sign-in to avoid a separate
Desktop Keychain permission request. When Desktop is signed in, Claude must verify
that Code's login belongs to the same account. Readings then use Code's organization.
If Code's login is unavailable or cannot be verified, Pipkin uses Desktop as before.

On Windows, a running Claude Desktop can exclusively lock its cookie database.
When its active-organization cookie is missing or inaccessible, Pipkin can infer
an organization only if its saved Desktop token caches contain exactly one
organization for the selected account. Expired entries and deleted-token markers
still count when checking for ambiguity. Pipkin verifies the token's account and
organization with Claude before reporting usage and rechecks saved Desktop state.
Multiple cached organizations remain unavailable without a readable selection
cookie. This fallback verifies the credential's identity; it cannot detect a GUI
organization switch that Claude has not yet saved in its token caches.

On macOS, installation may ask for permission to read the `Claude Safe Storage`
Keychain item, used to decrypt Claude Desktop's saved data. CLI updates started
by version 1.2.0 or later also check this permission from the newly installed
executable before restarting the background helper.
The dialog can ask for your Mac login password. Choose **Always Allow** for
background access. If access is blocked or you cancel, run `pipkin authorize`;
it restarts Pipkin afterwards. Updates started by an older CLI may also need
`pipkin restart`.
Routine background reads do not open permission dialogs. Linux credential-store
access requires an already unlocked Secret Service store; basic storage and auth
files do not. Windows uses the current user's credential storage.

The provider apps manage their own sign-ins. If a session expires, open the
provider's app to renew it. Pipkin does not use refresh tokens or refresh, replace,
save or delete provider credentials.

## Data handling

Subscription requests go directly to the relevant provider over HTTPS.
Credentials are read in memory, sent only to their provider, and never included
in Pipkin's files, terminal output, or display packets. Requests do not forward
credentials through redirects. The CLI sends no analytics, activity pings or crash
reports, and there is no Pipkin server receiving credentials or usage data.

Installation and `pipkin update` download CLI releases from GitHub. `pipkin flash`
checks and downloads firmware releases from GitHub, and downloads the pinned
Espressif flashing tool there when it is not already cached. These requests do not
include provider credentials or usage readings. See the [firmware guide](firmware.md).
`pipkin update` also checks GitHub for newer display firmware, using the
version your display last reported. It never probes or flashes the display.

Saved readings and output identify accounts with an
installation-specific hashed value, not a name, email address, provider account ID
or credential. Error messages omit provider response bodies and secrets.

Pipkin reads usage and the account information needed to verify it. It does not
submit model requests, redeem resets, purchase credits, or change subscription
settings.

## Tasks

On macOS and Windows, Pipkin reads task status from the Claude and Codex
desktop apps: Claude Code's local session list and status files, and the Codex
app's local connection. Codex sends whole conversations over that connection.
Pipkin keeps only each task's ID, update time and status, and discards the rest
in memory. Prompts, titles and conversation text are never saved, shown or sent,
and Pipkin doesn't scan transcript files.

Task detection doesn't use your sign-ins, contact the providers, install
anything, or start, change or answer chats. It only trusts apps running as you.
Claude Chat, Cowork, remote work (cloud, SSH or WSL) and Linux aren't covered.
The apps' private formats can change, so some tasks may be missed. See
[tasks](output.md#tasks) for the JSON format.

Over USB, Pipkin sends the display only each app's coverage, when its tasks were
checked, and how many are working, waiting for you or blocked. Task IDs, update
times and idle or unknown tasks stay on your computer.

## Local files and controls

Pipkin keeps settings, helper/display status, its latest usage snapshot and a
diagnostic log in `~/Library/Application Support/Pipkin` on macOS,
`~/.local/share/pipkin` on Linux, or `%LOCALAPPDATA%\Pipkin` on Windows. `PIPKIN_HOME`
can override this location; Linux also respects `XDG_DATA_HOME`.

Snapshots can include plan, allowance and model limits, reading and reset times,
banked resets, the hashed account reference and other provider-reported usage
details, plus task statuses refreshed about every 30 seconds. Raw provider
responses are not saved. Logs can include diagnostic errors, local file paths and
display connection details. Each snapshot replaces the previous one, and the log
rotates as it grows. Pipkin does not upload or cloud-sync
these files; they remain until removed.

If `config.json` is damaged, `pipkin install` saves a private
`config.json.backup-*` copy and starts fresh settings. Other commands leave it
untouched and ask you to run `pipkin install`. On macOS and Linux, the installer
prints any PATH command for you to paste; it never edits shell startup files.

`pipkin stop` pauses automatic checks until you next log in to this computer or
run `pipkin start`.
`pipkin uninstall` removes the helper, automatic startup and Pipkin's folder,
leaving provider sign-ins intact. To avoid deleting the wrong thing, it refuses
a folder that doesn't look like Pipkin's, contains other files or links, or is
your home or current folder.

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
