# Data sources and privacy

Pipkin reads subscription usage from the Codex and Claude accounts already
signed in on your computer. It does not require their CLIs or a separate login.
Provider compatibility can change when the desktop apps or services change.
Unavailable data is shown as unknown, never as a full allowance. A Claude session
that hasn't started yet shows as starting with your first message, with no
countdown.

## Existing sign-ins

For Claude, Pipkin uses the account selected in the Claude desktop app. It reads
the app's saved sign-in and decrypts it in memory. If the desktop app isn't signed
in, Pipkin can use an existing Claude Code sign-in instead. On a Mac, Pipkin tries
the Claude Code sign-in first to avoid an extra Keychain prompt, but only when
Claude confirms it belongs to the same account as the desktop app.

For Codex, Pipkin uses the Codex app's saved subscription sign-in, from its auth
file or your system's credential store. API keys aren't used.

On Windows, a running Claude app can lock the file that records which
organization you've selected. Pipkin then uses the organization in Claude's saved
sign-in, but only when your account has exactly one, and checks it with Claude
before showing usage. If you belong to several organizations, usage is
unavailable until the file can be read. If you've just switched organizations,
Pipkin may show the previous one until Claude saves the change.

On a Mac, installing Pipkin may ask for permission to read the
`Claude Safe Storage` Keychain item, which unlocks Claude's saved data. Enter your
Mac login password and choose **Always Allow** so Pipkin can read it in the
background. If you blocked it or cancelled, run `pipkin authorize`. Updates check
this permission again before restarting Pipkin. Background reads never open
permission prompts. On Linux, Pipkin can read the credential store only when it's
already unlocked. Windows uses your user account's credential storage.

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
desktop apps: Claude Code's local session list and status files, read every 2
seconds, and the Codex app's local connection, which the helper keeps open while
the computer is awake so changes arrive as they happen. Codex sends whole
conversations and their updates over that connection. Pipkin keeps only each
task's ID, update time and status, and discards the rest in memory. Prompts, titles and conversation text are never saved, shown or sent,
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
details, plus task statuses, saved when they change and at least every 15 seconds. Raw provider
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
provider states, freshness and errors, and [what the display receives](output.md#display-integrations).

## Compatibility

Pipkin supports macOS, Linux and Windows on AMD64 and ARM64. Automated tests run
on all three operating systems.

Locked or unsupported credential stores show as unavailable. On Linux, Pipkin
supports an unlocked Secret Service store or Electron's basic storage, but not
KWallet on its own. Codex's experimental encrypted storage and Electron's
app-bound encryption aren't supported.

Pipkin's Mac builds aren't signed with an Apple Developer ID yet, so the Keychain
may ask for permission again after an update.
