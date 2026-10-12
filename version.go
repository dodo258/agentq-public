package main

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var versionText string

func releaseVersion() string { return strings.TrimSpace(versionText) }
