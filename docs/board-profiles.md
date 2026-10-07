# Identify a board and contribute a profile

This maintainer command is available in Pipkin CLI 1.4.0 or later:

```sh
pipkin identify
pipkin identify --json
pipkin identify --issue
```

Connect one board, or add `--port PORT`. Inspection pauses a running helper,
queries responding Pipkin firmware, and reads the ESP32 chip, flash ID and security
state through the ROM loader. It briefly restarts the board and resumes the helper.
It does not write firmware, settings or eFuses. A helper that was stopped stays
stopped. The USB bridge details are collected from the selected port when available.

The default output includes the electronics and compatible catalog candidates.
`--json` produces a complete local report, including its port, MAC address and
available USB serial number. `--issue` creates a Markdown issue draft, omitting
those unit identifiers and local paths. Complete the physical-board checklist and
submit it through the linked board support issue form. No issue is created by
the command.

## Confirm a catalog variant locally

Chip type, flash size, USB IDs and running firmware do not identify a physical
CYD revision. Check the board's printed markings, connectors, screen and touch
compatibility before selecting a candidate:

```sh
pipkin identify --board bruce-cyd-2432s028-dual-usb
pipkin identify --board esp32-2432s028r-dual-usb
```

Use the one that matches the connected physical board. This records your explicit
confirmation in the installation's private `boards.json`, keyed by its ROM MAC
and electronics fingerprint. Later `pipkin identify` and `pipkin flash` recognise
that unit. The association follows the board when its port changes. Changed
electronics require another inspection and explicit confirmation.

`pipkin flash --board PROFILE` can select a known variant for that flash. It saves
the association only after flash confirmation and the second device check. When
firmware is already current, use `pipkin identify --board PROFILE` to remember the
board without rewriting firmware.

## Update the catalog

The firmware repository owns
[boards/profiles.json](https://github.com/damian-w/pipkin/blob/main/boards/profiles.json)
and the
[qualification guide](https://github.com/damian-w/pipkin/blob/main/docs/board-profiles.md).
This repository embeds a byte-for-byte copy at
`internal/pipkin/board_profiles.json`; refresh it with each catalog change and run
the CLI tests. Several physical variants can share a firmware profile if their
display, touch wiring and flash layout match. Keep unit MACs and serial numbers
out of the public catalog.

The canonical firmware profile `esp32-2432s028r` accepts
`esp32-2432s028r-provisional` as a compatibility alias. Firmware 1.3.0 uses the
canonical identity and requires CLI 1.4.0. The identity migration retains the same
partition layout and application settings. Unknown profiles or incompatible
layout/settings are rejected before flashing.
