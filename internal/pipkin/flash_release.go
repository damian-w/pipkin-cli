package pipkin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const firmwareRepository = "damian-w/pipkin"

// The only board the firmware supports. Its single-app ESP-IDF layout keeps
// board settings in NVS, between the partition table and the application.
const firmwareBoard = "esp32-2432s028r"

// Each release also carries a demo reel build of its application, versioned
// <version>-demo, which only 'pipkin flash --demo' installs. firmware.json does
// not list it, so it never changes what a normal flash writes.
const (
	demoFirmwareFile   = "pipkin-demo.bin"
	demoFirmwareSuffix = "-demo"
)

const (
	bootloaderOffset     = 0x1000
	bootloaderMaxSize    = 0x7000
	partitionTableOffset = 0x8000
	partitionTableSize   = 0xc00
	nvsOffset            = 0x9000
	nvsSize              = 0x6000
	applicationOffset    = 0x10000
	applicationMaxSize   = 1 << 20
)

var stableFirmwareVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var firmwareSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var firmwareRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

type firmwareImage struct {
	Role   string `json:"role"`
	File   string `json:"file"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type firmwareManifest struct {
	Product        string          `json:"product"`
	Version        string          `json:"version"`
	SourceRevision string          `json:"source_revision"`
	Board          string          `json:"board"`
	Images         []firmwareImage `json:"images"`
}

type firmwareRelease struct {
	Manifest firmwareManifest
	Folder   string
	Tag      string
}

func (r *firmwareRelease) close() { os.RemoveAll(r.Folder) }

func (r *firmwareRelease) image(role string) (firmwareImage, string) {
	for _, image := range r.Manifest.Images {
		if image.Role == role {
			return image, filepath.Join(r.Folder, image.File)
		}
	}
	return firmwareImage{}, ""
}

// validateFirmwareManifest accepts only firmware with this CLI's major version,
// for the one supported board and flash layout.
func validateFirmwareManifest(m firmwareManifest, tag string) error {
	if m.Product != "pipkin" {
		return errors.New("unsupported firmware manifest")
	}
	numbers, valid := stableVersionNumbers(m.Version)
	if !valid || strings.HasPrefix(m.Version, "v") || tag != "v"+m.Version {
		return errors.New("firmware manifest version does not match its stable release tag")
	}
	if numbers[0] > protocolVersion {
		return fmt.Errorf("firmware %s needs a newer Pipkin; run 'pipkin update' first", m.Version)
	}
	if numbers[0] < protocolVersion {
		return fmt.Errorf("firmware %s is too old for this version of Pipkin", m.Version)
	}
	if !firmwareRevision.MatchString(m.SourceRevision) {
		return errors.New("firmware manifest has no valid source revision")
	}
	if m.Board != firmwareBoard {
		return errors.New("release does not support the ESP32-2432S028R board")
	}
	if len(m.Images) != 3 {
		return errors.New("release must contain bootloader, partition table and application images")
	}
	want := map[string]struct {
		file        string
		offset, max int64
	}{
		"bootloader":      {"bootloader.bin", bootloaderOffset, bootloaderMaxSize},
		"partition-table": {"partition-table.bin", partitionTableOffset, partitionTableSize},
		"application":     {"pipkin.bin", applicationOffset, applicationMaxSize},
	}
	seen := map[string]bool{}
	for _, image := range m.Images {
		expected, ok := want[image.Role]
		if !ok || seen[image.Role] || image.File != expected.file || image.Offset != expected.offset || image.Size <= 0 || image.Size > expected.max || !firmwareSHA256.MatchString(image.SHA256) {
			return fmt.Errorf("invalid or unsupported %s firmware image", terminalText(image.Role))
		}
		if image.Role == "partition-table" && image.Size != partitionTableSize {
			return errors.New("release partition table has an unsupported size")
		}
		seen[image.Role] = true
	}
	return nil
}

type firmwareSource struct {
	api, downloads string
	client         *http.Client
}

func publicFirmwareSource() firmwareSource {
	return firmwareSource{
		api:       "https://api.github.com/repos/" + firmwareRepository,
		downloads: "https://github.com/" + firmwareRepository + "/releases/download",
		client:    httpClient,
	}
}

func (s firmwareSource) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "pipkin/"+version)
	var data bytes.Buffer
	err = download(s.client, request, &data, limit)
	if status := (*httpStatusError)(nil); errors.As(err, &status) {
		if status.Code == http.StatusNotFound {
			return nil, errors.New("no published firmware release or required release asset found")
		}
		return nil, fmt.Errorf("firmware download returned HTTP %d", status.Code)
	}
	return data.Bytes(), err
}

type firmwareReleaseInfo struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// release reads one GitHub release and accepts only a published stable version.
func (s firmwareSource) release(ctx context.Context, endpoint string) (firmwareReleaseInfo, error) {
	var release firmwareReleaseInfo
	metadata, err := s.get(ctx, endpoint, 1<<20)
	if err != nil {
		return release, err
	}
	if err := json.Unmarshal(metadata, &release); err != nil {
		return release, errors.New("invalid firmware release metadata")
	}
	if release.Draft || release.Prerelease || !strings.HasPrefix(release.Tag, "v") || !stableFirmwareVersion.MatchString(release.Tag) {
		return release, errors.New("firmware release is not the requested published stable version")
	}
	return release, nil
}

func (s firmwareSource) stage(ctx context.Context, selectedVersion, parent string, demo bool) (_ *firmwareRelease, err error) {
	endpoint := s.api + "/releases/latest"
	if selectedVersion != "" {
		if !stableFirmwareVersion.MatchString(selectedVersion) {
			return nil, errors.New("firmware version must be a stable version such as 1.0.0")
		}
		endpoint = s.api + "/releases/tags/v" + strings.TrimPrefix(selectedVersion, "v")
	}
	release, err := s.release(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if selectedVersion != "" && release.Tag != "v"+strings.TrimPrefix(selectedVersion, "v") {
		return nil, errors.New("firmware release is not the requested published stable version")
	}
	base := s.downloads + "/" + release.Tag + "/"
	assets := map[string]string{}
	for _, asset := range release.Assets {
		if _, duplicate := assets[asset.Name]; duplicate {
			return nil, errors.New("firmware release has duplicate assets")
		}
		// Resolve only assets belonging to the fixed repository and selected tag.
		if asset.URL == base+asset.Name && filepath.Base(asset.Name) == asset.Name && !strings.ContainsAny(asset.Name, `\?#`) {
			assets[asset.Name] = asset.URL
		}
	}
	if _, ok := assets[demoFirmwareFile]; demo && !ok {
		return nil, fmt.Errorf("firmware release %s has no demo reel", release.Tag)
	}
	fetch := func(name string, limit int64) ([]byte, error) {
		url, ok := assets[name]
		if !ok {
			return nil, fmt.Errorf("firmware release %s is missing %s", release.Tag, name)
		}
		return s.get(ctx, url, limit)
	}
	checksumData, err := fetch("SHA256SUMS", maxChecksumBytes)
	if err != nil {
		return nil, err
	}
	sums, err := parseChecksums(checksumData)
	if err != nil {
		return nil, err
	}
	manifestData, err := fetch("firmware.json", 32<<10)
	if err != nil {
		return nil, err
	}
	if sums["firmware.json"] != sha256Hex(manifestData) {
		return nil, errors.New("firmware manifest failed its release checksum")
	}
	var manifest firmwareManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("invalid firmware manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("firmware manifest contains trailing data")
	}
	if err := validateFirmwareManifest(manifest, release.Tag); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	folder, err := os.MkdirTemp(parent, "firmware-")
	if err != nil {
		return nil, err
	}
	staged := &firmwareRelease{Manifest: manifest, Folder: folder, Tag: release.Tag}
	defer func() {
		if err != nil {
			staged.close()
		}
	}()
	for _, image := range manifest.Images {
		if sums[image.File] != image.SHA256 {
			return nil, fmt.Errorf("%s manifest and release checksums disagree", image.File)
		}
		data, err := fetch(image.File, image.Size)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != image.Size || sha256Hex(data) != image.SHA256 {
			return nil, fmt.Errorf("%s failed its release checksum or size check", image.File)
		}
		if err := validateFirmwareImage(image, data, manifest); err != nil {
			return nil, fmt.Errorf("%s is incompatible: %w", image.File, err)
		}
		if err := os.WriteFile(filepath.Join(folder, image.File), data, 0o600); err != nil {
			return nil, err
		}
	}
	if demo {
		// The demo reel takes the application's place, so it passes the same checks.
		data, err := fetch(demoFirmwareFile, applicationMaxSize)
		if err != nil {
			return nil, err
		}
		image := firmwareImage{Role: "application", File: demoFirmwareFile, Offset: applicationOffset,
			Size: int64(len(data)), SHA256: sums[demoFirmwareFile]}
		if sha256Hex(data) != image.SHA256 {
			return nil, fmt.Errorf("%s failed its release checksum", demoFirmwareFile)
		}
		reel := manifest
		reel.Version += demoFirmwareSuffix
		if err := validateFirmwareImage(image, data, reel); err != nil {
			return nil, fmt.Errorf("%s is incompatible: %w", demoFirmwareFile, err)
		}
		if err := os.WriteFile(filepath.Join(folder, demoFirmwareFile), data, 0o600); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

func stableVersionNumbers(text string) ([3]uint64, bool) {
	var result [3]uint64
	if !stableFirmwareVersion.MatchString(text) {
		return result, false
	}
	for i, part := range strings.Split(strings.TrimPrefix(text, "v"), ".") {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return result, false
		}
		result[i] = n
	}
	return result, true
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
