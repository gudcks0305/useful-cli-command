// Command bintrace inspects binary paths without running them.
package main

import (
	"crypto/sha256"
	"debug/buildinfo"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

const (
	statusOK            = "ok"
	statusNotFound      = "not_found"
	statusPermission    = "permission_error"
	statusBrokenSymlink = "broken_symlink"
	statusNotExecutable = "not_executable"
	statusError         = "error"
)

var errHelpRequested = errors.New("help requested")
var errCandidateChanged = errors.New("candidate changed while inspecting")

// TraceReport is the complete result for one requested command or path.
type TraceReport struct {
	RequestedPath string            `json:"requested_path"`
	Status        string            `json:"status"`
	Candidates    []CandidateReport `json:"candidates"`
	Error         string            `json:"error,omitempty"`
}

// CandidateReport contains metadata for one PATH candidate or direct path.
type CandidateReport struct {
	CandidatePath       string             `json:"candidate_path"`
	Active              bool               `json:"active"`
	Executable          bool               `json:"executable"`
	Status              string             `json:"status"`
	SymlinkResolvedPath string             `json:"symlink_resolved_path,omitempty"`
	SHA256              string             `json:"sha256,omitempty"`
	Size                int64              `json:"size,omitempty"`
	MTime               string             `json:"mtime,omitempty"`
	Mode                string             `json:"mode,omitempty"`
	ModeOctal           string             `json:"mode_octal,omitempty"`
	MachOArchitectures  []string           `json:"macho_architectures,omitempty"`
	GoBuildInfo         *GoBuildInfoReport `json:"go_build_info,omitempty"`
	Error               string             `json:"error,omitempty"`
}

// GoBuildInfoReport is the safe, structured subset of debug/buildinfo output.
// Build settings containing compiler flags or environment paths are omitted.
type GoBuildInfoReport struct {
	GoVersion string           `json:"go_version,omitempty"`
	Path      string           `json:"path,omitempty"`
	Main      GoModule         `json:"main"`
	Deps      []GoModule       `json:"deps,omitempty"`
	Settings  []GoBuildSetting `json:"settings,omitempty"`
}

type GoModule struct {
	Path    string    `json:"path,omitempty"`
	Version string    `json:"version,omitempty"`
	Sum     string    `json:"sum,omitempty"`
	Replace *GoModule `json:"replace,omitempty"`
}

type GoBuildSetting struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses arguments, writes exactly one report to stdout, and returns an
// exit status. It never invokes the requested command.
func run(args []string, stdout, stderr io.Writer) int {
	jsonOutput, target, err := parseArgs(args)
	if err != nil {
		if errors.Is(err, errHelpRequested) {
			printUsage(stdout)
			return 0
		}
		fmt.Fprintln(stderr, err)
		printUsage(stderr)
		return 2
	}

	report := traceTarget(target, os.Getenv("PATH"))
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			fmt.Fprintf(stderr, "bintrace: output failed: %v\n", err)
			return 1
		}
	} else {
		printText(stdout, report)
	}
	if report.Status != statusOK {
		return 1
	}
	return 0
}

func parseArgs(args []string) (jsonOutput bool, target string, err error) {
	for _, arg := range args {
		switch {
		case arg == "--json" && target == "":
			jsonOutput = true
		case arg == "--help" || arg == "-h":
			return false, "", errHelpRequested
		case target == "":
			target = arg
		default:
			return false, "", fmt.Errorf("expected one command or path, got extra argument %q", arg)
		}
	}
	if target == "" {
		return false, "", errors.New("command or path is required")
	}
	return jsonOutput, target, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: bintrace [--json] <command-or-path>")
}

func traceTarget(target, pathValue string) TraceReport {
	report := TraceReport{
		RequestedPath: target,
		Candidates:    []CandidateReport{},
	}
	if isPathArgument(target) {
		candidate := inspectCandidate(target, true)
		report.Candidates = append(report.Candidates, candidate)
		report.Status = candidate.Status
		report.Error = candidate.Error
		return report
	}

	report.Candidates = discoverCommandCandidates(target, pathValue)
	if len(report.Candidates) == 0 {
		report.Status = statusNotFound
		report.Error = "no executable candidate found in PATH"
		return report
	}

	report.Status = statusNotFound
	for _, candidate := range report.Candidates {
		if candidate.Active {
			report.Status = candidate.Status
			report.Error = candidate.Error
			break
		}
	}
	if report.Status == statusNotFound {
		report.Status = report.Candidates[0].Status
		report.Error = report.Candidates[0].Error
	}
	return report
}

func isPathArgument(target string) bool {
	return strings.ContainsRune(target, '/') || strings.ContainsRune(target, filepath.Separator)
}

// discoverCommandCandidates follows PATH order and retains duplicate PATH
// entries. The active executable is moved to the first result; all other
// executable candidates retain their original order. Broken or inaccessible
// entries are retained as diagnostics when they prevent resolution.
func discoverCommandCandidates(command, pathValue string) []CandidateReport {
	var executable []CandidateReport
	var diagnostics []CandidateReport
	for _, directory := range splitPath(pathValue) {
		candidatePath := filepath.Join(directory, command)
		include, diagnostic := executableCandidate(candidatePath)
		if diagnostic != nil {
			diagnostics = append(diagnostics, *diagnostic)
			continue
		}
		if !include {
			continue
		}
		executable = append(executable, inspectCandidate(candidatePath, false))
	}

	if len(executable) > 0 {
		executable[0].Active = true
		return append(executable, diagnostics...)
	}
	return diagnostics
}

func splitPath(pathValue string) []string {
	parts := strings.Split(pathValue, string(os.PathListSeparator))
	if len(parts) == 0 {
		return []string{"."}
	}
	for i, part := range parts {
		if part == "" {
			parts[i] = "."
		}
	}
	return parts
}

// executableCandidate performs the inexpensive PATH filter. It returns a
// diagnostic for broken symlinks and path access errors, while silently
// filtering non-files and non-executable files like exec.LookPath does.
func executableCandidate(path string) (bool, *CandidateReport) {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		if statusForError(err) == statusNotFound {
			return false, nil
		}
		return false, diagnosticCandidate(path, statusForError(err), err)
	}

	info, err := os.Stat(path)
	if err != nil {
		status := statusForError(err)
		if linkInfo.Mode()&os.ModeSymlink != 0 && status == statusNotFound {
			status = statusBrokenSymlink
		}
		return false, diagnosticCandidate(path, status, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return false, nil
	}
	return true, nil
}

func diagnosticCandidate(path, status string, err error) *CandidateReport {
	return &CandidateReport{
		CandidatePath: path,
		Status:        status,
		Error:         err.Error(),
	}
}

func inspectCandidate(path string, active bool) CandidateReport {
	report := CandidateReport{
		CandidatePath: path,
		Active:        active,
		Status:        statusOK,
	}

	linkInfo, err := os.Lstat(path)
	if err != nil {
		report.Status = statusForError(err)
		report.Error = err.Error()
		return report
	}

	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		report.Status = statusForSymlinkError(linkInfo, err)
		report.Error = err.Error()
		return report
	}
	report.SymlinkResolvedPath = resolvedPath

	info, err := os.Stat(path)
	if err != nil {
		report.Status = statusForSymlinkError(linkInfo, err)
		report.Error = err.Error()
		return report
	}
	report.Size = info.Size()
	report.MTime = info.ModTime().UTC().Format(time.RFC3339Nano)
	report.Mode = info.Mode().String()
	report.ModeOctal = fmt.Sprintf("%#o", uint32(info.Mode().Perm()))
	if !info.Mode().IsRegular() {
		report.Status = statusError
		report.Error = "target is not a regular file"
		return report
	}
	report.Executable = info.Mode()&0o111 != 0

	digest, architectures, buildInfo, err := inspectFileSnapshot(resolvedPath, info)
	if err != nil {
		if errors.Is(err, errCandidateChanged) {
			report.Status = statusError
		} else {
			report.Status = statusForError(err)
		}
		report.Error = err.Error()
		return report
	}
	report.SHA256 = digest
	report.MachOArchitectures = architectures
	report.GoBuildInfo = buildInfo
	if !report.Executable {
		report.Status = statusNotExecutable
	}
	return report
}

func statusForSymlinkError(linkInfo os.FileInfo, err error) string {
	status := statusForError(err)
	if linkInfo != nil && linkInfo.Mode()&os.ModeSymlink != 0 && status == statusNotFound {
		return statusBrokenSymlink
	}
	return status
}

func statusForError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return statusNotFound
	case errors.Is(err, fs.ErrPermission):
		return statusPermission
	default:
		return statusError
	}
}

func hashFile(path string) (string, error) {
	before, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	digest, _, _, err := inspectFileSnapshot(path, before)
	return digest, err
}

func inspectFileSnapshot(path string, expected fs.FileInfo) (string, []string, *GoBuildInfoReport, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return "", nil, nil, errors.Join(err, file.Close())
	}
	if !sameRegularFileSnapshot(expected, opened) {
		return "", nil, nil, errors.Join(errCandidateChanged, file.Close())
	}

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", nil, nil, errors.Join(err, file.Close())
	}
	architectures := machoArchitectures(file)
	buildInfo := goBuildInfo(file)
	final, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return "", nil, nil, err
	}
	after, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, nil, errCandidateChanged
		}
		return "", nil, nil, err
	}
	if !sameRegularFileSnapshot(opened, final) || !sameRegularFileSnapshot(final, after) {
		return "", nil, nil, errCandidateChanged
	}
	return hex.EncodeToString(digest.Sum(nil)), architectures, buildInfo, nil
}

func sameRegularFileSnapshot(first, second fs.FileInfo) bool {
	return first != nil && second != nil &&
		first.Mode().IsRegular() && second.Mode().IsRegular() &&
		first.Mode() == second.Mode() && first.Size() == second.Size() &&
		first.ModTime().Equal(second.ModTime()) && os.SameFile(first, second)
}

func machoArchitectures(file io.ReaderAt) []string {
	fat, err := macho.NewFatFile(file)
	if err == nil {
		defer fat.Close()
		architectures := make([]string, 0, len(fat.Arches))
		for _, architecture := range fat.Arches {
			architectures = append(architectures, machoCPUName(architecture.Cpu))
		}
		return architectures
	}
	if !errors.Is(err, macho.ErrNotFat) {
		return nil
	}

	thin, err := macho.NewFile(file)
	if err != nil {
		return nil
	}
	defer thin.Close()
	return []string{machoCPUName(thin.Cpu)}
}

func machoCPUName(cpu macho.Cpu) string {
	switch cpu {
	case macho.Cpu386:
		return "i386"
	case macho.CpuAmd64:
		return "x86_64"
	case macho.CpuArm:
		return "arm"
	case macho.CpuArm64:
		return "arm64"
	case macho.CpuPpc:
		return "ppc"
	case macho.CpuPpc64:
		return "ppc64"
	default:
		return fmt.Sprintf("cpu_%#x", uint32(cpu))
	}
}

func goBuildInfo(file io.ReaderAt) *GoBuildInfoReport {
	info, err := buildinfo.Read(file)
	if err != nil {
		return nil
	}

	report := &GoBuildInfoReport{
		GoVersion: info.GoVersion,
		Path:      info.Path,
		Main:      goModule(info.Main),
	}
	for _, dependency := range info.Deps {
		if dependency != nil {
			report.Deps = append(report.Deps, goModule(*dependency))
		}
	}
	for _, setting := range info.Settings {
		if safeBuildSetting(setting.Key) {
			report.Settings = append(report.Settings, GoBuildSetting{Key: setting.Key, Value: setting.Value})
		}
	}
	return report
}

func goModule(module debug.Module) GoModule {
	report := GoModule{
		Path:    module.Path,
		Version: module.Version,
		Sum:     module.Sum,
	}
	if module.Replace != nil {
		replacement := goModule(*module.Replace)
		report.Replace = &replacement
	}
	return report
}

func safeBuildSetting(key string) bool {
	switch key {
	case "-buildmode", "-compiler", "CGO_ENABLED", "GOARCH", "GOOS", "GOAMD64", "GOARM", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GOWASM", "vcs", "vcs.revision", "vcs.time", "vcs.modified":
		return true
	default:
		return false
	}
}

func printText(w io.Writer, report TraceReport) {
	fmt.Fprintf(w, "requested_path: %s\nstatus: %s\n", report.RequestedPath, report.Status)
	if report.Error != "" {
		fmt.Fprintf(w, "error: %s\n", report.Error)
	}
	for index, candidate := range report.Candidates {
		fmt.Fprintf(w, "\ncandidate[%d]: %s", index+1, candidate.CandidatePath)
		if candidate.Active {
			fmt.Fprint(w, " (active)")
		}
		fmt.Fprintf(w, "\nstatus: %s\nexecutable: %t\n", candidate.Status, candidate.Executable)
		if candidate.SymlinkResolvedPath != "" {
			fmt.Fprintf(w, "symlink_resolved_path: %s\n", candidate.SymlinkResolvedPath)
		}
		if candidate.SHA256 != "" {
			fmt.Fprintf(w, "sha256: %s\nsize: %d\nmtime: %s\nmode: %s (%s)\n", candidate.SHA256, candidate.Size, candidate.MTime, candidate.Mode, candidate.ModeOctal)
		}
		if len(candidate.MachOArchitectures) > 0 {
			fmt.Fprintf(w, "macho_architectures: %s\n", strings.Join(candidate.MachOArchitectures, ", "))
		}
		if candidate.GoBuildInfo != nil {
			fmt.Fprintf(w, "go_version: %s\ngo_path: %s\n", candidate.GoBuildInfo.GoVersion, candidate.GoBuildInfo.Path)
		}
		if candidate.Error != "" {
			fmt.Fprintf(w, "error: %s\n", candidate.Error)
		}
	}
}
