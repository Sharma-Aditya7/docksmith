// build.go 
// Covers: Docksmithfile parser, build engine, image format (manifest + layers),
//         build cache, content-addressed layer storage, all 6 instructions.

package build

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ────────────────────────────────────────────────────────────
// 1.  IMAGE FORMAT
// ────────────────────────────────────────────────────────────

// LayerEntry is one entry in the manifest's layers array.
type LayerEntry struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	CreatedBy string `json:"createdBy"`
}

// ImageConfig holds the image-level configuration (env, cmd, workdir).
type ImageConfig struct {
	Env        []string `json:"Env"`
	Cmd        []string `json:"Cmd"`
	WorkingDir string   `json:"WorkingDir"`
}

// Manifest is the JSON file stored in ~/.docksmith/images/<name>:<tag>.json
type Manifest struct {
	Name    string      `json:"name"`
	Tag     string      `json:"tag"`
	Digest  string      `json:"digest"`
	Created string      `json:"created"`
	Config  ImageConfig `json:"config"`
	Layers  []LayerEntry `json:"layers"`
}

// computeManifestDigest serialises m with Digest="" and returns "sha256:<hex>".
func computeManifestDigest(m *Manifest) (string, error) {
	tmp := *m
	tmp.Digest = ""
	raw, err := json.Marshal(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

// writeManifest computes the digest, sets it, and writes the file.
func writeManifest(m *Manifest, imagesDir string) error {
	digest, err := computeManifestDigest(m)
	if err != nil {
		return err
	}
	m.Digest = digest
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(imagesDir, m.Name+":"+m.Tag+".json")
	return os.WriteFile(path, raw, 0644)
}

// WriteManifest is an exported wrapper for manifest writes used by other packages.
func WriteManifest(m *Manifest, imagesDir string) error {
	return writeManifest(m, imagesDir)
}

// loadManifest reads a manifest by name:tag.
func loadManifest(imagesDir, nameTag string) (*Manifest, error) {
	path := filepath.Join(imagesDir, nameTag+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("image %q not found: %w", nameTag, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest is an exported wrapper for manifest reads used by other packages.
func LoadManifest(imagesDir, nameTag string) (*Manifest, error) {
	return loadManifest(imagesDir, nameTag)
}

// listManifests returns all manifests in imagesDir, deduplicated by name:tag.
// If two files claim the same name:tag, the one whose filename matches
// "<name>:<tag>.json" wins (canonical); others are stale and ignored.
func listManifests(imagesDir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		return nil, err
	}
	// Use a map to deduplicate: canonical filename = name+":"+tag+".json"
	seen := map[string]*Manifest{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		nameTag := strings.TrimSuffix(e.Name(), ".json")
		m, err := loadManifest(imagesDir, nameTag)
		if err != nil {
			continue
		}
		canonical := m.Name + ":" + m.Tag
		// Only keep if the filename matches the canonical name:tag
		// This drops ghost manifests from old broken runs
		if e.Name() == canonical+".json" {
			seen[canonical] = m
		}
	}
	out := make([]*Manifest, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	// Sort by name then tag for stable output
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Tag < out[j].Tag
	})
	return out, nil
}

// ListManifests is an exported wrapper for listing manifests.
func ListManifests(imagesDir string) ([]*Manifest, error) {
	return listManifests(imagesDir)
}

// ────────────────────────────────────────────────────────────
// 2.  DOCKSMITHFILE PARSER
// ────────────────────────────────────────────────────────────

// Instruction kinds.
const (
	InstrFROM    = "FROM"
	InstrCOPY    = "COPY"
	InstrRUN     = "RUN"
	InstrWORKDIR = "WORKDIR"
	InstrENV     = "ENV"
	InstrCMD     = "CMD"
)

// Instruction represents one parsed line.
type Instruction struct {
	Line    int
	Kind    string
	Args    string // raw argument string (everything after the keyword)
}

// ParseDocksmithfile parses a Docksmithfile, returning instructions or an error
// with line number information.
func ParseDocksmithfile(path string) ([]Instruction, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read Docksmithfile: %w", err)
	}
	lines := strings.Split(string(raw), "\n")
	validKinds := map[string]bool{
		InstrFROM: true, InstrCOPY: true, InstrRUN: true,
		InstrWORKDIR: true, InstrENV: true, InstrCMD: true,
	}
	var instrs []Instruction
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		kw := strings.ToUpper(parts[0])
		if !validKinds[kw] {
			return nil, fmt.Errorf("line %d: unrecognised instruction %q", i+1, parts[0])
		}
		args := ""
		if len(parts) > 1 {
			args = strings.TrimSpace(parts[1])
		}
		instrs = append(instrs, Instruction{Line: i + 1, Kind: kw, Args: args})
	}
	return instrs, nil
}

// ────────────────────────────────────────────────────────────
// 3.  LAYER STORAGE  (content-addressed tar files)
// ────────────────────────────────────────────────────────────

// digestBytes returns "sha256:<hex>" of raw bytes.
func digestBytes(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// layerPath converts a digest to an on-disk path.
func layerPath(layersDir, digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(layersDir, hex+".tar")
}

// LayerPath is an exported wrapper for digest-based layer file paths.
func LayerPath(layersDir, digest string) string {
	return layerPath(layersDir, digest)
}

// writeLayer stores b in layersDir, returns digest.
func writeLayer(layersDir string, b []byte) (string, error) {
	digest := digestBytes(b)
	p := layerPath(layersDir, digest)
	if _, err := os.Stat(p); err == nil {
		return digest, nil // already exists, idempotent
	}
	return digest, os.WriteFile(p, b, 0644)
}

// WriteLayer is an exported wrapper for writing content-addressed layers.
func WriteLayer(layersDir string, b []byte) (string, error) {
	return writeLayer(layersDir, b)
}

// buildTarFromDir creates a deterministic tar of rootDir relative to base.
// Entries are sorted, timestamps zeroed — required for reproducibility.
func buildTarFromDir(rootDir string, files []string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Sort for determinism
	sort.Strings(files)

	for _, absPath := range files {
		rel, err := filepath.Rel(rootDir, absPath)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)

		fi, err := os.Lstat(absPath)
		if err != nil {
			return nil, err
		}

		hdr := &tar.Header{
			Name:     rel,
			Mode:     int64(fi.Mode()),
			ModTime:  time.Time{}, // zeroed for reproducibility
			Typeflag: tar.TypeReg,
			Size:     fi.Size(),
		}

		if fi.IsDir() {
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
			hdr.Size = 0
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			continue
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(absPath)
			if err != nil {
				return nil, err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = target
			hdr.Size = 0
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, err
			}
			continue
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		f, err := os.Open(absPath)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
// normalizeTar re-packs a tar archive with entries sorted and timestamps zeroed.
// This ensures the same source content always produces the same digest.
func normalizeTar(input []byte) ([]byte, error) {
	// First pass: collect all entries
	type entry struct {
		hdr  *tar.Header
		data []byte
	}
	var entries []entry
	tr := tar.NewReader(bytes.NewReader(input))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// Zero timestamps for reproducibility
		hdr.ModTime = time.Time{}
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}
		var data []byte
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			data, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
		}
		entries = append(entries, entry{hdr, data})
	}
	// Sort by name for determinism
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].hdr.Name < entries[j].hdr.Name
	})
	// Second pass: write normalized tar
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(e.hdr); err != nil {
			return nil, err
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// NormalizeTar is an exported wrapper used by import-base.
func NormalizeTar(input []byte) ([]byte, error) {
	return normalizeTar(input)
}


// extractTar extracts a tar archive into destDir.
func extractTar(tarData []byte, destDir string) error {
	tr := tar.NewReader(bytes.NewReader(tarData))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)|0755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)|0644)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

// extractImageLayers extracts all layers of m into destDir in order.
// If a layer file is missing (e.g. after rmi of a shared image), returns a
// clear error telling the user to re-import the base image.
func extractImageLayers(m *Manifest, layersDir, destDir string) error {
	for _, l := range m.Layers {
		p := layerPath(layersDir, l.Digest)
		data, err := os.ReadFile(p)
		if err != nil {
			short := l.Digest
			if len(short) > 15 { short = short[:15] }
			return fmt.Errorf("layer %s missing (run: docksmith import-base to restore base image): %w", short, err)
		}
		if err := extractTar(data, destDir); err != nil {
			short := l.Digest
			if len(short) > 15 { short = short[:15] }
			return fmt.Errorf("extracting layer %s: %w", short, err)
		}
	}
	return nil
}

// ExtractImageLayers is an exported wrapper used by runtime.
func ExtractImageLayers(m *Manifest, layersDir, destDir string) error {
	return extractImageLayers(m, layersDir, destDir)
}

// ────────────────────────────────────────────────────────────
// 4.  BUILD CACHE
// ────────────────────────────────────────────────────────────

// CacheIndex is the in-memory representation of ~/.docksmith/cache/index.json
type CacheIndex map[string]string // cacheKey -> layerDigest

func loadCacheIndex(cacheDir string) (CacheIndex, error) {
	p := filepath.Join(cacheDir, "index.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return CacheIndex{}, nil
		}
		return nil, err
	}
	var idx CacheIndex
	return idx, json.Unmarshal(raw, &idx)
}

// LoadCacheIndex is an exported wrapper used by CLI debug helpers.
func LoadCacheIndex(cacheDir string) (CacheIndex, error) {
	return loadCacheIndex(cacheDir)
}

func saveCacheIndex(cacheDir string, idx CacheIndex) error {
	p := filepath.Join(cacheDir, "index.json")
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0644)
}

// computeCacheKey builds the deterministic cache key for a COPY or RUN step.
//   prevDigest   – digest of last layer (or base manifest digest for first step)
//   instrText    – full instruction text as written
//   workdir      – current WORKDIR at the time of this instruction
//   env          – map of ENV accumulated so far
//   copyHashes   – sorted list of "path:sha256hex" only for COPY instructions
func computeCacheKey(prevDigest, instrText, workdir string, env map[string]string, copyHashes []string) string {
	h := sha256.New()
	h.Write([]byte(prevDigest + "\x00"))
	h.Write([]byte(instrText + "\x00"))
	h.Write([]byte(workdir + "\x00"))

	// ENV: lexicographically sorted key=value pairs
	envKeys := make([]string, 0, len(env))
	for k := range env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		h.Write([]byte(k + "=" + env[k] + "\x00"))
	}

	// COPY file hashes (already sorted by caller)
	for _, ch := range copyHashes {
		h.Write([]byte(ch + "\x00"))
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// hashFile returns the SHA-256 hex of a file's raw bytes.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ────────────────────────────────────────────────────────────
// 5.  GLOB EXPANSION  (supports * and **)
// ────────────────────────────────────────────────────────────

// expandGlob resolves a glob pattern relative to contextDir.
// Supports * (single segment) and ** (any depth).
func expandGlob(contextDir, pattern string) ([]string, error) {
	// Use filepath.Glob for simple patterns; walk for **
	if !strings.Contains(pattern, "**") {
		abs := filepath.Join(contextDir, pattern)
		matches, err := filepath.Glob(abs)
		if err != nil {
			return nil, err
		}
		return matches, nil
	}
	// ** handling: walk contextDir and match
	var matches []string
	err := filepath.WalkDir(contextDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(contextDir, path)
		rel = filepath.ToSlash(rel)
		ok, _ := matchDoubleGlob(pattern, rel)
		if ok {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, err
}

// matchDoubleGlob matches a path against a pattern containing **.
// Pattern like **/*.txt matches any .txt file at any depth.
// Pattern like src/**/*.go matches any .go file under src/ at any depth.
func matchDoubleGlob(pattern, name string) (bool, error) {
	// Split on ** to get prefix and suffix patterns
	parts := strings.SplitN(pattern, "**", 2)
	if len(parts) == 1 {
		// No ** — use standard filepath.Match
		return filepath.Match(pattern, name)
	}

	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[1], "/")

	// Check prefix constraint
	if prefix != "" {
		if !strings.HasPrefix(name, prefix+"/") && name != prefix {
			return false, nil
		}
	}

	// The part of the name after the prefix is what ** matches
	remaining := name
	if prefix != "" {
		remaining = strings.TrimPrefix(name, prefix+"/")
	}

	// If no suffix pattern, ** matches everything
	if suffix == "" {
		return true, nil
	}

	// Match the remaining path against the suffix pattern.
	// The suffix may itself contain *, so use filepath.Match on the base.
	// We match the base filename against the suffix pattern.
	base := filepath.Base(remaining)
	matched, err := filepath.Match(suffix, base)
	if err != nil {
		return false, err
	}
	if matched {
		return true, nil
	}

	// Also try matching the full remaining path against suffix
	// to handle patterns like **/*.txt where remaining = "subdir/sub.txt"
	// We check each path component suffix
	parts2 := strings.Split(remaining, "/")
	for i := range parts2 {
		sub := strings.Join(parts2[i:], "/")
		ok, _ := filepath.Match(suffix, sub)
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// collectFiles returns a sorted list of all regular files under paths.
func collectFiles(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if fi.IsDir() {
			err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !seen[path] {
					seen[path] = true
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else {
			if !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

// ────────────────────────────────────────────────────────────
// 6.  BUILD ENGINE
// ────────────────────────────────────────────────────────────

// BuildOptions controls the build.
type BuildOptions struct {
	Tag       string // "name:tag"
	Context   string // context directory
	NoCache   bool
	StateDir  string // ~/.docksmith
}

// BuildEngine orchestrates the build.
type BuildEngine struct {
	opts        BuildOptions
	imagesDir   string
	layersDir   string
	cacheDir    string
	cacheIdx    CacheIndex
	// current build state
	layers      []LayerEntry
	baseLayers  []LayerEntry // layers from FROM — refreshed each build from current manifest
	env         map[string]string
	workdir     string
	cmd         []string
	prevDigest  string // last layer digest (or base manifest digest)
	cacheMiss   bool   // once true, all subsequent steps are misses
	created     string // ISO-8601, set on first build, preserved on cache hits
}

func NewBuildEngine(opts BuildOptions) *BuildEngine {
	return &BuildEngine{
		opts:      opts,
		imagesDir: filepath.Join(opts.StateDir, "images"),
		layersDir: filepath.Join(opts.StateDir, "layers"),
		cacheDir:  filepath.Join(opts.StateDir, "cache"),
		env:       map[string]string{},
	}
}

// Run executes the full build and writes the manifest.
func (e *BuildEngine) Run() error {
	// Ensure dirs exist
	for _, d := range []string{e.imagesDir, e.layersDir, e.cacheDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}

	// Load cache index
	var err error
	if !e.opts.NoCache {
		e.cacheIdx, err = loadCacheIndex(e.cacheDir)
		if err != nil {
			return err
		}
	} else {
		e.cacheIdx = CacheIndex{}
	}

	// Load existing manifest's created timestamp so warm rebuilds preserve it.
	// Spec: "When all steps are cache hits, the manifest is rewritten with
	// the original created value so the manifest digest remains identical."
	existingNameTag := e.opts.Tag
	if !strings.Contains(existingNameTag, ":") {
		existingNameTag += ":latest"
	}
	if existing, loadErr := loadManifest(e.imagesDir, existingNameTag); loadErr == nil {
		e.created = existing.Created
	}

	// Parse Docksmithfile
	docksmithfilePath := filepath.Join(e.opts.Context, "Docksmithfile")
	instrs, err := ParseDocksmithfile(docksmithfilePath)
	if err != nil {
		return err
	}

	// Count total steps for display
	total := len(instrs)
	start := time.Now()

	for i, instr := range instrs {
		stepNum := i + 1
		switch instr.Kind {
		case InstrFROM:
			if err := e.execFROM(instr, stepNum, total); err != nil {
				return err
			}
		case InstrCOPY:
			if err := e.execCOPY(instr, stepNum, total); err != nil {
				return err
			}
		case InstrRUN:
			if err := e.execRUN(instr, stepNum, total); err != nil {
				return err
			}
		case InstrWORKDIR:
			e.workdir = instr.Args
			fmt.Printf("Step %d/%d : WORKDIR %s\n", stepNum, total, instr.Args)
		case InstrENV:
			k, v, ok := parseEnvArg(instr.Args)
			if !ok {
				return fmt.Errorf("line %d: invalid ENV syntax %q", instr.Line, instr.Args)
			}
			e.env[k] = v
			fmt.Printf("Step %d/%d : ENV %s\n", stepNum, total, instr.Args)
		case InstrCMD:
			var arr []string
			if err := json.Unmarshal([]byte(instr.Args), &arr); err != nil {
				return fmt.Errorf("line %d: CMD must be a JSON array: %w", instr.Line, err)
			}
			e.cmd = arr
			fmt.Printf("Step %d/%d : CMD %s\n", stepNum, total, instr.Args)
		}
	}

	// Build and write manifest
	nameTag := e.opts.Tag
	parts := strings.SplitN(nameTag, ":", 2)
	name := parts[0]
	tag := "latest"
	if len(parts) == 2 {
		tag = parts[1]
	}

	// Set created timestamp: use preserved value (from existing manifest on cache hits)
	// or generate fresh one for first-ever build.
	if e.created == "" {
		e.created = time.Now().UTC().Format(time.RFC3339)
	}

	// Build env slice for manifest
	envSlice := make([]string, 0, len(e.env))
	for k, v := range e.env {
		envSlice = append(envSlice, k+"="+v)
	}
	sort.Strings(envSlice)

	m := &Manifest{
		Name:    name,
		Tag:     tag,
		Created: e.created,
		Config: ImageConfig{
			Env:        envSlice,
			Cmd:        e.cmd,
			WorkingDir: e.workdir,
		},
		Layers: e.layers,
	}

	if err := writeManifest(m, e.imagesDir); err != nil {
		return err
	}

	// Save cache index
	if !e.opts.NoCache {
		if err := saveCacheIndex(e.cacheDir, e.cacheIdx); err != nil {
			return err
		}
	}

	elapsed := time.Since(start).Seconds()
	// Spec example: "Successfully built sha256:a3f9b2c1 myapp:latest (3.91s)"
	// Show "sha256:" + first 8 hex chars of digest
	hexPart := strings.TrimPrefix(m.Digest, "sha256:")
	shortDigest := "sha256:" + hexPart
	if len(hexPart) > 8 {
		shortDigest = "sha256:" + hexPart[:8]
	}
	fmt.Printf("Successfully built %s %s (%.2fs)\n", shortDigest, nameTag, elapsed)
	return nil
}

// execFROM handles the FROM instruction.
func (e *BuildEngine) execFROM(instr Instruction, step, total int) error {
	fmt.Printf("Step %d/%d : FROM %s\n", step, total, instr.Args)
	arg := instr.Args
	nameTag := arg
	if !strings.Contains(arg, ":") {
		nameTag = arg + ":latest"
	}
	m, err := loadManifest(e.imagesDir, nameTag)
	if err != nil {
		return fmt.Errorf("FROM %s: %w", nameTag, err)
	}
	// Inherit base layers — store separately so execRUN can always find them
	e.baseLayers = make([]LayerEntry, len(m.Layers))
	copy(e.baseLayers, m.Layers)
	e.layers = make([]LayerEntry, len(m.Layers))
	copy(e.layers, m.Layers)
	// Use manifest digest as the seed for first layer-producing step's cache key
	e.prevDigest = m.Digest
	// Inherit created timestamp if this is a warm rebuild
	if e.created == "" {
		// Will be set fresh during build; FROM doesn't set it
	}
	return nil
}

// execCOPY handles the COPY instruction.
func (e *BuildEngine) execCOPY(instr Instruction, step, total int) error {
	stepStart := time.Now()

	// Parse src and dest
	copyParts := strings.Fields(instr.Args)
	if len(copyParts) < 2 {
		return fmt.Errorf("line %d: COPY requires <src> <dest>", instr.Line)
	}
	dest := copyParts[len(copyParts)-1]
	srcPatterns := copyParts[:len(copyParts)-1]

	// Expand globs and collect source files
	var allSrcFiles []string
	for _, pat := range srcPatterns {
		matched, err := expandGlob(e.opts.Context, pat)
		if err != nil {
			return fmt.Errorf("line %d: COPY glob %q: %w", instr.Line, pat, err)
		}
		allSrcFiles = append(allSrcFiles, matched...)
	}
	srcFiles, err := collectFiles(allSrcFiles)
	if err != nil {
		return fmt.Errorf("line %d: COPY collect: %w", instr.Line, err)
	}

	// Compute file hashes for cache key (sorted by path)
	var copyHashes []string
	type pathHash struct{ path, hash string }
	var phs []pathHash
	for _, f := range srcFiles {
		fi, err := os.Lstat(f)
		if err != nil || fi.IsDir() {
			continue
		}
		h, err := hashFile(f)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e.opts.Context, f)
		phs = append(phs, pathHash{rel, h})
	}
	sort.Slice(phs, func(i, j int) bool { return phs[i].path < phs[j].path })
	for _, ph := range phs {
		copyHashes = append(copyHashes, ph.path+":"+ph.hash)
	}

	instrText := instr.Kind + " " + instr.Args
	cacheKey := computeCacheKey(e.prevDigest, instrText, e.workdir, e.env, copyHashes)

	// Cache lookup
	if !e.opts.NoCache && !e.cacheMiss {
		if layerDigest, ok := e.cacheIdx[cacheKey]; ok {
			if _, err := os.Stat(layerPath(e.layersDir, layerDigest)); err == nil {
				// Cache hit — reuse
				layerSize, _ := layerFileSize(e.layersDir, layerDigest)
				e.layers = append(e.layers, LayerEntry{
					Digest:    layerDigest,
					Size:      layerSize,
					CreatedBy: instrText,
				})
				e.prevDigest = layerDigest
				elapsed := time.Since(stepStart).Seconds()
				fmt.Printf("Step %d/%d : %s [CACHE HIT] %.2fs\n", step, total, instrText, elapsed)
				return nil
			}
		}
	}

	// Cache miss — execute
	e.cacheMiss = true

	// We need to build the layer tar.
	// Strategy: create a temp rootfs reflecting current layers, then
	// copy src files into it at dest, then tar only the new/changed files.
	tmpRoot, err := os.MkdirTemp("", "docksmith-copy-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot)

	// Ensure dest dir exists inside tmpRoot
	absDest := filepath.Join(tmpRoot, filepath.FromSlash(dest))
	// If dest ends in / or multiple sources, treat as directory
	isDir := strings.HasSuffix(dest, "/") || len(srcFiles) > 1
	if isDir {
		if err := os.MkdirAll(absDest, 0755); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(absDest), 0755); err != nil {
			return err
		}
	}

	// Create workdir in tmpRoot if set
	if e.workdir != "" {
		if err := os.MkdirAll(filepath.Join(tmpRoot, filepath.FromSlash(e.workdir)), 0755); err != nil {
			return err
		}
	}

	// Copy files
	var newFiles []string
	for _, srcPath := range srcFiles {
		fi, err := os.Lstat(srcPath)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e.opts.Context, srcPath)
		var targetPath string
		if isDir {
			targetPath = filepath.Join(absDest, filepath.Base(rel))
		} else {
			targetPath = absDest
		}
		if fi.IsDir() {
			if err := os.MkdirAll(targetPath, fi.Mode()); err != nil {
				return err
			}
			// Walk dir
			err = filepath.WalkDir(srcPath, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				relP, _ := filepath.Rel(srcPath, p)
				tp := filepath.Join(targetPath, relP)
				if d.IsDir() {
					return os.MkdirAll(tp, 0755)
				}
				if err := copyFileRaw(p, tp); err != nil {
					return err
				}
				newFiles = append(newFiles, tp)
				return nil
			})
			if err != nil {
				return err
			}
		} else {
			if err := copyFileRaw(srcPath, targetPath); err != nil {
				return err
			}
			newFiles = append(newFiles, targetPath)
		}
	}

	// Collect all files in tmpRoot for the tar
	allTmpFiles, err := collectFiles([]string{tmpRoot})
	if err != nil {
		return err
	}
	_ = newFiles

	tarData, err := buildTarFromDir(tmpRoot, allTmpFiles)
	if err != nil {
		return err
	}

	digest, err := writeLayer(e.layersDir, tarData)
	if err != nil {
		return err
	}

	// Update cache
	if !e.opts.NoCache {
		e.cacheIdx[cacheKey] = digest
	}

	e.layers = append(e.layers, LayerEntry{
		Digest:    digest,
		Size:      int64(len(tarData)),
		CreatedBy: instrText,
	})
	e.prevDigest = digest

	elapsed := time.Since(stepStart).Seconds()
	fmt.Printf("Step %d/%d : %s [CACHE MISS] %.2fs\n", step, total, instrText, elapsed)
	return nil
}

// execRUN handles the RUN instruction using OS-level isolation (see runtime.go).
func (e *BuildEngine) execRUN(instr Instruction, step, total int) error {
	stepStart := time.Now()
	instrText := instr.Kind + " " + instr.Args

	cacheKey := computeCacheKey(e.prevDigest, instrText, e.workdir, e.env, nil)

	// Cache lookup
	if !e.opts.NoCache && !e.cacheMiss {
		if layerDigest, ok := e.cacheIdx[cacheKey]; ok {
			if _, err := os.Stat(layerPath(e.layersDir, layerDigest)); err == nil {
				layerSize, _ := layerFileSize(e.layersDir, layerDigest)
				e.layers = append(e.layers, LayerEntry{
					Digest:    layerDigest,
					Size:      layerSize,
					CreatedBy: instrText,
				})
				e.prevDigest = layerDigest
				elapsed := time.Since(stepStart).Seconds()
				fmt.Printf("Step %d/%d : %s [CACHE HIT] %.2fs\n", step, total, instrText, elapsed)
				return nil
			}
		}
	}

	// Cache miss
	e.cacheMiss = true

	// Assemble full filesystem from current layers
	tmpRoot, err := os.MkdirTemp("", "docksmith-run-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot)

	// Extract base layers
	tmpManifest := &Manifest{Layers: e.layers}
	if err := extractImageLayers(tmpManifest, e.layersDir, tmpRoot); err != nil {
		return err
	}

	// Ensure workdir exists
	if e.workdir != "" {
		if err := os.MkdirAll(filepath.Join(tmpRoot, filepath.FromSlash(e.workdir)), 0755); err != nil {
			return err
		}
	}

	// Take snapshot before
	beforeSnap, err := snapshotDir(tmpRoot)
	if err != nil {
		return err
	}

	// Run command in isolation (defined in runtime.go)
	envList := buildEnvList(e.env)
	workdir := e.workdir
	if workdir == "" {
		workdir = "/"
	}
	if err := runInChroot(tmpRoot, instr.Args, envList, workdir); err != nil {
		return fmt.Errorf("RUN %q failed: %w", instr.Args, err)
	}

	// Take snapshot after, compute delta
	afterSnap, err := snapshotDir(tmpRoot)
	if err != nil {
		return err
	}
	deltaFiles := diffSnapshots(beforeSnap, afterSnap, tmpRoot)

	// Build delta tar
	var tarData []byte
	if len(deltaFiles) > 0 {
		tarData, err = buildTarFromDir(tmpRoot, deltaFiles)
		if err != nil {
			return err
		}
	} else {
		// Empty tar (still a valid layer)
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.Close()
		tarData = buf.Bytes()
	}

	digest, err := writeLayer(e.layersDir, tarData)
	if err != nil {
		return err
	}

	if !e.opts.NoCache {
		e.cacheIdx[cacheKey] = digest
	}

	e.layers = append(e.layers, LayerEntry{
		Digest:    digest,
		Size:      int64(len(tarData)),
		CreatedBy: instrText,
	})
	e.prevDigest = digest

	elapsed := time.Since(stepStart).Seconds()
	fmt.Printf("Step %d/%d : %s [CACHE MISS] %.2fs\n", step, total, instrText, elapsed)
	return nil
}

// ────────────────────────────────────────────────────────────
// 7.  HELPERS
// ────────────────────────────────────────────────────────────

func parseEnvArg(arg string) (key, val string, ok bool) {
	idx := strings.Index(arg, "=")
	if idx <= 0 {
		return "", "", false
	}
	return arg[:idx], arg[idx+1:], true
}

// ParseEnvArg is an exported wrapper used by runtime.
func ParseEnvArg(arg string) (key, val string, ok bool) {
	return parseEnvArg(arg)
}

func buildEnvList(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// BuildEnvList is an exported wrapper used by runtime.
func BuildEnvList(env map[string]string) []string {
	return buildEnvList(env)
}

func layerFileSize(layersDir, digest string) (int64, error) {
	fi, err := os.Stat(layerPath(layersDir, digest))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func copyFileRaw(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// snapshotDir returns a map of relPath -> modTime+size for all files under dir.
func snapshotDir(dir string) (map[string]string, error) {
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		snap[rel] = fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
		return nil
	})
	return snap, err
}

// diffSnapshots returns abs paths of files that are new or changed.
func diffSnapshots(before, after map[string]string, rootDir string) []string {
	var changed []string
	for rel, afterVal := range after {
		if beforeVal, exists := before[rel]; !exists || beforeVal != afterVal {
			changed = append(changed, filepath.Join(rootDir, rel))
		}
	}
	sort.Strings(changed)
	return changed
}

// runInChroot executes shellCmd inside rootDir using Linux namespaces and chroot.
func runInChroot(rootDir, shellCmd string, env []string, workdir string) error {
	if workdir == "" {
		workdir = "/"
	}

	setupMinimalDevProc(rootDir)

	procDir := filepath.Join(rootDir, "proc")
	_ = os.MkdirAll(procDir, 0755)
	_ = syscall.Mount("proc", procDir, "proc", 0, "")
	defer func() { _ = syscall.Unmount(procDir, syscall.MNT_DETACH) }()

	var exports []string
	for _, kv := range env {
		idx := strings.Index(kv, "=")
		if idx > 0 {
			k := kv[:idx]
			v := kv[idx+1:]
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
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Dir = workdir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func setupMinimalDevProc(rootDir string) {
	for _, d := range []string{"proc", "dev", "sys", "tmp", "etc"} {
		_ = os.MkdirAll(filepath.Join(rootDir, d), 0755)
	}

	resolvPath := filepath.Join(rootDir, "etc", "resolv.conf")
	if _, err := os.Stat(resolvPath); os.IsNotExist(err) {
		_ = os.WriteFile(resolvPath, []byte(""), 0644)
	}
}

