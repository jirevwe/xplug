package xplug

import (
	"bytes"
	"context"
	"fmt"
	"github.com/Masterminds/semver/v3"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"text/template"
	"time"
)

func Main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go trapSignals(ctx, cancel)

	if len(os.Args) > 1 && os.Args[1] == "build" {
		if err := Build(ctx, os.Args[2:]...); err != nil {
			log.Fatalf("[ERROR] %v", err)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version())
		return
	}
}

func Build(ctx context.Context, args ...string) error {
	var err error
	plugins := []Dependency{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--with":
			if i == len(args)-1 {
				return fmt.Errorf("expected value after --with flag")
			}
			i++
			mod, ver, err := splitWith(args[i])
			if err != nil {
				return err
			}
			mod = strings.TrimSuffix(mod, "/") // easy to accidentally leave a trailing slash if pasting from a URL, but is invalid for Go modules
			plugins = append(plugins, Dependency{
				PackagePath: mod,
				Version:     ver,
			})
		default:
			continue
		}
	}

	// clean up any SIV-incompatible module paths real quick
	for i, p := range plugins {
		plugins[i].PackagePath, err = versionedModulePath(p.PackagePath, p.Version)
		if err != nil {
			return err
		}
	}

	// create the context for the main module template
	tplCtx := goModTemplateContext{}
	for _, p := range plugins {
		tplCtx.Plugins = append(tplCtx.Plugins, p.PackagePath)
	}

	// evaluate the template for the main module
	var buf bytes.Buffer
	tpl, err := template.New("main").Parse(mainModuleTemplate)
	if err != nil {
		return err
	}
	err = tpl.Execute(&buf, tplCtx)
	if err != nil {
		return err
	}

	log.Printf("[INFO] Written main.go: %s", buf)

	// create the folder in which the build environment will operate
	tempFolder, err := newTempFolder()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err2 := os.RemoveAll(tempFolder)
			if err2 != nil {
				err = fmt.Errorf("%w; additionally, cleaning up folder: %v", err, err2)
			}
		}
	}()
	log.Printf("[INFO] Temporary folder: %s", tempFolder)

	// write the main module file to temporary folder
	mainPath := filepath.Join(tempFolder, "main.go")
	log.Printf("[INFO] Writing main module: %s\n%s", mainPath, buf.Bytes())
	err = os.WriteFile(mainPath, buf.Bytes(), 0o644)
	if err != nil {
		return err
	}

	log.Println("[INFO] Initializing Go module")
	whichGo(ctx, tempFolder)

	cmd := newCommand(ctx, tempFolder, "go", []string{"mod", "init", "plug"}...)
	err = runCommand(ctx, cmd)
	if err != nil {
		return err
	}

	log.Println("[INFO] Pinning versions")
	cmd = newCommand(ctx, tempFolder, "go", []string{"get", "-v", "-d", defaultModulePath}...)
	err = runCommand(ctx, cmd)
	if err != nil {
		return err
	}

	log.Println("[INFO] Downloading plugins")
	for _, p := range plugins {
		mod := p.PackagePath
		if p.Version != "" {
			mod += "@" + p.Version
		}

		cmd = newCommand(ctx, tempFolder, "go", []string{"get", "-v", "-d", mod}...)
		err = runCommand(ctx, cmd)
		if err != nil {
			return err
		}
	}

	cmd = newCommand(ctx, tempFolder, "go", []string{"get", "-v", "-d", ""}...)
	err = runCommand(ctx, cmd)
	if err != nil {
		return err
	}

	log.Println("[INFO] Build environment ready")

	log.Println("[INFO] Building your new version...")
	cmd = newCommand(ctx, tempFolder, "go", []string{"build", "-o", "plug", "."}...)
	err = runCommand(ctx, cmd)
	if err != nil {
		return err
	}

	log.Println("[INFO] Build complete")

	dir, err := os.Getwd()
	if err != nil {
		return err
	}

	cmd = newCommand(ctx, tempFolder, "mv", []string{tempFolder + "/plug", dir}...)
	err = runCommand(ctx, cmd)
	if err != nil {
		return err
	}

	return nil
}

func trapSignals(ctx context.Context, cancel context.CancelFunc) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	select {
	case <-sig:
		log.Printf("[INFO] SIGINT: Shutting down")
		cancel()
	case <-ctx.Done():
		return
	}
}

// version returns a detailed version string, if available.
func version() string {
	mod := goModule()
	ver := mod.Version
	if mod.Sum != "" {
		ver += " " + mod.Sum
	}
	if mod.Replace != nil {
		ver += " => " + mod.Replace.Path
		if mod.Replace.Version != "" {
			ver += "@" + mod.Replace.Version
		}
		if mod.Replace.Sum != "" {
			ver += " " + mod.Replace.Sum
		}
	}
	return ver
}

func goModule() *debug.Module {
	mod := &debug.Module{}
	mod.Version = "unknown"
	bi, ok := debug.ReadBuildInfo()
	if ok {
		mod.Path = bi.Main.Path
		// The recommended way to build xcaddy involves
		// creating a separate main module, which
		// TODO: track related Go issue: https://github.com/golang/go/issues/29228
		// once that issue is fixed, we should just be able to use bi.Main... hopefully.
		for _, dep := range bi.Deps {
			if dep.Path == "github.com/caddyserver/xcaddy" {
				return dep
			}
		}
		return &bi.Main
	}
	return mod
}

// newTempFolder creates a new folder in a temporary location.
// It is the caller's responsibility to remove the folder when finished.
func newTempFolder() (string, error) {
	var parentDir string
	if runtime.GOOS == "darwin" {
		// After upgrading to macOS High Sierra, Caddy builds mysteriously
		// started missing the embedded version information that -ldflags
		// was supposed to produce. But it only happened on macOS after
		// upgrading to High Sierra, and it didn't happen with the usual
		// `go run build.go` -- only when using a buildenv. Bug in git?
		// Nope. Not a bug in Go 1.10 either. Turns out it's a breaking
		// change in macOS High Sierra. When the GOPATH of the buildenv
		// was set to some other folder, like in the $HOME dir, it worked
		// fine. Only within $TMPDIR it broke. The $TMPDIR folder is inside
		// /var, which is symlinked to /private/var, which is mounted
		// with noexec. I don't understand why, but evidently that
		// makes -ldflags of `go build` not work. Bizarre.
		// The solution, I guess, is to just use our own "temp" dir
		// outside of /var. Sigh... as long as it still gets cleaned up,
		// I guess it doesn't matter too much.
		// See: https://github.com/caddyserver/caddy/issues/2036
		// and https://twitter.com/mholt6/status/978345803365273600 (thread)
		// (using an absolute path prevents problems later when removing this
		// folder if the CWD changes)
		var err error
		parentDir, err = filepath.Abs(".")
		if err != nil {
			return "", err
		}
	}
	ts := time.Now().Format(yearMonthDayHourMin)
	return os.MkdirTemp(parentDir, fmt.Sprintf("buildenv_%s.", ts))
}

func newCommand(ctx context.Context, tempFolder, command string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = tempFolder
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

func whichGo(ctx context.Context, folder string) string {
	args := []string{"mod"}
	cmd := newCommand(ctx, folder, "which", args...)
	err := runCommand(ctx, cmd)
	if err != nil {
		log.Fatalf("go is not installed. Err: %v", err)
	}
	return "go"
}

func runCommand(ctx context.Context, cmd *exec.Cmd) error {
	deadline, ok := ctx.Deadline()
	var timeout time.Duration
	// context doesn't necessarily have a deadline
	if ok {
		timeout = time.Until(deadline)
	}
	log.Printf("[INFO] exec (timeout=%s): %+v ", timeout, cmd)

	// start the command; if it fails to start, report error immediately
	err := cmd.Start()
	if err != nil {
		return err
	}

	// wait for the command in a goroutine; the reason for this is
	// very subtle: if, in our select, we do `case cmdErr := <-cmd.Wait()`,
	// then that case would be chosen immediately, because cmd.Wait() is
	// immediately available (even though it blocks for potentially a long
	// time, it can be evaluated immediately). So we have to remove that
	// evaluation from the `case` statement.
	cmdErrChan := make(chan error)
	go func() {
		cmdErrChan <- cmd.Wait()
	}()

	// unblock either when the command finishes, or when the done
	// channel is closed -- whichever comes first
	select {
	case cmdErr := <-cmdErrChan:
		// process ended; report any error immediately
		return cmdErr
	case <-ctx.Done():
		// context was canceled, either due to timeout or
		// maybe a signal from higher up canceled the parent
		// context; presumably, the OS also sent the signal
		// to the child process, so wait for it to die
		select {
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
		case <-cmdErrChan:
		}
		return ctx.Err()
	}
}

// versionedModulePath helps enforce Go Module's Semantic Import Versioning (SIV) by
// returning the form of modulePath with the major component of moduleVersion added,
// if > 1. For example, inputs of "foo" and "v1.0.0" will return "foo", but inputs
// of "foo" and "v2.0.0" will return "foo/v2", for use in Go imports and go commands.
// Inputs that conflict, like "foo/v2" and "v3.1.0" are an error. This function
// returns the input if the moduleVersion is not a valid semantic version string.
// If moduleVersion is empty string, the input modulePath is returned without error.
func versionedModulePath(modulePath, moduleVersion string) (string, error) {
	if moduleVersion == "" {
		return modulePath, nil
	}
	ver, err := semver.StrictNewVersion(strings.TrimPrefix(moduleVersion, "v"))
	if err != nil {
		// only return the error if we know they were trying to use a semantic version
		// (could have been a commit SHA or something)
		if strings.HasPrefix(moduleVersion, "v") {
			return "", fmt.Errorf("%s: %v", moduleVersion, err)
		}
		return modulePath, nil
	}
	major := ver.Major()

	// see if the module path has a major version at the end (SIV)
	matches := moduleVersionRegexp.FindStringSubmatch(modulePath)
	if len(matches) == 2 {
		modPathVer, err := strconv.Atoi(matches[1])
		if err != nil {
			return "", fmt.Errorf("this error should be impossible, but module path %s has bad version: %v", modulePath, err)
		}
		if modPathVer != int(major) {
			return "", fmt.Errorf("versioned module path (%s) and requested module major version (%d) diverge", modulePath, major)
		}
	} else if major > 1 {
		modulePath += fmt.Sprintf("/v%d", major)
	}

	return path.Clean(modulePath), nil
}

func splitWith(arg string) (module, version string, err error) {
	const versionSplit, replaceSplit = "@", "="

	parts := strings.SplitN(arg, replaceSplit, 2)
	module = parts[0]

	// accommodate module paths that have @ in them, but we can only tolerate that if there's also
	// a version, otherwise we don't know if it's a version separator or part of the file path (see #109)
	lastVersionSplit := strings.LastIndex(module, versionSplit)
	if lastVersionSplit < 0 {
		if replaceIdx := strings.Index(module, replaceSplit); replaceIdx >= 0 {
			module = module[:replaceIdx]
		}
	} else {
		module, version = module[:lastVersionSplit], module[lastVersionSplit+1:]
		if replaceIdx := strings.Index(version, replaceSplit); replaceIdx >= 0 {
			version = module[:replaceIdx]
		}
	}

	if module == "" {
		err = fmt.Errorf("module name is required")
	}

	return
}
