<h1 align="center">Pipkin CLI</h1>

<p align="center"><strong>Your agentic AI allowance, at a glance.</strong></p>

<p align="center">
  <img src="docs/images/badge-works-with-codex.svg" alt="Works with Codex" height="28">
  <img src="docs/images/badge-works-with-claude-code.svg" alt="Works with Claude Code" height="28">
</p>

Show your remaining Codex and Claude subscription usage on a Pipkin USB display.
Pipkin uses your existing desktop app sign-ins. No provider CLI, new login, or
other type of authentication is needed.

## Install

Supports macOS, Linux, and Windows on ARM64 and Intel/AMD 64-bit.

macOS / Linux:

```sh
curl -fsSL https://pipkin.io/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://pipkin.io/install.ps1 | iex
```

Installation starts Pipkin in the background and at sign-in. On Unix, add
`~/.local/bin` to your PATH if needed; on Windows, open a new terminal. Linux may
need serial-port group access.

The display follows computer sleep, resume and shutdown. macOS and Windows also
follow screen sleep, including macOS's **Put Display to Sleep** Hot Corner. Linux
requires systemd-logind for host power notifications. On macOS and Windows, Pipkin
remains dark during background wakes while the screens are asleep. It reconnects
automatically on resume or at sign-in. Closing a laptop lid while the system and an
external display remain awake keeps Pipkin awake. Automatic startup before sign-in
is not provided.

With matching display firmware, a missed system-sleep/shutdown report or stopped
helper is covered by a 90-second heartbeat timeout. Waking lights the display
immediately while fresh usage loads.

On macOS, installation and CLI updates may ask for access to Claude's saved
sign-in. The newly installed helper requests permission before it starts in the
background. Choose **Always Allow** if Keychain asks. If access is blocked or you
cancel, run `pipkin authorize`, then `pipkin restart` to retry. Updates started by
CLI 1.1.0 or earlier may need this manual recovery.

## Use

```sh
pipkin usage         # Read usage now
pipkin usage --json  # All metrics for integrations
pipkin status        # Check the helper and USB display
pipkin restart       # Restart the background helper
pipkin update        # Update the CLI
pipkin flash         # Install or update display firmware
pipkin uninstall     # Remove Pipkin
```

Claude readings include session, weekly, model-specific limits, extra usage, and
reset times. Codex includes session, weekly, credits, and reset times. Both include
banked resets when reported.

All metrics are available in JSON. Current display firmware shows session, weekly,
and reset information. See the [JSON output reference](docs/output.md) for the full
metric set and the separate [display protocol](docs/protocol.md).

Credentials stay local except for direct requests to their provider. No telemetry.
If a sign-in expires, open the provider's app. See
[data sources and limitations](docs/data-sources.md).

## Display firmware

For a self-built board, or to update a Pipkin kit, connect it by USB and run:

```sh
pipkin flash
```

Pipkin checks the connected board and its current firmware, shows the latest stable
firmware release from [damian-w/pipkin](https://github.com/damian-w/pipkin/releases),
and asks for yes/no confirmation before writing. Pressing Enter means no. It
distinguishes running firmware from a stored Pipkin version that is not responding.
Confirm that it is the intended ESP32 CYD 2.8-inch touch board.

The current release targets **ESP32 CYD 2.8-inch touch boards with 4 MB flash**
matching the firmware profile. Equivalent ESP32-S and WROOM boards use the same
profile; their printed model labels do not need to match exactly. See the
[board guide](https://github.com/damian-w/pipkin/blob/main/docs/development.md#board-profile).

No ESP-IDF or Python setup is needed: the CLI downloads and caches the pinned
Espressif flashing tool. Firmware and tool downloads come from GitHub. `pipkin flash`
requires CLI **1.1.0 or later**; run `pipkin update` if you have the initial release.
Kit setup still just needs the CLI and USB cable.

See the [firmware guide](docs/firmware.md) for port selection, specific releases and
USB recovery, or [build the firmware from source](https://github.com/damian-w/pipkin/blob/main/docs/development.md).

## Build from source

With the Go version listed in [go.mod](go.mod):

```sh
go test ./...
go build -o pipkin .
./pipkin install
```

On Windows, use `go build -o pipkin.exe .` and `.\pipkin.exe install`.

Source layout: [main.go](main.go) is the entry point; [internal/pipkin](internal/pipkin)
contains the Go implementation and tests; [docs](docs) covers the protocol and JSON
output; [scripts](scripts) supports releases.

<p>
  <img src="docs/images/badge-licence.svg" alt="Licence: noncommercial" height="28">
  <img src="docs/images/badge-runs-on.svg" alt="Runs on macOS, Linux and Windows" height="28">
</p>

## Licence

Pipkin's firmware, CLI, installers and documentation are source-available under the
[PolyForm Noncommercial License 1.0.0](LICENSE). You may use, build, modify and share
them for noncommercial purposes, including personal use, study and hobby projects.
Include the licence and its `Required Notice` line when sharing. Selling Pipkin or
products built from it needs separate permission.

If you own a Pipkin, whether a kit or one you've built yourself, you may also use this
software with it for any purpose, including paid work. See the
[terms of use](https://pipkin.io/terms).

Third-party components keep their own licences; see the
[firmware notices](https://github.com/damian-w/pipkin/blob/main/assets/NOTICE.md) and
[CLI notices](https://github.com/damian-w/pipkin-cli/blob/main/NOTICE.md).

Codex is a trademark of OpenAI. Claude and Claude Code are trademarks of Anthropic.
Pipkin is an independent product and is not affiliated with or endorsed by either
company.
