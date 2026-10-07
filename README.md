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

Pipkin runs in the background and starts automatically at sign-in. Connect your
display with a USB data cable. On Windows, open a new terminal after installing;
on macOS and Linux, add `~/.local/bin` to your PATH if needed.

The display sleeps and wakes with your computer and reconnects automatically.
On macOS and Windows, it also follows screen sleep.

On macOS, choose **Always Allow** if Keychain asks for access to Claude's saved
sign-in. See the [sign-in guide](docs/data-sources.md#existing-sign-ins) if you need
help with permissions.

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

To set up a self-built display or update your Pipkin, connect it by USB and run:

```sh
pipkin flash
```

Pipkin downloads the latest stable firmware and asks you to confirm before
installing it. No extra software setup is needed.

See the [firmware guide](docs/firmware.md) for supported boards, advanced options
and troubleshooting.

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
