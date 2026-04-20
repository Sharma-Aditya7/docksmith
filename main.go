// main.go — Integration entry point
// Wires together build.go + runtime.go + cli.go.
// Also handles the __child__ re-exec trick used by runInChroot.
//
// Build:  go build -o docksmith .
// State:  ~/.docksmith/

//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	// ── Re-exec child entry point ──────────────────────────────────────────
	// When runInChroot forks itself via /proc/self/exe, it passes "__child__"
	// as the first argument.  We intercept that here BEFORE any normal logic.
	if len(os.Args) >= 2 && os.Args[1] == "__child__" {
		childMain(os.Args[2:]) // defined in runtime.go
		os.Exit(0)
	}

	// ── State directory ────────────────────────────────────────────────────
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("cannot determine home directory: %v", err)
	}
	stateDir := filepath.Join(home, ".docksmith")

	// Ensure top-level directories exist
	for _, sub := range []string{"images", "layers", "cache"} {
		if err := os.MkdirAll(filepath.Join(stateDir, sub), 0755); err != nil {
			fatal("cannot create state dir %s: %v", sub, err)
		}
	}

	// ── Parse CLI arguments ────────────────────────────────────────────────
	if len(os.Args) < 2 {
		PrintHelp()
		os.Exit(0)
	}

	pc, err := ParseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		PrintHelp()
		os.Exit(1)
	}

	// ── Dispatch ───────────────────────────────────────────────────────────
	switch pc.Command {

	// ── build ──────────────────────────────────────────────────────────────
	case "build":
		opts := BuildOptions{
			Tag:      pc.BuildTag,
			Context:  pc.BuildContext,
			NoCache:  pc.NoCache,
			StateDir: stateDir,
		}
		engine := NewBuildEngine(opts)
		if err := engine.Run(); err != nil {
			fatal("build failed: %v", err)
		}

	// ── images ─────────────────────────────────────────────────────────────
	case "images":
		if err := CmdImages(stateDir); err != nil {
			fatal("images: %v", err)
		}

	// ── rmi ────────────────────────────────────────────────────────────────
	case "rmi":
		if err := CmdRmi(stateDir, pc.NameTag); err != nil {
			fatal("rmi: %v", err)
		}

	// ── run ────────────────────────────────────────────────────────────────
	case "run":
		opts := RunOptions{
			NameTag:      pc.NameTag,
			CmdArgs:      pc.RunCmd,
			EnvOverrides: pc.EnvOverrides,
			StateDir:     stateDir,
		}
		if err := CmdRun(opts); err != nil {
			fatal("run: %v", err)
		}

	// ── import-base ────────────────────────────────────────────────────────
	case "import-base":
		opts := ImportBaseOptions{
			TarPath:  pc.ImportTar,
			NameTag:  pc.ImportName,
			StateDir: stateDir,
		}
		if err := CmdImportBase(opts); err != nil {
			fatal("import-base: %v", err)
		}

	// ── scaffold ───────────────────────────────────────────────────────────
	case "scaffold":
		dir := "."
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		if err := CmdScaffold(dir); err != nil {
			fatal("scaffold: %v", err)
		}

	// ── help ───────────────────────────────────────────────────────────────
	case "help", "--help", "-h":
		PrintHelp()

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", pc.Command)
		PrintHelp()
		os.Exit(1)
	}
}

// fatal prints an error message and exits with code 1.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "docksmith: "+format+"\n", args...)
	os.Exit(1)
}

