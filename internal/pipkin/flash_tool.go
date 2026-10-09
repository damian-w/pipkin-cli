package pipkin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	flashToolVersion           = "5.4.0"
	maxToolArchiveBytes  int64 = 96 << 20
	maxToolFileBytes     int64 = 128 << 20
	maxToolExpandedBytes int64 = 400 << 20
	maxToolOutputBytes         = 2 << 20
	baselineFlashBytes   int64 = 4 << 20
)

var errFlashProcessCleanup = errors.New("could not stop all flashing-tool processes")

type flashToolAsset struct{ Platform, Archive, SHA256 string }

func (a flashToolAsset) binary() string {
	if strings.HasPrefix(a.Platform, "windows-") {
		return "esptool.exe"
	}
	return "esptool"
}

// Archive digests published by GitHub's v5.4.0 release-assets API. The helper
// is pinned separately from firmware and downloaded directly from Espressif.
var flashToolAssets = map[string]flashToolAsset{
	"darwin/amd64":  {"macos-amd64", "esptool-v5.4.0-macos-amd64.tar.gz", "910bb64fe39a84c792752701293c8aa294faeef229fe8705ecd6955b01db3778"},
	"darwin/arm64":  {"macos-arm64", "esptool-v5.4.0-macos-arm64.tar.gz", "ba332671130939e2e6db90c2784488f7e62a1459b0fe3c5ec66e9a366821de7a"},
	"linux/amd64":   {"linux-amd64", "esptool-v5.4.0-linux-amd64.tar.gz", "61648fbae20735cabb342f2fbe8fc89b3046e1ed6f9c3e09528d837dc9a9b152"},
	"linux/arm64":   {"linux-aarch64", "esptool-v5.4.0-linux-aarch64.tar.gz", "2964fff085071c1403f2cf812a7a1d425f987f9992851a60236bbee17b6e7dcc"},
	"windows/amd64": {"windows-amd64", "esptool-v5.4.0-windows-amd64.zip", "b7f6b9dd301a210b31f4829118c909c84aae23107f9ca1fdc14ccf4d7384be2e"},
	// Espressif does not publish Windows ARM64 binaries. Windows 11 on ARM
	// runs this official amd64 helper using the operating system's emulation.
	"windows/arm64": {"windows-amd64", "esptool-v5.4.0-windows-amd64.zip", "b7f6b9dd301a210b31f4829118c909c84aae23107f9ca1fdc14ccf4d7384be2e"},
}

type flashTool struct {
	path   string
	output io.Writer
}
type flashProbe struct {
	Chip              string
	ChipDescription   string
	Features          string
	CrystalMHz        int
	FlashBytes        int64
	FlashManufacturer string
	FlashDevice       string
	Secure            bool
	SecureBoot        bool
	FlashEncrypted    bool
	MAC               string
}
type flashWriteImage struct {
	Path   string
	Offset int64
}

func prepareFlashTool(ctx context.Context, output io.Writer) (*flashTool, error) {
	asset, ok := flashToolAssets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("firmware flashing is unavailable on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	dir, err := resolvedAppDir()
	if err != nil {
		return nil, err
	}
	cache := filepath.Join(dir, "tools")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(cache); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("flashing-tool cache must be a real private directory")
	}
	if err := os.Chmod(cache, 0o700); err != nil {
		return nil, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	lock, err := waitFileLock(lockCtx, filepath.Join(cache, "esptool.lock"))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	return prepareFlashToolAsset(lockCtx, output, cache, asset, http.DefaultClient, "https://github.com/espressif/esptool/releases/download/v"+flashToolVersion+"/"+asset.Archive)
}

func prepareFlashToolAsset(ctx context.Context, output io.Writer, cache string, asset flashToolAsset, client *http.Client, url string) (*flashTool, error) {
	archive := filepath.Join(cache, asset.Archive)
	if err := verifyToolArchive(archive, asset.SHA256); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cached esptool archive failed verification; remove %s and retry: %w", archive, err)
		}
		fmt.Fprintf(output, "Downloading flashing tool (esptool %s)…\n", flashToolVersion)
		if err := downloadToolArchive(ctx, client, url, archive, asset.SHA256); err != nil {
			return nil, err
		}
	}
	// Extract from the verified archive to a private staging directory on every
	// invocation, then compare the cached files against it. This also repairs a
	// changed executable without trusting a mutable sidecar checksum.
	stage, err := os.MkdirTemp(cache, ".esptool-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := extractFlashTool(ctx, archive, stage, asset); err != nil {
		return nil, fmt.Errorf("could not unpack esptool: %w", err)
	}
	destination := filepath.Join(cache, "esptool-"+flashToolVersion+"-"+asset.Platform)
	if !sameToolFiles(stage, destination, asset) {
		if err := os.RemoveAll(destination); err != nil {
			return nil, err
		}
		if err := os.Rename(stage, destination); err != nil {
			return nil, err
		}
	}
	tool := &flashTool{path: filepath.Join(destination, asset.binary()), output: output}
	data, err := tool.run(ctx, 30*time.Second, "version")
	if err != nil {
		return nil, fmt.Errorf("could not start the flashing tool: %w", err)
	}
	if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(flashToolVersion) + `\s*$`).Match(data) {
		return nil, errors.New("flashing tool returned an unexpected version")
	}
	return tool, nil
}

func verifyToolArchive(filename, expected string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxToolArchiveBytes {
		return errors.New("invalid flashing-tool archive")
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxToolArchiveBytes+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("SHA-256 checksum mismatch")
	}
	return nil
}

func downloadToolArchive(parent context.Context, client *http.Client, url, destination, expected string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "pipkin-cli-flash")
	file, err := os.CreateTemp(filepath.Dir(destination), ".esptool-download-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	hash := sha256.New()
	if err := errors.Join(download(client, request, io.MultiWriter(file, hash), maxToolArchiveBytes), file.Close()); err != nil {
		return fmt.Errorf("could not download esptool: %w", err)
	}
	// The pinned digest also rejects an empty download.
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("esptool download failed SHA-256 verification")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), destination)
}

func wantedToolFiles(asset flashToolAsset) map[string]bool {
	return map[string]bool{asset.binary(): true, "LICENSE": true, "README.md": true}
}

func safeToolEntry(name string, asset flashToolAsset) (string, error) {
	// Both archive formats use slash paths; backslashes are rejected even on Unix.
	if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return "", errors.New("unsafe archive path")
	}
	clean := path.Clean(name)
	root := "esptool-" + asset.Platform
	if clean == root {
		if strings.TrimSuffix(name, "/") != root {
			return "", errors.New("unsafe archive path")
		}
		return "", nil
	}
	if !strings.HasPrefix(clean, root+"/") || clean != strings.TrimSuffix(name, "/") {
		return "", errors.New("unsafe archive path")
	}
	return strings.TrimPrefix(clean, root+"/"), nil
}

func extractFlashTool(ctx context.Context, archive, dir string, asset flashToolAsset) error {
	wanted, seen := wantedToolFiles(asset), map[string]bool{}
	var expanded int64
	visit := func(name string, regular, directory bool, size int64, content io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := safeToolEntry(name, asset)
		if err != nil {
			return err
		}
		if !regular && !directory {
			return errors.New("links and special files are not allowed in the esptool archive")
		}
		if directory {
			return nil
		}
		if relative == "" || seen[relative] {
			return errors.New("duplicate or invalid esptool archive entry")
		}
		seen[relative] = true
		if size < 0 || size > maxToolFileBytes || expanded > maxToolExpandedBytes-size {
			return errors.New("esptool archive exceeds the unpacked size limit")
		}
		expanded += size
		if !wanted[relative] {
			_, err := io.Copy(io.Discard, io.LimitReader(content, size+1))
			return err
		}
		mode := os.FileMode(0o600)
		if relative == asset.binary() {
			mode = 0o700
		}
		file, err := os.OpenFile(filepath.Join(dir, relative), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(file, io.LimitReader(content, size+1))
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != size {
			return errors.New("truncated or oversized esptool archive entry")
		}
		return nil
	}
	if strings.HasSuffix(asset.Archive, ".zip") {
		reader, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer reader.Close()
		if len(reader.File) > 32 {
			return errors.New("too many esptool archive entries")
		}
		for _, entry := range reader.File {
			content, err := entry.Open()
			if err != nil {
				return err
			}
			err = visit(entry.Name, entry.Mode().IsRegular(), entry.FileInfo().IsDir(), int64(entry.UncompressedSize64), content)
			closeErr := content.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	} else {
		file, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer file.Close()
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer compressed.Close()
		reader := tar.NewReader(io.LimitReader(compressed, maxToolExpandedBytes+(1<<20)))
		for count := 0; ; count++ {
			entry, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if count >= 32 {
				return errors.New("too many esptool archive entries")
			}
			if err := visit(entry.Name, entry.Typeflag == tar.TypeReg || entry.Typeflag == tar.TypeRegA, entry.Typeflag == tar.TypeDir, entry.Size, reader); err != nil {
				return err
			}
		}
		// Drain to verify the gzip checksum, including the end of the stream.
		n, err := io.Copy(io.Discard, io.LimitReader(compressed, (1<<20)+1))
		if err != nil {
			return err
		}
		if n > 1<<20 {
			return errors.New("excess trailing data in esptool archive")
		}
	}
	for name := range wanted {
		if !seen[name] {
			return fmt.Errorf("esptool archive is missing %s", name)
		}
	}
	return nil
}

func sameToolFiles(source, destination string, asset flashToolAsset) bool {
	info, err := os.Lstat(destination)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	for name := range wantedToolFiles(asset) {
		cached := filepath.Join(destination, name)
		info, err := os.Lstat(cached)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxToolFileBytes {
			return false
		}
		original, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			return false
		}
		current, err := os.ReadFile(cached)
		if err != nil || !bytes.Equal(original, current) {
			return false
		}
		// Windows does not expose Unix execute permissions in FileMode.
		if runtime.GOOS != "windows" && name == "esptool" && info.Mode().Perm()&0o100 == 0 {
			return false
		}
	}
	return true
}

func (t *flashTool) run(parent context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// esptool falls back to host config files when ESPTOOL_CFGFILE is missing
	// or lacks its section. Supply a real minimal section to prevent that fallback.
	config, err := os.CreateTemp(filepath.Dir(t.path), ".pipkin-esptool-*.cfg")
	if err != nil {
		return nil, err
	}
	defer os.Remove(config.Name())
	_, writeErr := config.WriteString("[esptool]\nopen_port_attempts = 1\n")
	closeErr := config.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	command := exec.CommandContext(ctx, t.path, args...)
	// Local esptool configuration and inherited ESPTOOL_* flags must not change
	// the fixed flash plan (including reset, stub, chip and finite retries).
	command.Dir = filepath.Dir(t.path)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ESPTOOL_") || upper == "NO_COLOR" || upper == "PYTHONPATH" || upper == "PYTHONHOME" {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env, "NO_COLOR=1", "ESPTOOL_CFGFILE="+config.Name())
	output := cappedBuffer{limit: maxToolOutputBytes}
	command.Stdout, command.Stderr = &output, &output
	command.WaitDelay = 2 * time.Second
	err = runFlashProcess(command)
	data := bytes.Clone(output.Bytes())
	if ctx.Err() != nil {
		if errors.Is(err, errFlashProcessCleanup) {
			return data, errors.Join(fmt.Errorf("flashing tool interrupted: %w", ctx.Err()), err)
		}
		return data, fmt.Errorf("flashing tool interrupted: %w", ctx.Err())
	}
	if output.truncated {
		return data, errors.New("flashing tool exceeded its output limit")
	}
	if err != nil {
		detail := strings.TrimSpace(string(data))
		if len(detail) > 1800 {
			detail = detail[len(detail)-1800:]
		}
		return data, fmt.Errorf("esptool failed: %w\n%s", err, detail)
	}
	return data, nil
}

func toolPortArgs(port, before, after string, rom bool) ([]string, error) {
	if port == "" || strings.HasPrefix(port, "-") || strings.ContainsAny(port, "\r\n\x00") || strings.Contains(port, "://") {
		return nil, errors.New("invalid local serial port")
	}
	args := []string{"--port", port, "--baud", "115200", "--before", before, "--after", after, "--connect-attempts", "2", "--verbose"}
	if rom {
		args = append(args, "--no-stub")
	}
	return args, nil
}

var (
	toolChipLine   = regexp.MustCompile(`(?m)^Connected to (ESP[0-9A-Za-z-]+) on .+:\s*$`)
	toolFlashLine  = regexp.MustCompile(`(?m)^Detected flash size:\s*([0-9]+)(KB|MB)\s*$`)
	toolMACLine    = regexp.MustCompile(`(?mi)^MAC:\s*([0-9a-f]{2}(?::[0-9a-f]{2}){5})\s*$`)
	toolDetailLine = regexp.MustCompile(`(?m)^(Chip type|Features|Crystal frequency|Manufacturer|Device):[ \t]*([^\r\n]+)$`)
)

func parseFlashProbe(data []byte) (flashProbe, error) {
	var probe flashProbe
	chip, size, mac := toolChipLine.FindAllSubmatch(data, -1), toolFlashLine.FindAllSubmatch(data, -1), toolMACLine.FindAllSubmatch(data, -1)
	if len(chip) != 1 || len(size) != 1 || len(mac) != 1 {
		return probe, errors.New("could not establish the board's chip, flash size and MAC from esptool")
	}
	probe.Chip = strings.ToLower(string(chip[0][1]))
	if probe.Chip != "esp32" {
		return probe, fmt.Errorf("unsupported chip %s; Pipkin requires a classic ESP32 CYD", probe.Chip)
	}
	n, err := strconv.ParseInt(string(size[0][1]), 10, 64)
	if err != nil || n <= 0 || n > 256 {
		return probe, errors.New("invalid detected flash size")
	}
	probe.FlashBytes = n * 1024
	if string(size[0][2]) == "MB" {
		probe.FlashBytes *= 1024
	}
	probe.MAC = strings.ToLower(string(mac[0][1]))
	seen := map[string]bool{}
	for _, detail := range toolDetailLine.FindAllSubmatch(data, -1) {
		key, value := string(detail[1]), strings.TrimSpace(string(detail[2]))
		if seen[key] {
			return flashProbe{}, fmt.Errorf("ambiguous esptool %s", key)
		}
		seen[key] = true
		switch key {
		case "Chip type":
			probe.ChipDescription = value
		case "Features":
			probe.Features = value
		case "Crystal frequency":
			frequency, err := strconv.Atoi(strings.TrimSuffix(value, "MHz"))
			if err != nil || frequency <= 0 || frequency > 100 {
				return flashProbe{}, errors.New("invalid crystal frequency")
			}
			probe.CrystalMHz = frequency
		case "Manufacturer", "Device":
			width := 2
			if key == "Device" {
				width = 4
			}
			if len(value) != width {
				return flashProbe{}, errors.New("invalid SPI flash ID")
			}
			if _, err := strconv.ParseUint(value, 16, 16); err != nil {
				return flashProbe{}, errors.New("invalid SPI flash ID")
			}
			if key == "Manufacturer" {
				probe.FlashManufacturer = strings.ToLower(value)
			} else {
				probe.FlashDevice = strings.ToLower(value)
			}
		}
	}
	return probe, nil
}

func parseToolRegister(data []byte, address uint32) (uint32, error) {
	pattern := regexp.MustCompile(fmt.Sprintf(`(?mi)^0x%08x = (0x[0-9a-f]{8})\s*$`, address))
	matches := pattern.FindAllSubmatch(data, -1)
	if len(matches) != 1 {
		return 0, errors.New("could not establish ESP32 security eFuse state")
	}
	value, err := strconv.ParseUint(string(matches[0][1]), 0, 32)
	return uint32(value), err
}

func (t *flashTool) Probe(ctx context.Context, port string) (flashProbe, error) {
	args, err := toolPortArgs(port, "default-reset", "no-reset", true)
	if err != nil {
		return flashProbe{}, err
	}
	data, err := t.run(ctx, 35*time.Second, append(args, "flash-id")...)
	if err != nil {
		return flashProbe{}, err
	}
	probe, err := parseFlashProbe(data)
	if err != nil {
		return probe, err
	}
	args, _ = toolPortArgs(port, "no-reset", "no-reset", true)
	var registers [2]uint32
	// Classic ESP32 predates the ROM get-security-info command. These read-only
	// eFuse registers/masks are defined by Espressif's ESP32 target specification:
	// https://github.com/espressif/esptool/blob/v5.4.0/esptool/targets/esp32.py
	for i, address := range []uint32{0x3ff5a000, 0x3ff5a018} {
		data, err = t.run(ctx, 25*time.Second, append(args, "read-mem", fmt.Sprintf("0x%08x", address))...)
		if err != nil {
			return probe, fmt.Errorf("could not check board security: %w", err)
		}
		registers[i], err = parseToolRegister(data, address)
		if err != nil {
			return probe, err
		}
	}
	probe.FlashEncrypted = bits.OnesCount32((registers[0]>>20)&0x7f)%2 != 0
	probe.SecureBoot = registers[1]&0x30 != 0
	probe.Secure = probe.FlashEncrypted || probe.SecureBoot
	return probe, nil
}

func (t *flashTool) Read(ctx context.Context, port string, offset, size int64) ([]byte, error) {
	if offset < 0 || size <= 0 || offset > baselineFlashBytes-size {
		return nil, errors.New("invalid flash read region")
	}
	dir, err := os.MkdirTemp("", "pipkin-flash-read-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "flash.bin")
	args, err := toolPortArgs(port, "no-reset", "no-reset", false)
	if err != nil {
		return nil, err
	}
	args = append(args, "--chip", "esp32", "read-flash", "--no-progress", fmt.Sprintf("0x%x", offset), fmt.Sprintf("0x%x", size), filename)
	if _, err := t.run(ctx, 90*time.Second, args...); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return nil, errors.New("esptool returned an incomplete flash read")
	}
	return os.ReadFile(filename)
}

func (t *flashTool) Write(ctx context.Context, port string, images []flashWriteImage) error {
	if len(images) == 0 || len(images) > 4 {
		return errors.New("invalid firmware image list")
	}
	pairs := []string{}
	var previousEnd int64
	for _, image := range images {
		info, err := os.Lstat(image.Path)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(image.Path) || !info.Mode().IsRegular() || info.Size() <= 0 || image.Offset < previousEnd || image.Offset < 0 || image.Offset > baselineFlashBytes-info.Size() {
			return errors.New("invalid or overlapping firmware image")
		}
		previousEnd = image.Offset + info.Size()
		pairs = append(pairs, fmt.Sprintf("0x%x", image.Offset), image.Path)
	}
	args, err := toolPortArgs(port, "no-reset", "no-reset", false)
	if err != nil {
		return err
	}
	args = append(args, "--chip", "esp32")
	// Explicit baseline options prevent host esptool configuration from changing
	// image headers. Individual files leave intervening NVS sectors untouched.
	options := []string{"--flash-mode", "dio", "--flash-freq", "40m", "--flash-size", "4MB"}
	fmt.Fprintln(t.output, "Writing firmware…")
	if _, err := t.run(ctx, 3*time.Minute, slices.Concat(args, []string{"write-flash"}, options, []string{"--no-progress"}, pairs)...); err != nil {
		return err
	}
	fmt.Fprintln(t.output, "Verifying firmware…")
	_, err = t.run(ctx, 2*time.Minute, slices.Concat(args, []string{"verify-flash"}, options, pairs)...)
	return err
}

func (t *flashTool) Reset(ctx context.Context, port string) error {
	// Each POSIX esptool process opens with pyserial's DTR/RTS defaults. The
	// no-reset path leaves DTR asserted, and hard-reset only pulses RTS: GPIO0
	// then remains low when EN rises, selecting download mode again. Complete
	// the standard bootloader-entry sequence first; it explicitly releases DTR
	// before the final hard reset starts the application.
	args, err := toolPortArgs(port, "default-reset", "hard-reset", true)
	if err != nil {
		return err
	}
	args = append(args, "--chip", "esp32", "read-mac")
	_, err = t.run(ctx, 20*time.Second, args...)
	return err
}

func (t *flashTool) EraseSettings(ctx context.Context, port string) error {
	args, err := toolPortArgs(port, "no-reset", "no-reset", false)
	if err != nil {
		return err
	}
	// Only first installs explicitly approved by the caller use this operation.
	args = append(args, "--chip", "esp32", "erase-region", fmt.Sprintf("0x%x", nvsOffset), fmt.Sprintf("0x%x", nvsSize))
	_, err = t.run(ctx, time.Minute, args...)
	return err
}
