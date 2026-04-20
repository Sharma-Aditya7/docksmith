// runtime.go 
//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// runInChroot executes shellCmd inside rootDir using Linux namespaces +
// SysProcAttr.Chroot. ENV vars are exported into the shell so $VAR expansion
// works correctly inside RUN commands and CMD.
//
// This is the single isolation primitive used by both RUN (build) and
// `docksmith run` — satisfying the hard requirement.
func runInChroot(rootDir, shellCmd string, env []string, workdir string) error {
	if workdir == "" {
		workdir = "/"
	}

	setupMinimalDevProc(rootDir)

	// Mount /proc inside the container.
	procDir := filepath.Join(rootDir, "proc")
	_ = os.MkdirAll(procDir, 0755)
	_ = syscall.Mount("proc", procDir, "proc", 0, "")
	defer func() { _ = syscall.Unmount(procDir, syscall.MNT_DETACH) }()

	// Build a shell preamble that exports every ENV var so that $VAR
	// expansion works inside RUN commands and CMD strings.
	// e.g.  export GREETING='Hello'; export APP_ENV='production'; <shellCmd>
	var exports []string
	for _, kv := range env {
		idx := strings.Index(kv, "=")
		if idx > 0 {
			k := kv[:idx]
			v := kv[idx+1:]
			// Single-quote the value and escape any existing single quotes.
			safeV := strings.ReplaceAll(v, "'", "'\\''")
			exports = append(exports, fmt.Sprintf("export %s='%s'", k, safeV))
		}
	}
	fullCmd := shellCmd
	if len(exports) > 0 {
		fullCmd = strings.Join(exports, "; ") + "; " + shellCmd
	}

	cmd := exec.Command("/bin/sh", "-c", fullCmd)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWUTS,
		Chroot: rootDir,
	}

	// Clean env — PATH only; variables are exported via the shell preamble above.
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}

	// Dir is interpreted after chroot — path inside the container.
	cmd.Dir = workdir

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// setupMinimalDevProc creates the minimum directories inside rootDir.
func setupMinimalDevProc(rootDir string) {
	for _, d := range []string{"proc", "dev", "sys", "tmp", "etc"} {
		_ = os.MkdirAll(filepath.Join(rootDir, d), 0755)
	}
	resolvPath := filepath.Join(rootDir, "etc", "resolv.conf")
	if _, err := os.Stat(resolvPath); os.IsNotExist(err) {
		_ = os.WriteFile(resolvPath, []byte(""), 0644)
	}
}

// RunOptions holds parsed flags for `docksmith run`.
type RunOptions struct {
	NameTag      string
	CmdArgs      []string
	EnvOverrides map[string]string
	StateDir     string
}

// CmdRun implements `docksmith run <name:tag> [cmd]`.
func CmdRun(opts RunOptions) error {
	imagesDir := filepath.Join(opts.StateDir, "images")
	layersDir := filepath.Join(opts.StateDir, "layers")

	nameTag := opts.NameTag
	if !strings.Contains(nameTag, ":") {
		nameTag += ":latest"
	}

	m, err := loadManifest(imagesDir, nameTag)
	if err != nil {
		return fmt.Errorf("image %q not found", nameTag)
	}

	// Determine the shell command to run.
	// If a cmd override is given at runtime, treat it as a shell string.
	// Otherwise use the image CMD array.
	var shellCmd string
	if len(opts.CmdArgs) > 0 {
		shellCmd = strings.Join(opts.CmdArgs, " ")
	} else if len(m.Config.Cmd) > 0 {
		// If CMD is ["/bin/sh", "-c", "actual command"], extract just the
		// actual command so runInChroot doesn't double-wrap it.
		cmd := m.Config.Cmd
		if len(cmd) == 3 && (cmd[0] == "/bin/sh" || cmd[0] == "sh") && cmd[1] == "-c" {
			shellCmd = cmd[2]
		} else {
			shellCmd = strings.Join(cmd, " ")
		}
	} else {
		return errors.New("no CMD defined in image and no command provided")
	}

	// Assemble filesystem into a temp directory.
	tmpRoot, err := os.MkdirTemp("", "docksmith-container-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot) // cleanup enforces host isolation

	if err := extractImageLayers(m, layersDir, tmpRoot); err != nil {
		return fmt.Errorf("assembling image: %w", err)
	}

	// Merge image ENV with -e overrides (overrides take precedence).
	envMap := make(map[string]string)
	for _, kv := range m.Config.Env {
		k, v, ok := parseEnvArg(kv)
		if ok {
			envMap[k] = v
		}
	}
	for k, v := range opts.EnvOverrides {
		envMap[k] = v
	}
	envList := buildEnvList(envMap)

	workdir := m.Config.WorkingDir
	if workdir == "" {
		workdir = "/"
	}

	err = runInChroot(tmpRoot, shellCmd, envList, workdir)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			fmt.Printf("Container exited with code %d\n", exitErr.ExitCode())
			return nil
		}
		return err
	}
	fmt.Println("Container exited with code 0")
	return nil
}

// childMain is kept so main.go compiles — re-exec approach not used.
func childMain(_ []string) {
	fmt.Fprintln(os.Stderr, "docksmith: unexpected __child__ invocation")
	os.Exit(1)
}

