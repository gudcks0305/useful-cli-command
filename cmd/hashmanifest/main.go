package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	manifestVersion = 1
	defaultMaxFiles = 100_000
	maxManifestLine = 4 << 20
)

type manifestRecord struct {
	Type      string `json:"type"`
	Version   int    `json:"version,omitempty"`
	Algorithm string `json:"algorithm,omitempty"`
	Path      string `json:"path,omitempty"`
	Size      *int64 `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

type createOptions struct {
	root     string
	output   string
	maxFiles int
}

var publishOutput = publishNoReplace

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "create":
		return runCreate(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  hashmanifest create [--output FILE] [--max-files N] [root]")
	fmt.Fprintln(w, "  hashmanifest verify [--root DIR] [manifest]")
}

func runCreate(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("create", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.Usage = func() {
		fmt.Fprintln(stderr, "usage: hashmanifest create [--output FILE] [--max-files N] [root]")
		set.PrintDefaults()
	}
	output := set.String("output", "", "write manifest to FILE (must not exist)")
	maxFiles := set.Int("max-files", defaultMaxFiles, "maximum number of files to hash")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if set.NArg() > 1 || *maxFiles <= 0 {
		fmt.Fprintln(stderr, "create requires at most one root and --max-files must be positive")
		return 2
	}
	root := "."
	if set.NArg() == 1 {
		root = set.Arg(0)
	}

	return createManifest(createOptions{root: root, output: *output, maxFiles: *maxFiles}, stdout, stderr)
}

func createManifest(opts createOptions, stdout, stderr io.Writer) int {
	root, err := filepath.Abs(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 2
	}
	info, err := os.Lstat(root)
	if err != nil {
		fmt.Fprintf(stderr, "inspect root: %v\n", err)
		return 2
	}
	if !info.IsDir() {
		fmt.Fprintln(stderr, "root is not a directory")
		return 2
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		fmt.Fprintf(stderr, "canonicalize root: %v\n", err)
		return 2
	}

	out := stdout
	var temp *os.File
	var outputPath, tempPath string
	if opts.output != "" {
		outputPath, err = canonicalNewPath(opts.output)
		if err != nil {
			fmt.Fprintf(stderr, "resolve output: %v\n", err)
			return 2
		}
		if _, err = os.Lstat(outputPath); err == nil {
			fmt.Fprintf(stderr, "output already exists: %s\n", outputPath)
			return 2
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "inspect output: %v\n", err)
			return 2
		}
		temp, err = os.CreateTemp(filepath.Dir(outputPath), ".hashmanifest-*.tmp")
		if err != nil {
			fmt.Fprintf(stderr, "create temporary output: %v\n", err)
			return 2
		}
		tempPath = temp.Name()
		if err := temp.Chmod(0o600); err != nil {
			temp.Close()
			os.Remove(tempPath)
			fmt.Fprintf(stderr, "set temporary output permissions: %v\n", err)
			return 2
		}
		out = temp
		defer func() {
			temp.Close()
			os.Remove(tempPath)
		}()
	}

	partial, err := writeManifest(root, outputPath, tempPath, opts.maxFiles, out, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "create manifest: %v\n", err)
		return 2
	}
	if temp != nil {
		if err := temp.Sync(); err != nil {
			fmt.Fprintf(stderr, "sync output: %v\n", err)
			return 2
		}
		if err := temp.Close(); err != nil {
			fmt.Fprintf(stderr, "close output: %v\n", err)
			return 2
		}
		if err := publishOutput(tempPath, outputPath); err != nil {
			fmt.Fprintf(stderr, "publish output: %v\n", err)
			return 2
		}
	}
	if partial {
		return 3
	}
	return 0
}

// publishNoReplace atomically creates outputPath as another name for the
// already-synced temporary file. Link fails if outputPath exists, so publication
// can never overwrite a file created by a competing process. Both names are in
// the same directory and therefore on the same filesystem.
func publishNoReplace(tempPath, outputPath string) error {
	if err := os.Link(tempPath, outputPath); err != nil {
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove published temporary file: %w", err)
	}
	return nil
}

func writeManifest(root, outputPath, tempPath string, maxFiles int, out, stderr io.Writer) (partialResult bool, retErr error) {
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return false, fmt.Errorf("open root handle: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, rootHandle.Close())
	}()

	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(manifestRecord{Type: "header", Version: manifestVersion, Algorithm: "sha256"}); err != nil {
		return false, err
	}

	excluded := map[string]bool{}
	for _, path := range []string{outputPath, tempPath} {
		if path != "" {
			excluded[filepath.Clean(path)] = true
		}
	}
	count := 0
	partial := false
	errMaxFiles := errors.New("maximum file count reached")
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		cleanPath := filepath.Clean(path)
		if excluded[cleanPath] {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, cleanPath)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if walkErr != nil {
			partial = true
			fmt.Fprintf(stderr, "unreadable %s: %v\n", strconv.Quote(rel), walkErr)
			return enc.Encode(manifestRecord{Type: "issue", Path: rel, Status: "unreadable", Error: walkErr.Error()})
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := os.Lstat(cleanPath)
		if err != nil {
			partial = true
			fmt.Fprintf(stderr, "unreadable %s: %v\n", strconv.Quote(rel), err)
			return enc.Encode(manifestRecord{Type: "issue", Path: rel, Status: "unreadable", Error: err.Error()})
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if count >= maxFiles {
			partial = true
			return errMaxFiles
		}
		count++

		digest, changed, err := hashNoSymlinkPath(rootHandle, filepath.FromSlash(rel), info)
		if err != nil {
			partial = true
			fmt.Fprintf(stderr, "unreadable %s: %v\n", strconv.Quote(rel), err)
			return enc.Encode(manifestRecord{Type: "issue", Path: rel, Status: "unreadable", Error: err.Error()})
		}
		status := "ok"
		if changed {
			status = "changed"
			partial = true
			fmt.Fprintf(stderr, "changed while hashing %s\n", strconv.Quote(rel))
		}
		if digest == "" {
			return enc.Encode(manifestRecord{Type: "issue", Path: rel, Status: status, Error: "file identity changed before hashing"})
		}
		size := info.Size()
		return enc.Encode(manifestRecord{Type: "file", Path: rel, Size: &size, SHA256: digest, Status: status})
	})
	if errors.Is(err, errMaxFiles) {
		fmt.Fprintf(stderr, "maximum file count reached (%d)\n", maxFiles)
		if encodeErr := enc.Encode(manifestRecord{Type: "issue", Status: "limit", Error: err.Error()}); encodeErr != nil {
			return true, encodeErr
		}
		return true, nil
	}
	return partial, err
}

func hashAndRestat(root *os.Root, path string, expected fs.FileInfo) (string, bool, error) {
	file, err := root.Open(path)
	if err != nil {
		return "", false, err
	}
	opened, err := file.Stat()
	if err != nil {
		return "", false, errors.Join(err, file.Close())
	}
	if !sameRegularSnapshot(expected, opened) {
		return "", true, file.Close()
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	final, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(copyErr, statErr, closeErr); err != nil {
		return "", false, err
	}
	after, err := root.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hex.EncodeToString(hash.Sum(nil)), true, nil
		}
		return "", false, err
	}
	changed := !sameRegularSnapshot(opened, final) || !sameRegularSnapshot(final, after)
	return hex.EncodeToString(hash.Sum(nil)), changed, nil
}

func sameRegularSnapshot(first, second fs.FileInfo) bool {
	return first != nil && second != nil &&
		first.Mode().IsRegular() && second.Mode().IsRegular() &&
		first.Mode() == second.Mode() && first.Size() == second.Size() &&
		first.ModTime().Equal(second.ModTime()) && os.SameFile(first, second)
}

func hashNoSymlinkPath(root *os.Root, path string, expected fs.FileInfo) (string, bool, error) {
	return hashNoSymlinkPathWithHook(root, path, expected, nil)
}

func hashNoSymlinkPathWithHook(root *os.Root, path string, expected fs.FileInfo, afterParentOpen func()) (string, bool, error) {
	parent, name, owned, err := openNoSymlinkParent(root, path)
	if err != nil {
		return "", false, err
	}
	parentInfo, err := parent.Lstat(".")
	if err != nil {
		if owned {
			err = errors.Join(err, parent.Close())
		}
		return "", false, err
	}
	if afterParentOpen != nil {
		afterParentOpen()
	}
	digest, changed, hashErr := hashAndRestat(parent, name, expected)
	if owned {
		hashErr = errors.Join(hashErr, parent.Close())
	}
	if hashErr != nil {
		return "", false, hashErr
	}

	freshParent, freshName, freshOwned, freshErr := openNoSymlinkParent(root, path)
	if freshErr != nil {
		return digest, true, nil
	}
	freshParentInfo, parentErr := freshParent.Lstat(".")
	freshFileInfo, fileErr := freshParent.Lstat(freshName)
	if freshOwned {
		freshErr = freshParent.Close()
	}
	if err := errors.Join(parentErr, fileErr, freshErr); err != nil {
		return "", false, err
	}
	if !parentInfo.IsDir() || !freshParentInfo.IsDir() ||
		!os.SameFile(parentInfo, freshParentInfo) || !sameRegularSnapshot(expected, freshFileInfo) {
		changed = true
	}
	return digest, changed, nil
}

func openNoSymlinkParent(root *os.Root, path string) (*os.Root, string, bool, error) {
	clean := filepath.Clean(path)
	if !filepath.IsLocal(clean) || clean == "." {
		return nil, "", false, errors.New("path is not a local file path")
	}
	components := strings.Split(clean, string(filepath.Separator))
	current := root
	owned := false
	for _, component := range components[:len(components)-1] {
		if component == "" || component == "." || component == ".." {
			if owned {
				_ = current.Close()
			}
			return nil, "", false, errors.New("invalid path component")
		}
		expected, err := current.Lstat(component)
		if err != nil || expected.Mode()&os.ModeSymlink != 0 || !expected.IsDir() {
			if owned {
				_ = current.Close()
			}
			if err != nil {
				return nil, "", false, err
			}
			return nil, "", false, errors.New("path contains a symlink or non-directory component")
		}
		child, err := current.OpenRoot(component)
		if err != nil {
			if owned {
				_ = current.Close()
			}
			return nil, "", false, err
		}
		opened, openedErr := child.Lstat(".")
		currentInfo, currentErr := current.Lstat(component)
		if err := errors.Join(openedErr, currentErr); err != nil {
			_ = child.Close()
			if owned {
				_ = current.Close()
			}
			return nil, "", false, err
		}
		if currentInfo.Mode()&os.ModeSymlink != 0 || !currentInfo.IsDir() ||
			!os.SameFile(expected, opened) || !os.SameFile(opened, currentInfo) {
			_ = child.Close()
			if owned {
				_ = current.Close()
			}
			return nil, "", false, errors.New("directory component changed while opening")
		}
		if owned {
			if err := current.Close(); err != nil {
				_ = child.Close()
				return nil, "", false, err
			}
		}
		current = child
		owned = true
	}
	name := components[len(components)-1]
	if name == "" || name == "." || name == ".." {
		if owned {
			_ = current.Close()
		}
		return nil, "", false, errors.New("invalid file name")
	}
	return current, name, owned, nil
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("verify", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.Usage = func() {
		fmt.Fprintln(stderr, "usage: hashmanifest verify [--root DIR] [manifest]")
		set.PrintDefaults()
	}
	rootFlag := set.String("root", ".", "root directory for manifest paths")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if set.NArg() > 1 {
		fmt.Fprintln(stderr, "verify requires at most one manifest")
		return 2
	}

	input := io.Reader(os.Stdin)
	var file *os.File
	if set.NArg() == 1 && set.Arg(0) != "-" {
		var err error
		file, err = os.Open(set.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "open manifest: %v\n", err)
			return 2
		}
		defer file.Close()
		input = file
	}
	return verifyManifest(*rootFlag, input, stdout, stderr)
}

func verifyManifest(root string, input io.Reader, stdout, stderr io.Writer) int {
	canonicalRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 2
	}
	canonicalRoot, err = filepath.EvalSymlinks(canonicalRoot)
	if err != nil {
		fmt.Fprintf(stderr, "canonicalize root: %v\n", err)
		return 2
	}
	rootInfo, err := os.Lstat(canonicalRoot)
	if err != nil || !rootInfo.IsDir() {
		fmt.Fprintln(stderr, "root is not a readable directory")
		return 2
	}
	rootHandle, err := os.OpenRoot(canonicalRoot)
	if err != nil {
		fmt.Fprintf(stderr, "open root handle: %v\n", err)
		return 2
	}
	defer rootHandle.Close()

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), maxManifestLine)
	line := 0
	seen := make(map[string]bool)
	partial := false
	mismatch := false
	for scanner.Scan() {
		line++
		if len(scanner.Bytes()) == 0 {
			fmt.Fprintf(stderr, "manifest line %d is empty\n", line)
			return 2
		}
		var record manifestRecord
		if err := strictUnmarshal(scanner.Bytes(), &record); err != nil {
			fmt.Fprintf(stderr, "manifest line %d: %v\n", line, err)
			return 2
		}
		if line == 1 {
			if !validHeader(record) {
				fmt.Fprintln(stderr, "unsupported or invalid manifest header")
				return 2
			}
			continue
		}
		if record.Type == "issue" {
			partial = true
			fmt.Fprintf(stdout, "PARTIAL %s\n", strconv.Quote(record.Path))
			continue
		}
		if record.Type != "file" || record.Version != 0 || record.Algorithm != "" || record.Error != "" || (record.Status != "ok" && record.Status != "changed") || record.Size == nil || *record.Size < 0 || !validDigest(record.SHA256) {
			fmt.Fprintf(stderr, "invalid file record on line %d\n", line)
			return 2
		}
		localPath := filepath.FromSlash(record.Path)
		if record.Path == "" || strings.ContainsRune(record.Path, '\x00') || !filepath.IsLocal(localPath) || filepath.ToSlash(filepath.Clean(localPath)) != record.Path {
			fmt.Fprintf(stderr, "unsafe manifest path on line %d: %s\n", line, strconv.Quote(record.Path))
			return 2
		}
		if seen[record.Path] {
			fmt.Fprintf(stderr, "duplicate manifest path on line %d: %s\n", line, strconv.Quote(record.Path))
			return 2
		}
		seen[record.Path] = true
		target := filepath.Join(canonicalRoot, filepath.FromSlash(record.Path))
		if !withinRoot(canonicalRoot, target) {
			fmt.Fprintf(stderr, "manifest path escapes root on line %d\n", line)
			return 2
		}
		if record.Status == "changed" {
			partial = true
			fmt.Fprintf(stdout, "CHANGED %s\n", strconv.Quote(record.Path))
			continue
		}

		result := verifyFile(rootHandle, filepath.FromSlash(record.Path), record)
		fmt.Fprintf(stdout, "%s %s\n", result, strconv.Quote(record.Path))
		switch result {
		case "MATCH":
		case "MISMATCH", "MISSING":
			mismatch = true
		default:
			partial = true
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "read manifest: %v\n", err)
		return 2
	}
	if line == 0 {
		fmt.Fprintln(stderr, "manifest is empty")
		return 2
	}
	if partial {
		return 3
	}
	if mismatch {
		return 1
	}
	return 0
}

func strictUnmarshal(data []byte, dst any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple JSON values")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validHeader(record manifestRecord) bool {
	return record.Type == "header" &&
		record.Version == manifestVersion &&
		record.Algorithm == "sha256" &&
		record.Path == "" && record.Size == nil && record.SHA256 == "" &&
		record.Status == "" && record.Error == ""
}

func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && filepath.IsLocal(rel)
}

func verifyFile(root *os.Root, target string, expected manifestRecord) string {
	parent, name, owned, err := openNoSymlinkParent(root, target)
	if err != nil {
		return "UNREADABLE"
	}
	before, err := parent.Lstat(name)
	if owned {
		err = errors.Join(err, parent.Close())
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "MISSING"
		}
		return "UNREADABLE"
	}
	if !before.Mode().IsRegular() {
		return "UNREADABLE"
	}
	digest, changed, err := hashNoSymlinkPath(root, target, before)
	if err != nil {
		return "UNREADABLE"
	}
	if changed {
		return "CHANGED"
	}
	if before.Size() != *expected.Size || digest != expected.SHA256 {
		return "MISMATCH"
	}
	return "MATCH"
}

func canonicalNewPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}
