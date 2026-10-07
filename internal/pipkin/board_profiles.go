package pipkin

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// This copy is shipped with the CLI; the firmware repository owns the catalog.
//
//go:embed board_profiles.json
var boardCatalogJSON []byte

type boardVariant struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	PCBMarkings    []string `json:"pcb_markings"`
	ModuleMarkings []string `json:"module_markings"`
	USBConnectors  []string `json:"usb_connectors"`
	Observations   struct {
		DisplayMarking    string  `json:"display_marking"`
		DisplaySizeInches float64 `json:"display_size_inches"`
		NativeWidth       int     `json:"native_width"`
		NativeHeight      int     `json:"native_height"`
	} `json:"observations"`
}

type boardProfile struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases"`
	Name      string   `json:"name"`
	Hardware  string   `json:"hardware"`
	Chip      string   `json:"chip"`
	FlashSize int64    `json:"flash_size"`
	FlashMode string   `json:"flash_mode"`
	FlashFreq string   `json:"flash_freq"`
	Layout    string   `json:"layout"`
	Display   struct {
		Controller string `json:"controller"`
	} `json:"display"`
	Touch struct {
		Controller string `json:"controller"`
	} `json:"touch"`
	Variants []boardVariant `json:"variants"`
}

func boardProfiles() ([]boardProfile, error) {
	var catalog struct {
		SchemaVersion int            `json:"schema_version"`
		Profiles      []boardProfile `json:"profiles"`
	}
	if err := json.Unmarshal(boardCatalogJSON, &catalog); err != nil {
		return nil, err
	}
	if catalog.SchemaVersion != 1 || len(catalog.Profiles) == 0 {
		return nil, errors.New("unsupported board catalog")
	}
	seen := map[string]bool{}
	for _, p := range catalog.Profiles {
		if p.ID == "" || p.Chip != "esp32" || p.FlashSize != baselineFlashBytes || p.FlashMode != "dio" || p.FlashFreq != "40m" || p.Layout != "esp32-single-app-v1" || (p.Hardware != "confirmed" && p.Hardware != "unconfirmed") {
			return nil, errors.New("invalid board profile")
		}
		for _, id := range append(append([]string{p.ID}, p.Aliases...), variantIDs(p.Variants)...) {
			if id == "" || seen[id] {
				return nil, errors.New("duplicate or empty board catalog ID")
			}
			seen[id] = true
		}
	}
	return catalog.Profiles, nil
}

func variantIDs(variants []boardVariant) []string {
	ids := make([]string, 0, len(variants))
	for _, v := range variants {
		ids = append(ids, v.ID)
	}
	return ids
}

func firmwareBoardProfile(id string) (boardProfile, bool) {
	profiles, err := boardProfiles()
	if err != nil {
		return boardProfile{}, false
	}
	for _, p := range profiles {
		if p.ID == id {
			return p, true
		}
		for _, alias := range p.Aliases {
			if alias == id {
				return p, true
			}
		}
	}
	return boardProfile{}, false
}

func physicalBoardProfile(id string) (boardProfile, boardVariant, error) {
	profiles, err := boardProfiles()
	if err != nil {
		return boardProfile{}, boardVariant{}, err
	}
	for _, p := range profiles {
		for _, v := range p.Variants {
			if v.ID == id {
				return p, v, nil
			}
		}
	}
	return boardProfile{}, boardVariant{}, fmt.Errorf("unknown physical board profile %q; run pipkin identify to see candidates", terminalText(id))
}

func validateIdentifyBoard(id string) error {
	if id == "" {
		return nil
	}
	_, _, err := physicalBoardProfile(id)
	return err
}

type rememberedBoard struct {
	MAC               string `json:"mac"`
	Profile           string `json:"profile"`
	Chip              string `json:"chip"`
	ChipDescription   string `json:"chip_description"`
	FlashBytes        int64  `json:"flash_bytes"`
	FlashManufacturer string `json:"flash_manufacturer"`
	FlashDevice       string `json:"flash_device"`
}

type rememberedBoards struct {
	SchemaVersion int               `json:"schema_version"`
	Devices       []rememberedBoard `json:"devices"`
}

func rememberedBoardsPath() string { return filepath.Join(appDir(), "boards.json") }

func loadRememberedBoards() (rememberedBoards, error) {
	boards := rememberedBoards{SchemaVersion: 1}
	file, err := os.Open(rememberedBoardsPath())
	if errors.Is(err, os.ErrNotExist) {
		return boards, nil
	}
	if err != nil {
		return boards, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil {
		return boards, err
	}
	if len(data) > 64*1024 {
		return boards, errors.New("saved board associations exceed size limit")
	}
	if err := json.Unmarshal(data, &boards); err != nil {
		return boards, fmt.Errorf("could not read saved board associations: %w", err)
	}
	if boards.SchemaVersion != 1 {
		return boards, errors.New("unsupported saved board associations")
	}
	seen := map[string]bool{}
	for _, b := range boards.Devices {
		if b.MAC == "" || seen[b.MAC] {
			return boards, errors.New("invalid saved board associations")
		}
		seen[b.MAC] = true
	}
	return boards, nil
}

func boardFingerprint(probe flashProbe, profile string) rememberedBoard {
	return rememberedBoard{MAC: probe.MAC, Profile: profile, Chip: probe.Chip, ChipDescription: probe.ChipDescription, FlashBytes: probe.FlashBytes, FlashManufacturer: probe.FlashManufacturer, FlashDevice: probe.FlashDevice}
}

func catalogBoardInfo(p boardProfile, v boardVariant, status string) identifyBoardInfo {
	return identifyBoardInfo{Profile: v.ID, Name: v.Name, FirmwareProfile: p.ID, Status: status,
		PCBMarkings: v.PCBMarkings, ModuleMarkings: v.ModuleMarkings, USBConnectors: v.USBConnectors,
		DisplayMarking: v.Observations.DisplayMarking, DisplaySizeInches: v.Observations.DisplaySizeInches,
		DisplayWidth: v.Observations.NativeWidth, DisplayHeight: v.Observations.NativeHeight,
		DisplayController: p.Display.Controller, TouchController: p.Touch.Controller}
}

// Only explicit physical-board confirmation can create an association. Generic
// electronics and the running firmware cannot identify a CYD PCB revision.
func inspectBoard(probe flashProbe, requested string) (identifyBoardInfo, error) {
	profiles, err := boardProfiles()
	if err != nil {
		return identifyBoardInfo{}, err
	}
	info := identifyBoardInfo{Status: "unconfirmed"}
	for _, p := range profiles {
		if p.Chip == probe.Chip && p.FlashSize == probe.FlashBytes {
			info.Candidates = append(info.Candidates, variantIDs(p.Variants)...)
		}
	}
	boards, err := loadRememberedBoards()
	if err != nil {
		return info, err
	}
	if requested == "" {
		for _, b := range boards.Devices {
			if b.MAC == probe.MAC {
				if b != boardFingerprint(probe, b.Profile) {
					return info, errors.New("saved board electronics no longer match; inspect the PCB and confirm again with pipkin identify --board PROFILE")
				}
				requested = b.Profile
				break
			}
		}
		if requested == "" {
			return info, nil
		}
		p, v, err := physicalBoardProfile(requested)
		if err != nil {
			return info, err
		}
		if p.Chip != probe.Chip || p.FlashSize != probe.FlashBytes {
			return info, errors.New("saved board profile does not match connected electronics")
		}
		return catalogBoardInfo(p, v, "confirmed locally"), nil
	}
	p, v, err := physicalBoardProfile(requested)
	if err != nil {
		return info, err
	}
	if probe.MAC == "" || p.Chip != probe.Chip || p.FlashSize != probe.FlashBytes {
		return info, errors.New("selected board profile does not match connected electronics; no association was saved")
	}
	if probe.Secure {
		return info, errors.New("secure boot or flash encryption is enabled; this board cannot use the standard profile")
	}
	record := boardFingerprint(probe, requested)
	found := false
	for i, b := range boards.Devices {
		if b.MAC == probe.MAC {
			boards.Devices[i] = record
			found = true
		}
	}
	if !found {
		boards.Devices = append(boards.Devices, record)
	}
	data, err := json.MarshalIndent(boards, "", "  ")
	if err != nil {
		return info, err
	}
	if err := writeFileAtomic(rememberedBoardsPath(), append(data, '\n'), 0600); err != nil {
		return info, fmt.Errorf("could not remember board profile: %w", err)
	}
	return catalogBoardInfo(p, v, "confirmed locally"), nil
}

func sameFirmwareBoard(a, b string) bool {
	left, ok := firmwareBoardProfile(a)
	if !ok {
		return false
	}
	right, ok := firmwareBoardProfile(b)
	return ok && left.ID == right.ID
}

func profileCompatibleWithManifest(profile string, manifest firmwareManifest) bool {
	p, _, err := physicalBoardProfile(profile)
	return err == nil && sameFirmwareBoard(p.ID, manifest.Board) && p.Chip == manifest.Chip && p.FlashSize == manifest.FlashSize && p.FlashMode == manifest.FlashMode && p.FlashFreq == manifest.FlashFreq && p.Layout == manifest.Layout
}
