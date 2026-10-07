// fakeinstall records arguments for the Windows installer test.
package main

import (
	"os"
	"strings"
)

func main() {
	if err := os.WriteFile(os.Getenv("TEST_INSTALLED"), []byte(strings.Join(os.Args[1:], " ")), 0o644); err != nil {
		os.Exit(1)
	}
}
