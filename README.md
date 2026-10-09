<h1 align="center">Pipkin CLI</h1>

<p align="center"><strong>Your agentic AI allowance, at a glance.</strong></p>

<p align="center">
  <img src="docs/images/badge-works-with-codex.svg" alt="Works with Codex" height="28">
  <img src="docs/images/badge-works-with-claude-code.svg" alt="Works with Claude Code" height="28">
</p>

Pipkin is a small USB display for your desk. It shows how much of your Claude and
Codex allowance you have left, and when it resets, so you don't have to dig
through settings in the middle of your work.

This repository holds the helper that runs on your computer and keeps the display
up to date. If you're signed in to Claude or Codex in their desktop apps, you're
already set up. Pipkin uses those existing sign-ins, so there's nothing new to log
into and no API key to find.

## What you need

- A Pipkin display, either a kit or one you've built from a CYD board.
- A USB **data** cable. Some cables only carry power and won't work.
- A Mac, Windows PC or Linux computer.
- The Claude desktop app, the Codex app, or both, signed in.

## Install

You'll paste one line into a terminal window. After that, Pipkin looks after
itself.

**Mac:** open **Terminal** (press ⌘ Space, type *Terminal*, then press Return),
paste this line and press Return:

```sh
curl -fsSL https://pipkin.io/install.sh | sh
```

**Windows:** open **PowerShell** from the Start menu, paste this line and press
Enter:

```powershell
irm https://pipkin.io/install.ps1 | iex
```

**Linux:** run the Mac command in your terminal.

Then plug in your display. Your numbers appear shortly, and Pipkin starts by
itself each time you sign in to your computer.

On a Mac, you may see a Keychain prompt asking about Claude's saved sign-in.
Choose **Always Allow** so Pipkin can keep reading it in the background. If you
clicked something else, the [sign-in guide](docs/data-sources.md#existing-sign-ins)
explains how to fix it.

## What the display shows

For each app, Pipkin shows how much of your current session and your weekly
allowance is left, and when each one resets.

The display goes to sleep with your computer and wakes up with it. On Mac and
Windows, it also switches off when your screens do. If you unplug it, it
reconnects on its own when you plug it back in.

If a sign-in expires, open that app and sign in again. Pipkin picks it up
automatically.

## Handy commands

You won't need these day to day, but they're there if something looks off. Type
them into a terminal window. On Windows, open a new PowerShell window after
installing so it can find `pipkin`.

| Command | What it does |
| --- | --- |
| `pipkin status` | Checks that the helper is running and the display is connected |
| `pipkin usage` | Shows your current usage in the terminal |
| `pipkin restart` | Restarts the helper |
| `pipkin update` | Updates Pipkin to the latest version |
| `pipkin flash` | Installs or updates the display's firmware |
| `pipkin uninstall` | Removes Pipkin from your computer |

On a Mac or Linux, if the terminal says `pipkin: command not found`, use
`~/.local/bin/pipkin` instead, or add `~/.local/bin` to your PATH.

## Updating your display

The display's firmware gets improvements too. With the display plugged in, run:

```sh
pipkin flash
```

Pipkin downloads the latest firmware, shows you what it's about to do, and only
goes ahead once you say yes. Updates keep your display's settings. The same
command sets up a display you've built yourself. The [firmware guide](docs/firmware.md)
covers supported boards and troubleshooting.

## Privacy

Your sign-ins stay on your computer. Pipkin sends them only to Anthropic or
OpenAI, to read your usage. It never reads your conversations, there's no
telemetry or Pipkin account, and no Pipkin server ever sees your usage. Apart
from that, it only contacts GitHub to download updates, firmware and the tool that
installs firmware. The [data sources guide](docs/data-sources.md) has the details.

## For developers

Everything Pipkin reads is also available as JSON for your own scripts and
dashboards: run `pipkin usage --json`, or see the [JSON output reference](docs/output.md).
The display talks a simple text [protocol](docs/protocol.md) over USB serial.

To build from source, install the Go version listed in [go.mod](go.mod), then run:

```sh
go test ./...
go build -o pipkin .
./pipkin install
```

On Windows, use `go build -o pipkin.exe .` and `.\pipkin.exe install`. The code lives
in [internal/pipkin](internal/pipkin), with [main.go](main.go) as the entry point.

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
