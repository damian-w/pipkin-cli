package pipkin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxBinaryBytes   = 64 << 20
	maxChecksumBytes = 1 << 20
)

// Keep the lock outside the installation so uninstall cannot replace it.
func installationLockPath() string {
	dir := filepath.Clean(appDir())
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+"-installation.lock")
}

type binaryArtifact struct {
	name, asset, staged string
}

func binaryManifest(goos, goarch string) []binaryArtifact {
	bases := []string{"pipkin"}
	extension := ""
	if goos == "windows" {
		bases = append(bases, "pipkinw")
		extension = ".exe"
	}
	artifacts := make([]binaryArtifact, 0, len(bases))
	for _, base := range bases {
		artifacts = append(artifacts, binaryArtifact{
			name: base + extension, asset: fmt.Sprintf("%s-%s-%s%s", base, goos, goarch, extension),
		})
	}
	return artifacts
}

type stagedBinaries struct {
	folder, target   string
	artifacts        []binaryArtifact
	missingCompanion bool
}

func newStagedBinaries(target, goos, goarch string) (*stagedBinaries, error) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}
	folder, err := os.MkdirTemp(target, ".pipkin-stage-")
	if err != nil {
		return nil, err
	}
	return &stagedBinaries{folder: folder, target: target, artifacts: binaryManifest(goos, goarch)}, nil
}

func (s *stagedBinaries) close() { os.RemoveAll(s.folder) }

func stageLocalBinaries(source, target, goos, goarch string) (_ *stagedBinaries, err error) {
	staged, err := newStagedBinaries(target, goos, goarch)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			staged.close()
		}
	}()
	for i := range staged.artifacts {
		artifact := &staged.artifacts[i]
		path := source
		if i != 0 {
			path = filepath.Join(filepath.Dir(source), artifact.name)
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) && i != 0 {
			// A console-only source build must not leave a stale companion installed.
			staged.missingCompanion = true
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular executable file", artifact.name)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		artifact.staged = filepath.Join(staged.folder, artifact.name)
		copyErr := writeStagedFile(artifact.staged, func(w io.Writer) error {
			return copyBounded(w, file, maxBinaryBytes)
		})
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return nil, fmt.Errorf("could not stage %s: %w", artifact.name, err)
		}
	}
	return staged, nil
}

func copyBounded(writer io.Writer, reader io.Reader, limit int64) error {
	n, err := io.Copy(writer, io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%w (%d bytes)", errReadTooLarge, limit)
	}
	return nil
}

func downloadTo(url string, writer io.Writer, limit int64) error {
	response, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, response.Status)
	}
	if response.ContentLength > limit {
		return fmt.Errorf("%w (%d bytes)", errReadTooLarge, limit)
	}
	return copyBounded(writer, response.Body, limit)
}

func writeStagedFile(path string, write func(io.Writer) error) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	writeErr := write(file)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func parseChecksums(data []byte) (map[string]string, error) {
	sums := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, errors.New("invalid release checksum manifest")
		}
		digest, err := hex.DecodeString(fields[0])
		name := strings.TrimPrefix(fields[1], "*")
		if err != nil || len(digest) != sha256.Size || name == "" {
			return nil, errors.New("invalid release checksum manifest")
		}
		if _, exists := sums[name]; exists {
			return nil, fmt.Errorf("duplicate checksum for %s", name)
		}
		sums[name] = strings.ToLower(fields[0])
	}
	return sums, nil
}

func stageVerifiedAsset(release string, artifact *binaryArtifact, folder, expected string) error {
	artifact.staged = filepath.Join(folder, artifact.name)
	return writeStagedFile(artifact.staged, func(writer io.Writer) error {
		hash := sha256.New()
		if err := downloadTo(release+"/"+artifact.asset, io.MultiWriter(writer, hash), maxBinaryBytes); err != nil {
			return err
		}
		if expected != hex.EncodeToString(hash.Sum(nil)) {
			return fmt.Errorf("%s failed its checksum", artifact.asset)
		}
		return nil
	})
}

func stageReleaseBinaries(release, target, goos, goarch string) (_ *stagedBinaries, err error) {
	var checksums bytes.Buffer
	if err := downloadTo(release+"/SHA256SUMS", &checksums, maxChecksumBytes); err != nil {
		return nil, err
	}
	sums, err := parseChecksums(checksums.Bytes())
	if err != nil {
		return nil, err
	}
	staged, err := newStagedBinaries(target, goos, goarch)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			staged.close()
		}
	}()
	for _, artifact := range staged.artifacts {
		if _, ok := sums[artifact.asset]; !ok {
			return nil, fmt.Errorf("%s has no release checksum", artifact.asset)
		}
	}
	for i := range staged.artifacts {
		if err := stageVerifiedAsset(release, &staged.artifacts[i], staged.folder, sums[staged.artifacts[i].asset]); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

type binaryReplacement struct {
	target    string
	artifacts []binaryArtifact
	backedUp  []bool
	installed []bool
}

func (s *stagedBinaries) validateTargets() error {
	for _, artifact := range s.artifacts {
		path := filepath.Join(s.target, artifact.name)
		present := [2]bool{}
		for i, candidate := range []string{path, path + ".old"} {
			info, err := os.Lstat(candidate)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			present[i] = err == nil
			if err == nil && !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular executable file", candidate)
			}
		}
		if !present[0] && present[1] {
			return fmt.Errorf("%s is missing but its recovery backup remains at %s.old; restore that backup before retrying", artifact.name, path)
		}
	}
	return nil
}

func (s *stagedBinaries) replace() (*binaryReplacement, error) {
	if err := s.validateTargets(); err != nil {
		return nil, err
	}
	replacement := &binaryReplacement{target: s.target, artifacts: s.artifacts,
		backedUp: make([]bool, len(s.artifacts)), installed: make([]bool, len(s.artifacts))}
	for i, artifact := range s.artifacts {
		path := filepath.Join(s.target, artifact.name)
		if err := os.Remove(path + ".old"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(err, replacement.rollback())
		}
		if err := os.Rename(path, path+".old"); err == nil {
			replacement.backedUp[i] = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(err, replacement.rollback())
		}
		if artifact.staged != "" {
			if err := os.Rename(artifact.staged, path); err != nil {
				return nil, errors.Join(err, replacement.rollback())
			}
			replacement.installed[i] = true
		}
	}
	return replacement, nil
}

func (r *binaryReplacement) rollback() error {
	var failures []error
	for i := len(r.artifacts) - 1; i >= 0; i-- {
		path := filepath.Join(r.target, r.artifacts[i].name)
		if r.installed[i] {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("could not remove replacement %s: %w", r.artifacts[i].name, err))
				continue
			}
		}
		if r.backedUp[i] {
			if err := os.Rename(path+".old", path); err != nil {
				failures = append(failures, fmt.Errorf("could not restore %s from %s.old: %w", r.artifacts[i].name, path, err))
			}
		}
	}
	return errors.Join(failures...)
}

func (r *binaryReplacement) finish() {
	for i, artifact := range r.artifacts {
		if r.backedUp[i] {
			// Windows may retain the running backup until the next upgrade.
			os.Remove(filepath.Join(r.target, artifact.name) + ".old")
		}
	}
}

type installationActions struct {
	running                  func() (int, error)
	stop, start              func() error
	register                 func() (string, error)
	snapshot                 func() (func() error, error)
	configure, undoConfigure func() error
	authorize                func()
}

func helperInstallationActions() installationActions {
	return installationActions{running: runningPID, stop: stopHelper, start: startHelper,
		register: registerAutostart, snapshot: snapshotAutostart, authorize: authorizeInstalledHelper}
}

func activateBinaries(staged *stagedBinaries, actions installationActions) (string, error) {
	if err := staged.validateTargets(); err != nil {
		return "", err
	}
	pid, err := actions.running()
	if err != nil {
		return "", err
	}
	restoreRegistration, err := actions.snapshot()
	if err != nil {
		return "", fmt.Errorf("could not read previous automatic start: %w", err)
	}
	if err := actions.stop(); err != nil {
		if pid != 0 {
			err = errors.Join(err, actions.start())
		}
		return "", err
	}
	replacement, err := staged.replace()
	if err != nil {
		if pid != 0 {
			err = errors.Join(err, actions.start())
		}
		return "", err
	}
	startAttempted := false
	recover := func(cause error) (string, error) {
		if startAttempted {
			if err := actions.stop(); err != nil {
				return "", errors.Join(cause, fmt.Errorf("could not stop replacement helper; original binaries remain in .old backups: %w", err))
			}
		}
		if actions.undoConfigure != nil {
			if err := actions.undoConfigure(); err != nil {
				cause = errors.Join(cause, fmt.Errorf("could not restore command launcher: %w", err))
			}
		}
		if err := replacement.rollback(); err != nil {
			return "", errors.Join(cause, err)
		}
		if err := restoreRegistration(); err != nil {
			cause = errors.Join(cause, fmt.Errorf("could not restore automatic start: %w", err))
		}
		if pid != 0 {
			if err := actions.start(); err != nil {
				cause = errors.Join(cause, fmt.Errorf("previous helper could not restart: %w", err))
			}
		}
		return "", cause
	}
	// Older startup definitions may omit the custom installation directory.
	mechanism, err := actions.register()
	if err != nil {
		return recover(err)
	}
	if actions.authorize != nil {
		actions.authorize()
	}
	if actions.configure != nil {
		if err := actions.configure(); err != nil {
			return recover(err)
		}
	}
	startAttempted = true
	if err := actions.start(); err != nil {
		return recover(err)
	}
	replacement.finish()
	return mechanism, nil
}
