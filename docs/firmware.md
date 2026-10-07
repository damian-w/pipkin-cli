# Install and update display firmware

`pipkin flash` installs the latest stable firmware from
[damian-w/pipkin](https://github.com/damian-w/pipkin/releases). Self-built boards and
Pipkin kits use the same command. The latest firmware requires CLI **1.4.0 or
later**; `pipkin update` updates the CLI, while `pipkin flash` updates the display.
Earlier firmware releases supported CLI 1.1.0 or later.

## Supported board

The current firmware uses one profile for **ESP32 CYD 2.8-inch touch boards with
4 MB flash**. Equivalently wired ESP32-S and WROOM boards use the same profile;
their printed model labels do not need to match exactly. First installation,
settings-preserving updates, USB reset and display/touch operation have been tested
on a physical CYD board on macOS.
Check the PCB model and components against the
[board profile](https://github.com/damian-w/pipkin/blob/main/docs/development.md#board-profile).
The firmware's current wiring and display/touch drivers determine compatibility,
rather than a seller's product name.

A USB serial adapter or an ESP32 chip does not prove which screen is attached. The
CLI checks the ESP chip and flash compatibility, but cannot identify the LCD or
touch controller. Confirm that it is the intended ESP32 CYD 2.8-inch touch board
before flashing. If running firmware does not respond, the CLI checks the stored
application descriptor for a Pipkin version. A recognized version is labelled
`stored; not responding`; it does not prove that firmware is running. Otherwise,
the board is shown as not identified as Pipkin.

## Flash a release

Use a USB data cable, connect one supported board, and open a terminal:

```sh
pipkin flash
```

The release must include `firmware.json`, `SHA256SUMS` and the flash images. Older
releases without these artifacts cannot be installed by this command. New firmware
releases stay drafts until hardware qualification; drafts and prereleases are not
selected, and the CLI does not build unpublished source as a fallback.

The CLI obtains the stable release's flash manifest, downloads the required images
and verifies their checksums. It checks the serial connection, current firmware
when available, and the board's ESP chip and flash. These checks may briefly restart
the display. It then shows the board, current firmware, target version and whether
the action is an installation or update, and asks for yes/no confirmation.

Only `y` or `yes` confirms. Enter, no, end of input or cancellation does not write
firmware. Confirmation requires an interactive terminal; there is no `--yes`
option. All compatibility checks still apply after confirmation.

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

Replace `VERSION` with an existing firmware release version, not the CLI version.
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
application slot and no automatic rollback. After an interrupted flash, reconnect
the board and rerun `pipkin flash`; add `--reinstall` if needed. If automatic
bootloader entry fails, follow the command's bootloader guidance, then try again.

A verification error means the CLI could not confirm the expected running firmware.
Do not treat it as a completed update. Check the cable and port, try again, and run
`pipkin status` once the helper is running. Unsupported hardware, an incompatible
release, a failed checksum or a board that cannot be safely flashed stops the
command before writing; reinstalling does not bypass those checks.

## Flashing tool and platform support

The CLI automatically downloads and privately caches Espressif's official
**esptool v5.4.0** executable and verifies the download. You do not need to install
Python or ESP-IDF. The tool is used only when you run `pipkin flash`; it does not run
as part of the background helper. GitHub requests contain no usage readings or
provider credentials, and Pipkin has no telemetry.

See [flashing-tool provenance and licences](esptool.md) for the pinned downloads,
cache checks and the tool's third-party notices.

The CLI supports macOS, Linux and Windows on AMD64 and ARM64. Flashing on Windows
ARM64 uses the official AMD64 tool through Windows 11's x64 emulation, and still
needs hardware qualification. The qualified board catalog records the completed
physical checks; USB flashing still requires qualification on the other platforms.

Building and flashing your own firmware with ESP-IDF remains available in the
[source-build guide](https://github.com/damian-w/pipkin/blob/main/docs/development.md#build-firmware-from-source).
