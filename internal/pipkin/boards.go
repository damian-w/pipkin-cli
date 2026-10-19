package pipkin

import "strings"

// displayBoard is a board the firmware supports. Both share one flash layout:
// settings in NVS after the partition table, then a single 1 MB application.
type displayBoard struct {
	ID         string // board= in the identity reply and the firmware manifest
	Name       string // the screen, as plans and prompts show it
	Called     string // the board in a sentence: "Confirm this is a CYD"
	Screen     string // its screen size
	Chip       string // esptool's chip name
	ChipName   string // the chip as people write it
	ChipID     uint16 // chip ID in ESP image headers
	FlashBytes int64  // flash size esptool must detect
	// The bootloader's flash offset and the most it may occupy before the partition table.
	BootloaderOffset, BootloaderMaxSize int64
	// Added to release asset names. The CYD came first and keeps the plain names,
	// which CLIs from before the 4-inch board read.
	Suffix string
}

var displayBoards = []displayBoard{
	{ID: firmwareBoard, Name: "2.8-inch CYD", Called: "a CYD", Screen: "2.8-inch",
		Chip: "esp32", ChipName: "ESP32", ChipID: 0, FlashBytes: 4 << 20,
		BootloaderOffset: 0x1000, BootloaderMaxSize: 0x7000},
	{ID: "es3c40p", Name: "4-inch ES3C40P", Called: "an ES3C40P", Screen: "4-inch",
		Chip: "esp32s3", ChipName: "ESP32-S3", ChipID: 9, FlashBytes: 16 << 20,
		BootloaderOffset: 0, BootloaderMaxSize: 0x8000, Suffix: "-es3c40p"},
}

func boardByID(id string) (displayBoard, bool) {
	for _, board := range displayBoards {
		if board.ID == id {
			return board, true
		}
	}
	return displayBoard{}, false
}

// boardByChip accepts esptool's chip names, such as ESP32-S3, in any case.
func boardByChip(chip string) (displayBoard, bool) {
	chip = strings.ReplaceAll(strings.ToLower(chip), "-", "")
	for _, board := range displayBoards {
		if board.Chip == chip {
			return board, true
		}
	}
	return displayBoard{}, false
}

func (b displayBoard) manifestFile() string { return "firmware" + b.Suffix + ".json" }
func (b displayBoard) demoFile() string     { return "pipkin-demo" + b.Suffix + ".bin" }

// imageFile names a role's release asset: bootloader, partition-table or application.
func (b displayBoard) imageFile(role string) string {
	if role == "application" {
		return "pipkin" + b.Suffix + ".bin"
	}
	return role + b.Suffix + ".bin"
}
