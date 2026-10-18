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
background access. If access is blocked or you cancel, run `pipkin authorize` to
retry; a standalone authorization restarts a running Pipkin afterward. During
installation and updates, authorization happens before the replacement starts
and does not perform a nested restart. Updates started by an older CLI may need
`pipkin restart` as well.
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
Foreground `pipkin update` can also check for a newer stable firmware release
using the saved display version. This advisory has a short timeout, is optional
when the network is unavailable, and does not probe USB or flash the display.

Pipkin does not scan transcript logs or keep or upload conversation text.
On macOS and Windows, local task detection reads Claude Code's GUI session
registry and matching Desktop status metadata, and subscribes to the existing
Codex GUI's local socket or named pipe. Codex snapshots can include conversation
content; it is processed transiently in memory and discarded after reducing it
to task ID, native update time and status. Claude metadata containers can also
contain text; only status fields are retained. Raw snapshots, prompts, titles,
tool arguments and error bodies are never saved or printed.

Task collection does not read provider sign-ins, invoke provider CLIs, install
hooks, start/resume/interrupt chats, send approvals, change provider settings or
contact provider servers. It uses the apps' existing local state with no extra
installation step. Current-user process/connection identity checks and bounded
reads reject unverifiable sources. Private GUI formats can change, so coverage
is always partial; unsupported or missing data means unknown counts.
Ordinary Claude Chat, Cowork, cloud/SSH/WSL work and Linux task detection are not
covered. See [local GUI tasks](output.md#local-gui-tasks).

Saved readings and output identify accounts with an
installation-specific hashed value, not a name, email address, provider account ID
or credential. Error messages omit provider response bodies and secrets.

Pipkin reads usage and the account information needed to verify it. It does not
submit model requests, redeem resets, purchase credits, or change subscription
settings.

## Local files and controls

Pipkin keeps settings, helper/display status, its latest usage snapshot and a
diagnostic log in `~/Library/Application Support/Pipkin` on macOS,
`~/.local/share/pipkin` on Linux, or `%LOCALAPPDATA%\Pipkin` on Windows. `PIPKIN_HOME`
can override this location; Linux also respects `XDG_DATA_HOME`.

Snapshots can include plan, allowance and model limits, reading and reset times,
banked resets, the hashed account reference and other provider-reported usage
details. The helper snapshot also includes reduced local task observations about
every 30 seconds while awake; a direct `pipkin tasks --json` check writes no cache.
Raw provider responses are not saved. Logs can include diagnostic errors,
local file paths and display connection details. Each snapshot replaces the
previous one, and the log rotates as it grows. Pipkin does not upload or cloud-sync
these files; they remain until removed.

Only an explicit `pipkin install` repairs malformed `config.json` settings. It
first saves the original bytes in a private, uniquely named `config.json.backup-*`
file, then initializes fresh settings. Valid settings and unknown fields are
preserved. Normal usage reads and updates leave malformed settings untouched;
unreadable files are reported rather than replaced. Backups remain in Pipkin's
folder until you remove them or uninstall.

On macOS and Linux, installation prints PATH commands for you to paste when
needed; it does not edit shell startup files automatically. Windows keeps its
existing per-user PATH setup. Provider sign-ins are not changed by installation.

`pipkin stop` pauses automatic checks until you next log in to this computer or
run `pipkin start`.
`pipkin uninstall` removes the helper, automatic startup and Pipkin's folder,
leaving provider sign-ins intact. It first verifies that its target is a dedicated
installation with the expected settings and installed app. Relative overrides,
filesystem roots, home/current folders and their parents, directory links and
unrelated files are refused before stopping Pipkin. Run uninstall from outside
the installation folder; move unrelated files elsewhere first. The target is
checked again before removal, and delayed cleanup cannot delete a fresh install.

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
