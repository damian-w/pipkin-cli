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

// The first supported board, the 2.8-inch CYD. Its release assets keep the plain
// names: firmware.json, bootloader.bin, partition-table.bin and pipkin.bin.
const firmwareBoard = "esp32-2432s028r"

// Each release also carries a demo reel build of each board's application,
// versioned <version>-demo, which only 'pipkin flash --demo' installs. The
// manifests do not list it, so it never changes what a normal flash writes.
const (
	demoFirmwareFile   = "pipkin-demo.bin"
	demoFirmwareSuffix = "-demo"
)

const (
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
	// The selected board's manifest, which is the CYD's until selectBoard.
	Manifest firmwareManifest
	Folder   string
	Tag      string
	// Every board's manifest in the release, by board ID.
	boards map[string]firmwareManifest
}

func (r *firmwareRelease) close() { os.RemoveAll(r.Folder) }

// selectBoard narrows the release to one board's images, which it may not have:
// releases before the 4-inch board carry only the CYD's.
func (r *firmwareRelease) selectBoard(board displayBoard) error {
	manifest, ok := r.boards[board.ID]
	if !ok {
		return fmt.Errorf("firmware %s doesn't support the %s display; install a newer release, or update Pipkin with 'pipkin update'", r.Tag, board.Name)
	}
	r.Manifest = manifest
	return nil
}

func (r *firmwareRelease) image(role string) (firmwareImage, string) {
	for _, image := range r.Manifest.Images {
		if image.Role == role {
			return image, filepath.Join(r.Folder, image.File)
		}
	}
	return firmwareImage{}, ""
}

// validateFirmwareManifest accepts only firmware with this CLI's major version,
// for a supported board and the shared flash layout.
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
	board, ok := boardByID(m.Board)
	if !ok {
		return errors.New("release is for a board this version of Pipkin doesn't support")
	}
	if len(m.Images) != 3 {
		return errors.New("release must contain bootloader, partition table and application images")
	}
	want := map[string]struct {
		file        string
		offset, max int64
	}{
		"bootloader":      {board.imageFile("bootloader"), board.BootloaderOffset, board.BootloaderMaxSize},
		"partition-table": {board.imageFile("partition-table"), partitionTableOffset, partitionTableSize},
		"application":     {board.imageFile("application"), applicationOffset, applicationMaxSize},
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
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	folder, err := os.MkdirTemp(parent, "firmware-")
	if err != nil {
		return nil, err
	}
	staged := &firmwareRelease{Folder: folder, Tag: release.Tag, boards: map[string]firmwareManifest{}}
	defer func() {
		if err != nil {
			staged.close()
		}
	}()
	// Every release has the CYD's firmware.json. Other boards' manifests are optional,
	// and each is checked as strictly when present.
	for _, board := range displayBoards {
		if _, ok := assets[board.manifestFile()]; !ok && board.ID != firmwareBoard {
			continue
		}
		manifest, err := s.stageBoard(fetch, sums, folder, release.Tag, board, demo)
		if err != nil {
			return nil, err
		}
		staged.boards[board.ID] = manifest
	}
	staged.Manifest = staged.boards[firmwareBoard]
	return staged, nil
}

// stageBoard downloads and checks one board's manifest and images, and its demo
// reel when asked and the release has one.
func (s firmwareSource) stageBoard(fetch func(string, int64) ([]byte, error), sums map[string]string, folder, tag string, board displayBoard, demo bool) (firmwareManifest, error) {
	var manifest firmwareManifest
	name := board.manifestFile()
	manifestData, err := fetch(name, 32<<10)
	if err != nil {
		return manifest, err
	}
	if sums[name] != sha256Hex(manifestData) {
		return manifest, errors.New("firmware manifest failed its release checksum")
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("invalid firmware manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return manifest, errors.New("firmware manifest contains trailing data")
	}
	if err := validateFirmwareManifest(manifest, tag); err != nil {
		return manifest, err
	}
	if manifest.Board != board.ID {
		return manifest, fmt.Errorf("%s describes another board", name)
	}
	for _, image := range manifest.Images {
		if sums[image.File] != image.SHA256 {
			return manifest, fmt.Errorf("%s manifest and release checksums disagree", image.File)
		}
		data, err := fetch(image.File, image.Size)
		if err != nil {
			return manifest, err
		}
		if int64(len(data)) != image.Size || sha256Hex(data) != image.SHA256 {
			return manifest, fmt.Errorf("%s failed its release checksum or size check", image.File)
		}
		if err := validateFirmwareImage(image, data, manifest); err != nil {
			return manifest, fmt.Errorf("%s is incompatible: %w", image.File, err)
		}
		if err := os.WriteFile(filepath.Join(folder, image.File), data, 0o600); err != nil {
			return manifest, err
		}
	}
	reelFile := board.demoFile()
	if _, ok := sums[reelFile]; !demo || !ok {
		return manifest, nil
	}
	// The demo reel takes the application's place, so it passes the same checks.
	data, err := fetch(reelFile, applicationMaxSize)
	if err != nil {
		return manifest, err
	}
	image := firmwareImage{Role: "application", File: reelFile, Offset: applicationOffset,
		Size: int64(len(data)), SHA256: sums[reelFile]}
	if sha256Hex(data) != image.SHA256 {
		return manifest, fmt.Errorf("%s failed its release checksum", reelFile)
	}
	reel := manifest
	reel.Version += demoFirmwareSuffix
	if err := validateFirmwareImage(image, data, reel); err != nil {
		return manifest, fmt.Errorf("%s is incompatible: %w", reelFile, err)
	}
	return manifest, os.WriteFile(filepath.Join(folder, reelFile), data, 0o600)
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
