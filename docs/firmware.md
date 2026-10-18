# Install and update display firmware

`pipkin flash` installs the latest stable firmware from
[damian-w/pipkin](https://github.com/damian-w/pipkin/releases). Self-built boards and
Pipkin kits use the same command. `pipkin update` updates the CLI, while
`pipkin flash` updates the display.

The CLI and firmware share a major version, which is the protocol they speak:
Pipkin 2.x installs and talks to display firmware 2.x. If the latest firmware has
a newer major version, `pipkin flash` asks you to run `pipkin update` first.

## Supported board

The current firmware supports **ESP32 CYD 2.8-inch touch boards with 4 MB
flash**, including equivalently wired ESP32-S and WROOM versions; the printed
model label doesn't need to match. It has been tested on a physical CYD board on
macOS: first installation, updates that keep settings, USB reset, and the screen
and touch. What matters is the board's wiring and its display and touch parts,
not the seller's product name. Compare yours with the
[board profile](https://github.com/damian-w/pipkin/blob/main/docs/development.md#board-profile).

Pipkin can check the ESP32 chip and its flash, but not which screen is attached,
so make sure it's a CYD 2.8-inch touch board before flashing. If the display's
firmware doesn't respond, Pipkin looks for a Pipkin version stored on the board
and shows it as "installed but not responding", or "too old for this version of
Pipkin" for firmware from before 2.0. Either way it's offered as an update that
keeps your settings. Otherwise the board is treated as a new install.

## Flash a release

Use a USB data cable, connect one supported board, and open a terminal:

```sh
pipkin flash
```

Pipkin installs only published stable firmware releases with its own major
version. New releases stay drafts until they've been tested on hardware.

The CLI obtains the stable release's flash manifest, downloads the required images
and verifies their checksums. It checks the serial connection, current firmware
when available, and the board's ESP chip and flash. These checks may briefly restart
the display. It then shows the board, current firmware, target version and whether
the action (install, update, reinstall or downgrade), and asks for yes/no confirmation.
A first installation also names the board and port and warns that the board's
current firmware and settings will be replaced, so check that it really is a CYD
board before confirming.

Only `y` or `yes` confirms. Anything else, end of input or Ctrl-C writes nothing.
Confirmation requires an interactive terminal; there is no `--yes` option. All
compatibility checks still apply after confirmation.

The CLI pauses a running helper while using the USB port and restores it afterward,
including when you decline or a step fails. A helper that was already stopped stays
stopped. Flashing verifies the written images, reboots the display and checks that
the expected Pipkin firmware responds before reporting success.

If several candidate ports are connected, specify the board's port:

```sh
pipkin flash --port PORT
```

Replace `PORT` with its serial port, such as `COM9`, `/dev/ttyUSB0` or
`/dev/cu.usbserial-110`. Selecting a port does not bypass board checks.

To choose a specific stable firmware release:

```sh
pipkin flash --version VERSION
```

Replace `VERSION` with an existing 2.x firmware release version.
The plan still shows any downgrade and asks for confirmation. Drafts and
prereleases are not used.
If the same firmware version is already running, a normal flash does not rewrite
it. If that version is recognized in storage but does not respond, the CLI asks
you to use `--reinstall` to repair it. For a deliberate reinstall or USB recovery:

```sh
pipkin flash --reinstall
```

`--reinstall` allows writing the same version again; it does not skip confirmation
or compatibility checks. It can be combined with `--port` and `--version`.

## Settings and recovery

Ordinary compatible Pipkin updates retain the selected display page and saved
settings. This also applies to recovery when the CLI recognizes stored Pipkin
firmware but cannot get a running response. Existing Pipkin with a different
partition layout is rejected before writing; the CLI cannot safely migrate it.

Installing onto a board without recognized Pipkin firmware replaces its firmware
and clears its saved application settings. The plan states this before confirmation.
Flashing does not erase the entire flash chip.

Keep the USB cable connected until the command finishes. The firmware has one
application slot and no automatic rollback. Ctrl-C before writing starts cancels
safely. Once writing starts, Pipkin finishes the write, verification and restart
before exiting. Unplugging, closing the terminal or losing power can still
interrupt it.

After an interrupted flash, reconnect the board and run the command Pipkin
printed, or rerun `pipkin flash` and add `--reinstall` if needed. If automatic
bootloader entry fails, follow the command's bootloader guidance, then try again.

A verification error means the CLI could not confirm the expected running firmware.
Do not treat it as a completed update. Check the cable and port, try again, and run
`pipkin status` once the helper is running. Unsupported hardware, an incompatible
release, a failed checksum or a board that cannot be safely flashed stops the
command before writing; reinstalling does not bypass those checks.

## Flashing tool and platform support

Pipkin downloads Espressif's official **esptool v5.4.0**, checks the download and
keeps a private copy, so you don't need to install Python or ESP-IDF. The tool
runs only when you use `pipkin flash`, never in the background. GitHub requests
carry no usage readings or sign-ins, and Pipkin has no telemetry.

See [flashing-tool provenance and licences](esptool.md) for the pinned downloads,
cache checks and the tool's third-party notices.

Pipkin runs on macOS, Linux and Windows, on both Intel/AMD and ARM. On Windows ARM,
flashing runs the official Intel/AMD tool through Windows 11's emulation. Flashing
has been tested with a physical board on macOS; the other systems haven't been
yet.

Building and flashing your own firmware with ESP-IDF remains available in the
[source-build guide](https://github.com/damian-w/pipkin/blob/main/docs/development.md#build-firmware-from-source).
