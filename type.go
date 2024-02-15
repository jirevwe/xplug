package xplug

import (
	"embed"
	"regexp"
	"strings"
)

//go:embed VERSION
var F embed.FS

func GetVersion() string {
	v := "0.1.0"

	f, err := readVersion(F)
	if err != nil {
		return v
	}

	v = strings.TrimSpace(string(f))
	return v
}

func readVersion(fs embed.FS) ([]byte, error) {
	data, err := fs.ReadFile("VERSION")
	if err != nil {
		return nil, err
	}

	return data, nil
}

type Plugins []Dependency

// Dependency pairs a Go module path with a version.
type Dependency struct {
	// The name (import path) of the Go package. If at a version > 1,
	// it should contain the semantic import version (i.e. "/v2").
	// Used with `go get`.
	PackagePath string `json:"module_path,omitempty"`

	// The version of the Go module, as used with `go get`.
	Version string `json:"version,omitempty"`
}

var moduleVersionRegexp = regexp.MustCompile(`.+/v(\d+)$`)

type goModTemplateContext struct {
	Plugins []string
}

const (
	// yearMonthDayHourMin is the date format
	// used for temporary folder paths.
	yearMonthDayHourMin = "2006-01-02-1504"

	defaultModulePath = "github.com/jirevwe/plug"

	mainModuleTemplate = `package main
import (
	"github.com/jirevwe/plug"
	_ "github.com/jirevwe/plug"

	{{- range .Plugins}}
	_ "{{.}}"
	{{- end}}
)

func main() {
	plug.Main()
}
`
)
