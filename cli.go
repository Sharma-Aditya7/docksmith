// cli.go 
// Covers:
//   • `docksmith images`   — list all images
//   • `docksmith rmi`      — remove image + its layers
//   • `docksmith import-base` — import a base image tar into the local store
//   • Sample Docksmithfile and sample app (printed / written on request)
//   • ParseFlags helper consumed by main.go

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// ────────────────────────────────────────────────────────────
// `docksmith images`
// ────────────────────────────────────────────────────────────

// CmdImages prints all images in the local store.
// Columns: NAME  TAG  ID(12-char digest prefix)  CREATED
func CmdImages(stateDir string) error {
	imagesDir := filepath.Join(stateDir, "images")
	manifests, err := listManifests(imagesDir)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tTAG\tID\tCREATED")
	for _, m := range manifests {
		// ID = first 12 chars of digest (strip "sha256:" prefix first)
		digest := strings.TrimPrefix(m.Digest, "sha256:")
		id := digest
		if len(id) > 12 {
			id = id[:12]
		}
		// Pretty-print created time
		created := m.Created
		if t, err := time.Parse(time.RFC3339, m.Created); err == nil {
			created = t.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Name, m.Tag, id, created)
	}
	return w.Flush()
}

// ────────────────────────────────────────────────────────────
// `docksmith rmi`
// ────────────────────────────────────────────────────────────

// CmdRmi removes an image manifest and all of its layer files.
// Spec: "no reference counting is performed. If multiple images share a layer,
// deleting one image will remove the shared layer files" — this is expected behaviour.
// We implement exactly as spec says: delete ALL layer files belonging to this image.
func CmdRmi(stateDir, nameTag string) error {
	imagesDir := filepath.Join(stateDir, "images")
	layersDir := filepath.Join(stateDir, "layers")

	if !strings.Contains(nameTag, ":") {
		nameTag += ":latest"
	}

	m, err := loadManifest(imagesDir, nameTag)
	if err != nil {
		return fmt.Errorf("image %q not found", nameTag)
	}

	// Remove ALL layer files belonging to this image (spec: no reference counting).
	// Note: if a base image shares a layer, that layer will be deleted too —
	// this is the specified behaviour per section 8 Constraints.
	for _, l := range m.Layers {
		p := layerPath(layersDir, l.Digest)
		if removeErr := os.Remove(p); removeErr != nil && !os.IsNotExist(removeErr) {
			fmt.Fprintf(os.Stderr, "warning: could not remove layer %s: %v\n", l.Digest[:16], removeErr)
		}
	}

	// Remove manifest file
	manifestPath := filepath.Join(imagesDir, m.Name+":"+m.Tag+".json")
	if err := os.Remove(manifestPath); err != nil {
		return fmt.Errorf("removing manifest: %w", err)
	}

	fmt.Printf("Deleted image %s:%s\n", m.Name, m.Tag)
	return nil
}

// ────────────────────────────────────────────────────────────
// `docksmith import-base`
// ────────────────────────────────────────────────────────────

// ImportBaseOptions holds parameters for the import-base command.
type ImportBaseOptions struct {
	TarPath  string // path to the OCI/Docker-exported tar (or a raw rootfs tar)
	NameTag  string // "name:tag" to register it as
	StateDir string
}

// CmdImportBase imports a pre-downloaded base image tar into the Docksmith store.
//
// Supported input format: a single tar archive that is the *rootfs* of the base
// image (not a Docker-format multi-layer tar).  To obtain this from a real image:
//
//   docker export $(docker create alpine:3.18) | gzip > alpine-3.18.tar.gz
//   # then:
//   docksmith import-base --tar alpine-3.18.tar.gz --name alpine:3.18
//
// The entire tar is stored as one layer.  A manifest is generated with that layer.
func CmdImportBase(opts ImportBaseOptions) error {
	imagesDir := filepath.Join(opts.StateDir, "images")
	layersDir := filepath.Join(opts.StateDir, "layers")
	for _, d := range []string{imagesDir, layersDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}

	nameTag := opts.NameTag
	if !strings.Contains(nameTag, ":") {
		nameTag += ":latest"
	}
	parts := strings.SplitN(nameTag, ":", 2)
	name, tag := parts[0], parts[1]

	// Read the tar
	tarData, err := os.ReadFile(opts.TarPath)
	if err != nil {
		return fmt.Errorf("reading base tar: %w", err)
	}

	// Normalize the tar: re-pack with sorted entries and zeroed timestamps
	// so the same source always produces the same digest (reproducibility).
	tarData, err = normalizeTar(tarData)
	if err != nil {
		return fmt.Errorf("normalizing tar: %w", err)
	}

	// Store as a single layer
	digest, err := writeLayer(layersDir, tarData)
	if err != nil {
		return err
	}

	m := &Manifest{
		Name:    name,
		Tag:     tag,
		Created: time.Now().UTC().Format(time.RFC3339),
		Config: ImageConfig{
			Env:        []string{},
			Cmd:        []string{},
			WorkingDir: "/",
		},
		Layers: []LayerEntry{
			{
				Digest:    digest,
				Size:      int64(len(tarData)),
				CreatedBy: "import-base " + opts.TarPath,
			},
		},
	}

	if err := writeManifest(m, imagesDir); err != nil {
		return err
	}

	fmt.Printf("Imported base image %s:%s (layer %s)\n", name, tag, digest[:15])
	return nil
}

// ────────────────────────────────────────────────────────────
// FLAG / ARG PARSING  (used by main.go)
// ────────────────────────────────────────────────────────────

// ParsedCommand is the result of parsing CLI arguments.
type ParsedCommand struct {
	Command string // "build", "images", "rmi", "run", "import-base"

	// build
	BuildTag     string
	BuildContext string
	NoCache      bool

	// rmi / images / run
	NameTag string

	// run
	RunCmd      []string
	EnvOverrides map[string]string

	// import-base
	ImportTar  string
	ImportName string
}

// ParseArgs parses os.Args[1:] into a ParsedCommand.
func ParseArgs(args []string) (*ParsedCommand, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: docksmith <build|images|rmi|run|import-base> [options]")
	}
	pc := &ParsedCommand{
		Command:      args[0],
		EnvOverrides: map[string]string{},
	}
	rest := args[1:]

	switch pc.Command {
	case "build":
		// docksmith build -t <name:tag> [--no-cache] <context>
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "-t", "--tag":
				i++
				if i >= len(rest) {
					return nil, fmt.Errorf("-t requires an argument")
				}
				pc.BuildTag = rest[i]
			case "--no-cache":
				pc.NoCache = true
			default:
				pc.BuildContext = rest[i]
			}
		}
		if pc.BuildTag == "" {
			return nil, fmt.Errorf("build requires -t <name:tag>")
		}
		if pc.BuildContext == "" {
			pc.BuildContext = "."
		}

	case "images":
		// no args

	case "rmi":
		if len(rest) == 0 {
			return nil, fmt.Errorf("rmi requires <name:tag>")
		}
		pc.NameTag = rest[0]

	case "run":
		// docksmith run [-e KEY=VALUE ...] <name:tag> [cmd ...]
		i := 0
		for i < len(rest) {
			if rest[i] == "-e" {
				i++
				if i >= len(rest) {
					return nil, fmt.Errorf("-e requires KEY=VALUE")
				}
				k, v, ok := parseEnvArg(rest[i])
				if !ok {
					return nil, fmt.Errorf("-e: invalid KEY=VALUE %q", rest[i])
				}
				pc.EnvOverrides[k] = v
			} else if strings.HasPrefix(rest[i], "-e=") {
				kv := strings.TrimPrefix(rest[i], "-e=")
				k, v, ok := parseEnvArg(kv)
				if !ok {
					return nil, fmt.Errorf("-e: invalid KEY=VALUE %q", kv)
				}
				pc.EnvOverrides[k] = v
			} else {
				// first non-flag is name:tag
				pc.NameTag = rest[i]
				pc.RunCmd = rest[i+1:]
				break
			}
			i++
		}
		if pc.NameTag == "" {
			return nil, fmt.Errorf("run requires <name:tag>")
		}

	case "scaffold", "help", "--help", "-h":
		// no special parsing needed; handled directly in main.go

	case "import-base":
		// docksmith import-base --tar <file> --name <name:tag>
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--tar":
				i++
				if i >= len(rest) {
					return nil, fmt.Errorf("--tar requires a file path")
				}
				pc.ImportTar = rest[i]
			case "--name":
				i++
				if i >= len(rest) {
					return nil, fmt.Errorf("--name requires <name:tag>")
				}
				pc.ImportName = rest[i]
			}
		}
		if pc.ImportTar == "" || pc.ImportName == "" {
			return nil, fmt.Errorf("import-base requires --tar <file> --name <name:tag>")
		}

	default:
		return nil, fmt.Errorf("unknown command %q. Valid commands: build, images, rmi, run, import-base", pc.Command)
	}

	return pc, nil
}

// ────────────────────────────────────────────────────────────
// SAMPLE APP SCAFFOLD  (`docksmith scaffold`)
// ────────────────────────────────────────────────────────────

// CmdScaffold writes the sample app files into the current directory so teams
// can demo immediately.  All six instructions are exercised.
func CmdScaffold(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Docksmithfile — uses all 6 instructions + proves glob COPY + WORKDIR auto-create
	docksmithfile := `FROM alpine:3.18
WORKDIR /app
ENV APP_ENV=production
ENV GREETING=Hello
COPY *.txt /app/
RUN echo "Build complete. GREETING=$GREETING APP_ENV=$APP_ENV" && ls /app
CMD ["/bin/sh", "-c", "echo $GREETING from Docksmith && cat /app/message.txt"]
`
	if err := os.WriteFile(filepath.Join(dir, "Docksmithfile"), []byte(docksmithfile), 0644); err != nil {
		return err
	}

	// message.txt — source file that COPY picks up
	msg := "Docksmith container is running!\nBuilt with: FROM COPY RUN WORKDIR ENV CMD\n"
	if err := os.WriteFile(filepath.Join(dir, "message.txt"), []byte(msg), 0644); err != nil {
		return err
	}

	// README
	readme := sampleReadme()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		return err
	}

	fmt.Printf("Sample app written to %s/\n", dir)
	fmt.Println("Next steps:")
	fmt.Println("  1. Import base image (once):  docksmith import-base --tar alpine-3.18.tar --name alpine:3.18")
	fmt.Println("  2. Cold build:                docksmith build -t myapp:latest " + dir)
	fmt.Println("  3. Warm build (same digest):  docksmith build -t myapp:latest " + dir)
	fmt.Println("  4. Edit file + rebuild:       echo 'x' >> " + dir + "/message.txt && docksmith build -t myapp:latest " + dir)
	fmt.Println("  5. List images:               docksmith images")
	fmt.Println("  6. Run:                       docksmith run myapp:latest")
	fmt.Println("  7. Override env:              docksmith run -e GREETING=Howdy myapp:latest")
	fmt.Println("  8. Isolation:                 docksmith run myapp:latest 'touch /tmp/sentinel' && ls /tmp/sentinel")
	fmt.Println("  9. --no-cache:               docksmith build --no-cache -t myapp:latest " + dir)
	fmt.Println(" 10. Remove:                    docksmith rmi myapp:latest")
	return nil
}

func sampleReadme() string {
	return `# Docksmith Sample App

This sample uses all six Docksmithfile instructions:

| Instruction | Usage in this sample |
|-------------|---------------------|
| FROM        | alpine:3.18          |
| WORKDIR     | /app                 |
| ENV         | APP_ENV, GREETING    |
| COPY        | . /app               |
| RUN         | echo + ls            |
| CMD         | prints GREETING + message.txt |

## Demo Sequence

` + "```" + `bash
# 0. Get alpine rootfs once
docker export $(docker create alpine:3.18) > alpine-3.18.tar
docksmith import-base --tar alpine-3.18.tar --name alpine:3.18

# 1. Cold build — all CACHE MISS
docksmith build -t myapp:latest .

# 2. Warm build — all CACHE HIT
docksmith build -t myapp:latest .

# 3. Edit message.txt, rebuild — partial cache miss
echo "changed" >> message.txt
docksmith build -t myapp:latest .

# 4. List images
docksmith images

# 5. Run container
docksmith run myapp:latest

# 6. Override env
docksmith run -e GREETING=Howdy myapp:latest

# 7. Isolation check (file must NOT appear on host)
docksmith run myapp:latest "touch /tmp/should_not_appear && echo done"
ls /tmp/should_not_appear  # must fail

# 8. Remove image
docksmith rmi myapp:latest
` + "```" + `
`
}

// PrintHelp prints usage information.
func PrintHelp() {
	fmt.Print(`Docksmith — simplified Docker-like build and runtime system

Usage:
  docksmith build -t <name:tag> [--no-cache] <context>
  docksmith images
  docksmith rmi <name:tag>
  docksmith run [-e KEY=VALUE ...] <name:tag> [cmd ...]
  docksmith import-base --tar <file> --name <name:tag>
  docksmith scaffold [dir]

Commands:
  build         Build an image from a Docksmithfile
  images        List all images in the local store
  rmi           Remove an image and its layer files
  run           Run a container from an image
  import-base   Import a pre-downloaded base image tar
  scaffold      Write a sample app to get started

Flags for build:
  -t, --tag     Image name and tag (required), e.g. myapp:latest
  --no-cache    Skip all cache lookups and writes

Flags for run:
  -e KEY=VALUE  Override or add an environment variable (repeatable)

State directory: ~/.docksmith/
`)
}

// ────────────────────────────────────────────────────────────
// STATE DIR INSPECTION  (debug helper, not part of spec)
// ────────────────────────────────────────────────────────────

// InspectLayer dumps the manifest of a layer for debugging.
func InspectLayer(stateDir, digest string) {
	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	layersDir := filepath.Join(stateDir, "layers")
	p := layerPath(layersDir, digest)
	fi, err := os.Stat(p)
	if err != nil {
		fmt.Printf("Layer %s not found: %v\n", digest, err)
		return
	}
	fmt.Printf("Layer %s  size=%d bytes  path=%s\n", digest[:15], fi.Size(), p)
}

// DumpCacheIndex pretty-prints the cache index for debugging.
func DumpCacheIndex(stateDir string) {
	cacheDir := filepath.Join(stateDir, "cache")
	idx, err := loadCacheIndex(cacheDir)
	if err != nil {
		fmt.Printf("cache index error: %v\n", err)
		return
	}
	raw, _ := json.MarshalIndent(idx, "", "  ")
	fmt.Printf("Cache index (%d entries):\n%s\n", len(idx), string(raw))
}

