package main

import (
	_ "embed"

	"github.com/damian-w/pipkin-cli/internal/pipkin"
)

var version = "2.1.0"

//go:embed LICENSE
var licenseText string

//go:embed NOTICE.md
var thirdPartyNotices string

func main() {
	pipkin.Run(version, licenseText, thirdPartyNotices)
}
