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
	SchemaVersion  int             `json:"schema_version"`
	Product        string          `json:"product"`
	Version        string          `json:"version"`
	SourceRevision string          `json:"source_revision"`
	Board          string          `json:"board"`
	Hardware       string          `json:"hardware"`
	Chip           string          `json:"chip"`
	FlashSize      int64           `json:"flash_size"`
	FlashMode      string          `json:"flash_mode"`
	FlashFreq      string          `json:"flash_freq"`
	ProtocolMin    int             `json:"protocol_min"`
	ProtocolMax    int             `json:"protocol_max"`
	MinimumCLI     string          `json:"minimum_cli"`
	Layout         string          `json:"layout"`
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

// Keep the first public partition layout explicit. Future layouts need a migration
// design; a new manifest must not silently authorize erasing an owner's settings.
func validateFirmwareManifest(m firmwareManifest, tag, cliVersion string) error {
	if m.SchemaVersion != 1 || m.Product != "pipkin" {
		return errors.New("unsupported firmware manifest")
	}
	if _, valid := stableVersionNumbers(m.Version); !valid || strings.HasPrefix(m.Version, "v") || tag != "v"+m.Version {
		return errors.New("firmware manifest version does not match its stable release tag")
	}
	if !firmwareRevision.MatchString(m.SourceRevision) {
		return errors.New("firmware manifest has no valid source revision")
	}
	if _, valid := stableVersionNumbers(m.MinimumCLI); !valid || strings.HasPrefix(m.MinimumCLI, "v") {
		return errors.New("firmware manifest has an invalid minimum CLI version")
	}
	if _, valid := stableVersionNumbers(cliVersion); !valid || newerStableFirmware(m.MinimumCLI, cliVersion) {
		return fmt.Errorf("firmware requires CLI %s or later; run pipkin update", m.MinimumCLI)
	}
	if m.Board != "esp32-2432s028r-provisional" || (m.Hardware != "unconfirmed" && m.Hardware != "confirmed") || m.Chip != "esp32" {
		return errors.New("release does not support the ESP32-2432S028R board profile")
	}
	if m.ProtocolMin != 1 || m.ProtocolMax < 1 {
		return errors.New("release is incompatible with the display protocol supported by this CLI")
	}
	if m.FlashSize != 4<<20 || m.FlashMode != "dio" || m.FlashFreq != "40m" || m.Layout != "esp32-single-app-v1" {
		return errors.New("release uses an unsupported flash layout or flash settings")
	}
	if len(m.Images) != 3 {
		return errors.New("release must contain bootloader, partition table and application images")
	}
	want := map[string]struct {
		file        string
		offset, max int64
	}{
		"bootloader":      {"bootloader.bin", 0x1000, 0x7000},
		"partition-table": {"partition-table.bin", 0x8000, 0xc00},
		"application":     {"pipkin.bin", 0x10000, 1 << 20},
	}
	seen := map[string]bool{}
	for _, image := range m.Images {
		expected, ok := want[image.Role]
		if !ok || seen[image.Role] || image.File != expected.file || image.Offset != expected.offset || image.Size <= 0 || image.Size > expected.max || !firmwareSHA256.MatchString(image.SHA256) {
			return fmt.Errorf("invalid or unsupported %s firmware image", terminalText(image.Role))
		}
		if image.Role == "partition-table" && image.Size != 0xc00 {
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
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, errors.New("no published firmware release or required release asset found")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("firmware download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, errReadTooLarge
	}
	var data bytes.Buffer
	if err := copyBounded(&data, response.Body, limit); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func (s firmwareSource) stage(ctx context.Context, selectedVersion, cliVersion, parent string) (_ *firmwareRelease, err error) {
	endpoint := s.api + "/releases/latest"
	if selectedVersion != "" {
		if !stableFirmwareVersion.MatchString(selectedVersion) {
			return nil, errors.New("firmware version must be a stable version such as 1.0.0")
		}
		endpoint = s.api + "/releases/tags/v" + strings.TrimPrefix(selectedVersion, "v")
	}
	metadata, err := s.get(ctx, endpoint, 1<<20)
	if err != nil {
		return nil, err
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(metadata, &release); err != nil {
		return nil, errors.New("invalid firmware release metadata")
	}
	if release.Draft || release.Prerelease || !strings.HasPrefix(release.Tag, "v") || !stableFirmwareVersion.MatchString(release.Tag) || selectedVersion != "" && release.Tag != "v"+strings.TrimPrefix(selectedVersion, "v") {
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
	digest := sha256.Sum256(manifestData)
	if sums["firmware.json"] != hex.EncodeToString(digest[:]) {
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
	if err := validateFirmwareManifest(manifest, release.Tag, cliVersion); err != nil {
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
		digest := sha256.Sum256(data)
		if int64(len(data)) != image.Size || hex.EncodeToString(digest[:]) != image.SHA256 {
			return nil, fmt.Errorf("%s failed its release checksum or size check", image.File)
		}
		if err := validateFirmwareImage(image, data, manifest); err != nil {
			return nil, fmt.Errorf("%s is incompatible: %w", image.File, err)
		}
		if err := os.WriteFile(filepath.Join(folder, image.File), data, 0o600); err != nil {
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

func newerStableFirmware(candidate, current string) bool {
	a, aOK := stableVersionNumbers(candidate)
	b, bOK := stableVersionNumbers(current)
	if !aOK || !bOK {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
