package pipkin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type firmwareHTTPFixture struct {
	server   *httptest.Server
	source   firmwareSource
	files    map[string][]byte
	metadata map[string]any
	requests []string
}

func newFirmwareHTTPFixture(t *testing.T) *firmwareHTTPFixture {
	t.Helper()
	r := testFirmwareRelease(t, "1.0.0")
	f := &firmwareHTTPFixture{files: map[string][]byte{}}
	for i := range r.Manifest.Images {
		image := &r.Manifest.Images[i]
		data := buildTestESPImage(r.Manifest.Version, r.Manifest.SourceRevision)
		if image.Role == "partition-table" {
			data = testPartitionTable()
		}
		image.Size = int64(len(data))
		digest := sha256.Sum256(data)
		image.SHA256 = hex.EncodeToString(digest[:])
		f.files[image.File] = data
	}
	f.files["firmware.json"], _ = json.Marshal(r.Manifest)
	f.refreshChecksums()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		f.requests = append(f.requests, request.URL.Path)
		if request.URL.Path == "/releases/latest" || request.URL.Path == "/releases/tags/v1.0.0" {
			json.NewEncoder(w).Encode(f.metadata)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/download/v1.0.0/") {
			name := strings.TrimPrefix(request.URL.Path, "/download/v1.0.0/")
			if data, ok := f.files[name]; ok {
				w.Write(data)
				return
			}
		}
		http.NotFound(w, request)
	}))
	t.Cleanup(f.server.Close)
	var assets []map[string]string
	for name := range f.files {
		assets = append(assets, map[string]string{"name": name, "browser_download_url": f.server.URL + "/download/v1.0.0/" + name})
	}
	f.metadata = map[string]any{"tag_name": "v1.0.0", "draft": false, "prerelease": false, "assets": assets}
	f.source = firmwareSource{api: f.server.URL, downloads: f.server.URL + "/download", client: f.server.Client()}
	return f
}

func (f *firmwareHTTPFixture) refreshChecksums() {
	var sums strings.Builder
	for name, data := range f.files {
		if name == "SHA256SUMS" {
			continue
		}
		digest := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%x  %s\n", digest, name)
	}
	f.files["SHA256SUMS"] = []byte(sums.String())
}

func (f *firmwareHTTPFixture) alterManifest(change func(map[string]any)) {
	var manifest map[string]any
	json.Unmarshal(f.files["firmware.json"], &manifest)
	change(manifest)
	f.files["firmware.json"], _ = json.Marshal(manifest)
	f.refreshChecksums()
}

func TestFirmwareReleaseStagesOneFrozenStableVersion(t *testing.T) {
	f := newFirmwareHTTPFixture(t)
	r, err := f.source.stage(context.Background(), "", "1.1.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if r.Tag != "v1.0.0" || len(r.Manifest.Images) != 3 {
		t.Fatal("incorrect release", r)
	}
	for name, data := range f.files {
		if strings.HasSuffix(name, ".bin") {
			got, err := os.ReadFile(filepath.Join(r.Folder, name))
			if err != nil || string(got) != string(data) {
				t.Fatal("staged image does not match release", name, err)
			}
		}
	}
	latest := 0
	for _, request := range f.requests {
		if request == "/releases/latest" {
			latest++
		} else if !strings.HasPrefix(request, "/download/v1.0.0/") {
			t.Fatal("download escaped frozen version", request)
		}
	}
	if latest != 1 {
		t.Fatal("latest firmware release was resolved more than once")
	}
}

func TestFirmwareReleaseRejectsIncompleteCorruptAndUnsafeAssets(t *testing.T) {
	for _, scenario := range []string{"draft", "prerelease", "branch", "missing-asset", "wrong-repository", "duplicate-assets", "bad-sums", "changed-manifest", "trailing-manifest", "unknown-schema-field", "newer-cli", "changed-image", "truncated-image", "unsafe-file", "wrong-app", "wrong-partitions"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFirmwareHTTPFixture(t)
			switch scenario {
			case "draft":
				f.metadata["draft"] = true
			case "prerelease":
				f.metadata["prerelease"] = true
			case "branch":
				f.metadata["tag_name"] = "main"
			case "missing-asset":
				delete(f.files, "pipkin.bin")
			case "wrong-repository":
				assets := f.metadata["assets"].([]map[string]string)
				for _, asset := range assets {
					asset["browser_download_url"] = "https://example.com/" + asset["name"]
				}
			case "duplicate-assets":
				assets := f.metadata["assets"].([]map[string]string)
				f.metadata["assets"] = append(assets, assets[0])
			case "bad-sums":
				f.files["SHA256SUMS"] = []byte("not a checksum file")
			case "changed-manifest":
				f.files["firmware.json"] = append(f.files["firmware.json"], ' ')
			case "trailing-manifest":
				f.files["firmware.json"] = append(f.files["firmware.json"], []byte("\n{}")...)
				f.refreshChecksums()
			case "unknown-schema-field":
				f.alterManifest(func(m map[string]any) { m["erase_all"] = true })
			case "newer-cli":
				f.alterManifest(func(m map[string]any) { m["minimum_cli"] = "2.0.0" })
			case "changed-image":
				f.files["pipkin.bin"][100] ^= 1
			case "truncated-image":
				f.files["pipkin.bin"] = f.files["pipkin.bin"][:32]
			case "unsafe-file":
				f.alterManifest(func(m map[string]any) { m["images"].([]any)[0].(map[string]any)["file"] = "../bootloader.bin" })
			case "wrong-app", "wrong-partitions":
				name := "pipkin.bin"
				if scenario == "wrong-app" {
					f.files[name] = buildTestESPImage("9.9.9", strings.Repeat("a", 40))
				} else {
					name = "partition-table.bin"
					f.files[name][12] ^= 1
				}
				digest := sha256.Sum256(f.files[name])
				f.alterManifest(func(m map[string]any) {
					for _, image := range m["images"].([]any) {
						i := image.(map[string]any)
						if i["file"] == name {
							i["size"] = len(f.files[name])
							i["sha256"] = hex.EncodeToString(digest[:])
						}
					}
				})
			}
			parent := t.TempDir()
			if release, err := f.source.stage(context.Background(), "", "1.1.0", parent); err == nil {
				release.close()
				t.Fatal("unsafe firmware release was accepted")
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatal("failed release left staged executable/images behind")
			}
		})
	}
}

func TestFirmwareReleaseExplicitStableVersionAndCancellation(t *testing.T) {
	f := newFirmwareHTTPFixture(t)
	r, err := f.source.stage(context.Background(), "v1.0.0", "1.1.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.close()
	if f.requests[0] != "/releases/tags/v1.0.0" {
		t.Fatal("explicit version used latest endpoint", f.requests)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.source.stage(ctx, "", "1.1.0", t.TempDir()); err == nil {
		t.Fatal("cancelled firmware request succeeded")
	}
	for _, invalid := range []string{"../main", "main", "v1.0.0-beta", "v1.00.0"} {
		if _, err := f.source.stage(context.Background(), invalid, "1.1.0", t.TempDir()); err == nil {
			t.Fatal("invalid release version accepted", invalid)
		}
	}
}
