# Flashing tool

`pipkin flash` downloads [Espressif's official esptool v5.4.0 release](https://github.com/espressif/esptool/releases/tag/v5.4.0)
when it is first needed. Python and ESP-IDF are not required. The Go CLI remains a
single executable; esptool runs separately from its private `tools` directory in
Pipkin's application directory.

The archive is verified against a pinned SHA-256 digest before use. Each run
checks the cached archive again and compares the extracted executable and
upstream notices against that archive. Downloads, subprocesses and archive
extraction have time and size limits. The CLI uses a minimal esptool configuration
and explicit chip, reset and flash options so local esptool settings do not alter
the planned operation.

On Windows, the helper starts suspended, joins a private process job, and then
runs. Canceling terminates that job; closing the job provides a second cleanup
path for the helper and its children. If Windows cannot establish that process
containment, Pipkin stops before the helper can access the board.

The cache retains the upstream `LICENSE` and `README.md` next to esptool.
esptool is [GPL-2.0-or-later](https://github.com/espressif/esptool/blob/v5.4.0/LICENSE),
and its matching [source and build instructions](https://github.com/espressif/esptool/tree/v5.4.0)
remain available from Espressif. Pipkin does not embed esptool code or executable
assets in its CLI releases.

## Platforms

| Pipkin CLI | Official esptool archive |
| --- | --- |
| macOS Intel | `esptool-v5.4.0-macos-amd64.tar.gz` |
| macOS Apple silicon | `esptool-v5.4.0-macos-arm64.tar.gz` |
| Linux x86-64 | `esptool-v5.4.0-linux-amd64.tar.gz` |
| Linux ARM64 | `esptool-v5.4.0-linux-aarch64.tar.gz` |
| Windows x86-64 | `esptool-v5.4.0-windows-amd64.zip` |
| Windows ARM64 | The same Windows x86-64 archive, using Windows 11's x64 emulation |

Espressif does not publish a Windows ARM64 executable. The Windows ARM64 path
requires Windows 11's x64 emulation and still needs testing with a physical CYD.
CI checks that the official helper starts and reports its version on macOS,
Linux and Windows; these checks never connect to a board. See Espressif's
[binary installation limitations](https://docs.espressif.com/projects/esptool/en/latest/esp32/installation.html#binary-releases)
for system-library and antivirus considerations.

## Pinned archive digests

These are the asset digests returned by GitHub's public API for the official
v5.4.0 release. Updating the helper is a reviewed source change, independent of
choosing a newer Pipkin firmware release.

```text
910bb64fe39a84c792752701293c8aa294faeef229fe8705ecd6955b01db3778  esptool-v5.4.0-macos-amd64.tar.gz
ba332671130939e2e6db90c2784488f7e62a1459b0fe3c5ec66e9a366821de7a  esptool-v5.4.0-macos-arm64.tar.gz
61648fbae20735cabb342f2fbe8fc89b3046e1ed6f9c3e09528d837dc9a9b152  esptool-v5.4.0-linux-amd64.tar.gz
2964fff085071c1403f2cf812a7a1d425f987f9992851a60236bbee17b6e7dcc  esptool-v5.4.0-linux-aarch64.tar.gz
b7f6b9dd301a210b31f4829118c909c84aae23107f9ca1fdc14ccf4d7384be2e  esptool-v5.4.0-windows-amd64.zip
```
