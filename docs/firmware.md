# Install and update display firmware

`pipkin flash` installs the latest stable firmware from
[damian-w/pipkin](https://github.com/damian-w/pipkin/releases). Self-built boards and
Pipkin kits use the same command. `pipkin update` updates the CLI, while
`pipkin flash` updates the display.

The CLI and firmware share a major version, which is the protocol they speak:
Pipkin 2.x installs and talks to display firmware 2.x. If the latest firmware has
a newer major version, `pipkin flash` asks you to run `pipkin update` first.

## Supported boards

The firmware supports two boards:

| Board | Screen | Chip and flash |
| --- | --- | --- |
| ESP32 CYD (ESP32-2432S028R) | 2.8-inch, 320 × 240, resistive touch | ESP32, 4 MB |
| LCDWIKI ES3C40P | 4-inch IPS, 480 × 320, capacitive touch | ESP32-S3, 16 MB |

The CYD includes equivalently wired ESP32-S and WROOM versions; the printed model
label doesn't need to match. What matters is the board's wiring and its display
and touch parts, not the seller's product name. Compare yours with the
[board profiles](https://github.com/damian-w/pipkin/blob/main/docs/development.md#board-profiles).
The CYD has been tested on a physical board on macOS: first installation, updates
that keep settings, USB reset, and the screen and touch. The ES3C40P has been
installed and updated on macOS. Flashing hasn't been tested on Linux or Windows
yet. The 4-inch board needs Pipkin 2.1 or later.

A display running Pipkin reports its board. Otherwise Pipkin tells the boards
apart by their chip, ESP32 or ESP32-S3, and checks the flash size, but it can't
see the screen itself, so make sure the board is one of these before flashing. If
the display's firmware doesn't respond, Pipkin looks for a Pipkin version stored
on the board and shows it as "installed but not responding", or "too old for this
version of Pipkin" for firmware from before 2.0. Either way it's offered as an
update that keeps your settings. Otherwise the board is treated as a new install.

## Flash a release

Use a USB data cable, connect your display, and open a terminal:

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
current firmware and settings will be replaced, so check that it really is that
board before confirming.

Only `y` or `yes` confirms. Anything else, end of input or Ctrl-C writes nothing.
Confirmation requires an interactive terminal; there is no `--yes` option. All
compatibility checks still apply after confirmation.

The CLI pauses a running helper while using the USB port and restores it afterward,
including when you decline or a step fails. A helper that was already stopped stays
stopped. Flashing verifies the written images, reboots the display and checks that
the expected Pipkin firmware responds before reporting success.

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

### Several displays

With more than one display connected, `pipkin flash` asks each one which board it
is and which Pipkin version it runs, then lists them and asks which to flash:

```text
  1. 2.8-inch CYD, Pipkin 2.0.1 (/dev/cu.usbserial-2010)
  2. 4-inch ES3C40P, Pipkin 2.0.1 (/dev/cu.usbmodem202201)
Which display should Pipkin flash? [1-2, Enter cancels]
```

A board without Pipkin is listed by its port, and its chip is checked once you
choose it. Flash one display at a time; run the command again for the next. To
skip the question, name the display's serial port:

```sh
pipkin flash --port PORT
```

Replace `PORT` with its serial port, such as `COM9`, `/dev/ttyUSB0` or
`/dev/cu.usbserial-110`. Selecting a port does not bypass board checks.

The helper drives one display at a time. With two connected, it uses the one it
last used and the other waits for your computer.

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
printed, or rerun `pipkin flash` and add `--reinstall` if needed. If the display
doesn't enter its bootloader on its own, hold the BOOT button on the back of the
display, run `pipkin flash` again and keep holding BOOT until "Writing firmware..."
appears. This is more likely on Windows, where the flashing tool can switch only
one USB control line at a time.

A verification error means the CLI could not confirm the expected running firmware.
Do not treat it as a completed update. Check the cable and port, try again, and run
`pipkin status` once the helper is running. Unsupported hardware, an incompatible
release, a failed checksum or a board that cannot be safely flashed stops the
command before writing; reinstalling does not bypass those checks.

## Flashing tool and platform support

Pipkin's flashing tool runs only when you use `pipkin flash`, never in the
background. GitHub requests carry no usage readings or sign-ins, and Pipkin has no
telemetry. See [flashing-tool provenance and licences](esptool.md) for the pinned
downloads, supported platforms (Windows ARM uses Windows 11's emulation), cache
checks and the tool's third-party notices.

Building and flashing your own firmware with ESP-IDF remains available in the
[source-build guide](https://github.com/damian-w/pipkin/blob/main/docs/development.md#build-firmware-from-source).
