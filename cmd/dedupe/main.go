// Command dedupe reports exact duplicate files without modifying them.
//
// It deliberately skips .git directories and symbolic links. The default
// limits are 100,000 files and 64 directory levels; hitting either limit makes
// the result partial and returns exit code 3.
package main

import (
	"bytes"
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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/useful-go/pkg/text"
)

const (
	defaultMaxFiles = 100_000
	defaultMaxDepth = 64
	maxWorkers      = 256
)

type options struct {
	json     bool
	minSize  int64
	maxFiles int
	maxDepth int
	workers  int
	root     string
}

type fileRecord struct {
	path    string
	size    int64
	modTime int64
	info    os.FileInfo
	hash    [sha256.Size]byte
}

type issue struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type duplicateGroup struct {
	Size             int64    `json:"size"`
	ReclaimableBytes int64    `json:"reclaimable_bytes"`
	Files            []string `json:"files"`
}

type report struct {
	Root            string           `json:"root"`
	Partial         bool             `json:"partial"`
	ScannedFiles    int              `json:"scanned_files"`
	HashedFiles     int              `json:"hashed_files"`
	SkippedSymlinks int              `json:"skipped_symlinks"`
	ExcludedGitDirs int              `json:"excluded_git_dirs"`
	DuplicateGroups []duplicateGroup `json:"duplicate_groups"`
	Errors          []issue          `json:"errors"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	result, fatalErr := scan(opts)
	if fatalErr != nil {
		fmt.Fprintf(stderr, "dedupe: %v\n", fatalErr)
		return 2
	}

	if opts.json {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			fmt.Fprintf(stderr, "dedupe: write JSON: %v\n", err)
			return 2
		}
	} else {
		writeTextReport(stdout, result)
	}

	if result.Partial {
		return 3
	}
	if len(result.DuplicateGroups) > 0 {
		return 1
	}
	return 0
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}

	var opts options
	var minSize string
	flags := flag.NewFlagSet("dedupe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.BoolVar(&opts.json, "json", false, "emit deterministic JSON")
	flags.StringVar(&minSize, "min-size", "1B", "minimum file size (for example 4KB, 2MiB, 1GB)")
	flags.IntVar(&opts.maxFiles, "max-files", defaultMaxFiles, "maximum regular files to inspect")
	flags.IntVar(&opts.maxDepth, "max-depth", defaultMaxDepth, "maximum directory depth below root")
	flags.IntVar(&opts.workers, "workers", workers, "hash worker count (1-256)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: dedupe [--json] [--min-size SIZE] [--max-files N] [--max-depth N] [--workers N] [root]")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return options{}, errors.New("too many roots")
	}
	if flags.NArg() == 1 {
		opts.root = flags.Arg(0)
	} else {
		opts.root = "."
	}
	if opts.maxFiles < 1 {
		fmt.Fprintln(stderr, "dedupe: --max-files must be at least 1")
		return options{}, errors.New("invalid max files")
	}
	if opts.maxDepth < 0 {
		fmt.Fprintln(stderr, "dedupe: --max-depth must be at least 0")
		return options{}, errors.New("invalid max depth")
	}
	if opts.workers < 1 || opts.workers > maxWorkers {
		fmt.Fprintf(stderr, "dedupe: --workers must be between 1 and %d\n", maxWorkers)
		return options{}, errors.New("invalid workers")
	}

	parsedSize, err := text.ParseSizeE(minSize)
	if err != nil {
		fmt.Fprintf(stderr, "dedupe: invalid --min-size %q: %v\n", minSize, err)
		return options{}, err
	}
	opts.minSize = parsedSize
	return opts, nil
}

func canonicalRoot(root string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Lstat(absRoot)
	if err != nil {
		return "", fmt.Errorf("inspect root %q: %w", absRoot, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("root must not be a symbolic link: %q", absRoot)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root is not a directory: %q", absRoot)
	}
	canonical, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("canonicalize root %q: %w", absRoot, err)
	}
	return filepath.Clean(canonical), nil
}

func scan(opts options) (report, error) {
	root, err := canonicalRoot(opts.root)
	if err != nil {
		return report{}, err
	}
	result := report{
		Root:            root,
		DuplicateGroups: make([]duplicateGroup, 0),
		Errors:          make([]issue, 0),
	}

	files := make([]fileRecord, 0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Partial = true
			result.Errors = append(result.Errors, issue{Path: path, Error: walkErr.Error()})
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				result.ExcludedGitDirs++
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				result.Partial = true
				result.Errors = append(result.Errors, issue{Path: path, Error: relErr.Error()})
				return filepath.SkipDir
			}
			depth := strings.Count(rel, string(filepath.Separator)) + 1
			if depth > opts.maxDepth {
				result.Partial = true
				result.Errors = append(result.Errors, issue{Path: path, Error: "maximum depth reached"})
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			result.SkippedSymlinks++
			return nil
		}

		info, statErr := os.Lstat(path)
		if statErr != nil {
			result.Partial = true
			result.Errors = append(result.Errors, issue{Path: path, Error: statErr.Error()})
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			result.SkippedSymlinks++
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if result.ScannedFiles == opts.maxFiles {
			result.Partial = true
			result.Errors = append(result.Errors, issue{Path: path, Error: "maximum file count reached"})
			return fs.SkipAll
		}
		result.ScannedFiles++
		if info.Size() >= opts.minSize {
			files = append(files, fileRecord{
				path:    path,
				size:    info.Size(),
				modTime: info.ModTime().UnixNano(),
				info:    info,
			})
		}
		return nil
	})
	if err != nil {
		result.Partial = true
		result.Errors = append(result.Errors, issue{Path: root, Error: err.Error()})
	}

	sizeBuckets := make(map[int64][]fileRecord)
	for _, file := range files {
		sizeBuckets[file.size] = append(sizeBuckets[file.size], file)
	}
	candidates := make([]fileRecord, 0)
	for _, bucket := range sizeBuckets {
		if len(bucket) > 1 {
			candidates = append(candidates, bucket...)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].path < candidates[j].path })

	hashed, hashIssues := hashFiles(candidates, opts.workers)
	result.HashedFiles = len(hashed)
	if len(hashIssues) > 0 {
		result.Partial = true
		result.Errors = append(result.Errors, hashIssues...)
	}

	hashBuckets := make(map[string][]fileRecord)
	for _, file := range hashed {
		key := strconv.FormatInt(file.size, 10) + ":" + hex.EncodeToString(file.hash[:])
		hashBuckets[key] = append(hashBuckets[key], file)
	}
	keys := make([]string, 0, len(hashBuckets))
	for key, bucket := range hashBuckets {
		if len(bucket) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	for _, key := range keys {
		bucket := hashBuckets[key]
		sort.Slice(bucket, func(i, j int) bool { return bucket[i].path < bucket[j].path })
		verified, verifyIssues := verifyBucket(bucket)
		if len(verifyIssues) > 0 {
			result.Partial = true
			result.Errors = append(result.Errors, verifyIssues...)
		}
		for _, group := range verified {
			if len(group) < 2 {
				continue
			}
			if snapshotIssues := validateGroupSnapshots(group); len(snapshotIssues) > 0 {
				result.Partial = true
				result.Errors = append(result.Errors, snapshotIssues...)
				continue
			}
			paths := make([]string, len(group))
			for i := range group {
				paths[i] = group[i].path
			}
			result.DuplicateGroups = append(result.DuplicateGroups, duplicateGroup{
				Size:             group[0].size,
				ReclaimableBytes: reclaimableBytes(group),
				Files:            paths,
			})
		}
	}

	sort.Slice(result.DuplicateGroups, func(i, j int) bool {
		return result.DuplicateGroups[i].Files[0] < result.DuplicateGroups[j].Files[0]
	})
	sort.Slice(result.Errors, func(i, j int) bool {
		if result.Errors[i].Path == result.Errors[j].Path {
			return result.Errors[i].Error < result.Errors[j].Error
		}
		return result.Errors[i].Path < result.Errors[j].Path
	})
	return result, nil
}

type hashResult struct {
	index int
	file  fileRecord
	err   error
}

func hashFiles(files []fileRecord, workers int) ([]fileRecord, []issue) {
	if len(files) == 0 {
		return nil, nil
	}
	if workers > len(files) {
		workers = len(files)
	}
	jobs := make(chan int)
	results := make(chan hashResult, len(files))
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				file := files[index]
				hash, info, err := hashFile(file)
				file.hash = hash
				file.info = info
				results <- hashResult{index: index, file: file, err: err}
			}
		}()
	}
	go func() {
		for index := range files {
			jobs <- index
		}
		close(jobs)
		wait.Wait()
		close(results)
	}()

	ordered := make([]hashResult, len(files))
	for result := range results {
		ordered[result.index] = result
	}
	hashed := make([]fileRecord, 0, len(files))
	issues := make([]issue, 0)
	for _, result := range ordered {
		if result.err != nil {
			issues = append(issues, issue{Path: result.file.path, Error: result.err.Error()})
			continue
		}
		hashed = append(hashed, result.file)
	}
	return hashed, issues
}

func hashFile(file fileRecord) ([sha256.Size]byte, os.FileInfo, error) {
	var zero [sha256.Size]byte
	before, err := os.Lstat(file.path)
	if err != nil {
		return zero, nil, err
	}
	if !sameSnapshot(file, before) {
		return zero, nil, errors.New("file changed before hashing")
	}
	handle, err := os.Open(file.path)
	if err != nil {
		return zero, nil, err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, handle)
	closeErr := handle.Close()
	if copyErr != nil {
		return zero, nil, copyErr
	}
	if closeErr != nil {
		return zero, nil, closeErr
	}
	after, err := os.Lstat(file.path)
	if err != nil {
		return zero, nil, err
	}
	if !sameSnapshot(file, after) {
		return zero, nil, errors.New("file changed while hashing")
	}
	var sum [sha256.Size]byte
	copy(sum[:], hasher.Sum(nil))
	return sum, after, nil
}

func sameSnapshot(file fileRecord, current os.FileInfo) bool {
	return current.Mode().IsRegular() &&
		current.Size() == file.size &&
		current.ModTime().UnixNano() == file.modTime &&
		os.SameFile(file.info, current)
}

func verifyBucket(bucket []fileRecord) ([][]fileRecord, []issue) {
	groups := make([][]fileRecord, 0)
	issues := make([]issue, 0)
	for _, candidate := range bucket {
		matched := false
		failed := false
		for i := range groups {
			equal, err := filesEqual(groups[i][0], candidate)
			if err != nil {
				issues = append(issues, issue{Path: candidate.path, Error: err.Error()})
				failed = true
				break
			}
			if equal {
				groups[i] = append(groups[i], candidate)
				matched = true
				break
			}
		}
		if !matched && !failed {
			groups = append(groups, []fileRecord{candidate})
		}
	}
	return groups, issues
}

func filesEqual(left, right fileRecord) (bool, error) {
	leftBefore, err := os.Lstat(left.path)
	if err != nil {
		return false, fmt.Errorf("verify %q: %w", left.path, err)
	}
	if !sameSnapshot(left, leftBefore) {
		return false, fmt.Errorf("file changed before verification: %q", left.path)
	}
	rightBefore, err := os.Lstat(right.path)
	if err != nil {
		return false, fmt.Errorf("verify %q: %w", right.path, err)
	}
	if !sameSnapshot(right, rightBefore) {
		return false, fmt.Errorf("file changed before verification: %q", right.path)
	}

	leftFile, err := os.Open(left.path)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", left.path, err)
	}
	defer leftFile.Close()
	rightFile, err := os.Open(right.path)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", right.path, err)
	}
	defer rightFile.Close()

	leftBuffer := make([]byte, 64*1024)
	rightBuffer := make([]byte, 64*1024)
	for {
		leftN, leftErr := io.ReadFull(leftFile, leftBuffer)
		rightN, rightErr := io.ReadFull(rightFile, rightBuffer)
		if leftN != rightN || !bytes.Equal(leftBuffer[:leftN], rightBuffer[:rightN]) {
			return false, nil
		}
		leftDone := errors.Is(leftErr, io.EOF) || errors.Is(leftErr, io.ErrUnexpectedEOF)
		rightDone := errors.Is(rightErr, io.EOF) || errors.Is(rightErr, io.ErrUnexpectedEOF)
		if leftErr != nil && !leftDone {
			return false, fmt.Errorf("read %q: %w", left.path, leftErr)
		}
		if rightErr != nil && !rightDone {
			return false, fmt.Errorf("read %q: %w", right.path, rightErr)
		}
		if leftDone || rightDone {
			if leftDone != rightDone {
				return false, nil
			}
			break
		}
	}

	leftAfter, err := os.Lstat(left.path)
	if err != nil || !sameSnapshot(left, leftAfter) {
		return false, fmt.Errorf("file changed while verifying: %q", left.path)
	}
	rightAfter, err := os.Lstat(right.path)
	if err != nil || !sameSnapshot(right, rightAfter) {
		return false, fmt.Errorf("file changed while verifying: %q", right.path)
	}
	return true, nil
}

func validateGroupSnapshots(group []fileRecord) []issue {
	issues := make([]issue, 0)
	for _, file := range group {
		current, err := os.Lstat(file.path)
		if err != nil {
			issues = append(issues, issue{Path: file.path, Error: err.Error()})
			continue
		}
		if !sameSnapshot(file, current) {
			issues = append(issues, issue{Path: file.path, Error: "file changed after verification"})
		}
	}
	return issues
}

func reclaimableBytes(group []fileRecord) int64 {
	unique := make([]os.FileInfo, 0, len(group))
	for _, file := range group {
		seen := false
		for _, info := range unique {
			if os.SameFile(file.info, info) {
				seen = true
				break
			}
		}
		if !seen {
			unique = append(unique, file.info)
		}
	}
	if len(unique) < 2 {
		return 0
	}
	return int64(len(unique)-1) * group[0].size
}

func writeTextReport(writer io.Writer, result report) {
	fmt.Fprintf(writer, "Root: %s\n", result.Root)
	fmt.Fprintf(writer, "Scanned files: %d (hashed: %d)\n", result.ScannedFiles, result.HashedFiles)
	for index, group := range result.DuplicateGroups {
		fmt.Fprintf(writer, "\nGroup %d: %d bytes each, %d reclaimable bytes\n", index+1, group.Size, group.ReclaimableBytes)
		for _, path := range group.Files {
			fmt.Fprintf(writer, "  %s\n", path)
		}
	}
	fmt.Fprintf(writer, "\nDuplicate groups: %d\n", len(result.DuplicateGroups))
	fmt.Fprintf(writer, "Skipped symlinks: %d; excluded .git directories: %d\n", result.SkippedSymlinks, result.ExcludedGitDirs)
	if result.Partial {
		fmt.Fprintln(writer, "PARTIAL: scan or verification did not cover every file")
	}
	for _, issue := range result.Errors {
		fmt.Fprintf(writer, "ERROR: %s: %s\n", issue.Path, issue.Error)
	}
}
