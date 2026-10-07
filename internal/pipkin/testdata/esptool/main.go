// A small subprocess fixture for the flashing backend; never used by Pipkin.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(2)
	}
	if log := os.Getenv("PIPKIN_TOOL_TEST_LOG"); log != "" {
		file, _ := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		fmt.Fprintln(file, strings.Join(args, "|"))
		file.Close()
	}
	for _, arg := range args {
		if arg == os.Getenv("PIPKIN_TOOL_TEST_FAIL") && arg != "" {
			fmt.Fprintln(os.Stderr, "simulated device failure")
			os.Exit(2)
		}
	}
	for i, arg := range args {
		switch arg {
		case "version":
			fmt.Println("esptool v5.4.0\n5.4.0")
			return
		case "--help":
			fmt.Println("write-flash read-flash flash-id get-security-info")
			return
		case "flash-id":
			fmt.Println("Connected to ESP32 on COM7:\nChip type:           ESP32-D0WD-V3 (revision v3.1)\nMAC:                aa:bb:cc:dd:ee:ff\nDetected flash size: 4MB")
			return
		case "read-mem":
			value := "00000000"
			if os.Getenv("PIPKIN_TOOL_TEST_SECURE") != "" {
				value = "00100030"
			}
			fmt.Printf("%s = 0x%s\n", args[i+1], value)
			return
		case "read-flash":
			size, _ := strconv.ParseInt(args[len(args)-2], 0, 64)
			if os.Getenv("PIPKIN_TOOL_TEST_SHORT_READ") != "" {
				size--
			}
			if err := os.WriteFile(args[len(args)-1], make([]byte, size), 0o600); err != nil {
				panic(err)
			}
			return
		case "write-flash":
			fmt.Println("Hash of data verified.")
			return
		case "verify-flash":
			fmt.Println("Verify successful.")
			return
		case "read-mac", "erase-region":
			return
		case "environment":
			data, _ := os.ReadFile(os.Getenv("ESPTOOL_CFGFILE"))
			fmt.Printf("chip=%s color=%s config=%s", os.Getenv("ESPTOOL_CHIP"), os.Getenv("NO_COLOR"), data)
			return
		case "hang", "orphan":
			child := exec.Command(os.Args[0], "child-hang", args[i+1])
			if arg == "hang" {
				child.Stdout, child.Stderr = os.Stdout, os.Stderr
			}
			if err := child.Start(); err != nil {
				panic(err)
			}
			fmt.Println("started child")
			if err := os.WriteFile(args[i+1]+".ready", []byte("ready"), 0o600); err != nil {
				panic(err)
			}
			if arg == "orphan" {
				return
			}
			time.Sleep(time.Minute)
			return
		case "child-hang":
			time.Sleep(time.Second)
			os.WriteFile(args[i+1], []byte("child survived"), 0o600)
			time.Sleep(time.Minute)
			return
		}
	}
	fmt.Fprintln(os.Stderr, "unrecognised test command")
	os.Exit(2)
}
