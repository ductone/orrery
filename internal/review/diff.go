// Package review plans independent reviews of a workspace diff. It splits the
// diff into files, decides which files a reviewer must read in full, sizes the
// review from what remains, packs large reviews into shards, and merges shard
// verdicts. Classification that needs judgement goes through a Classifier; the
// rules that do not, such as "code is always reviewed", are plain code.
package review

import (
	"bytes"
	"path"
	"strconv"
	"strings"
)

// Status is how a file changed.
type Status string

const (
	Added    Status = "added"
	Modified Status = "modified"
	Deleted  Status = "deleted"
	Renamed  Status = "renamed"
)

// File is one file's part of the diff.
type File struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  Status `json:"status"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary,omitempty"`
	// Patch is the file's complete section of the unified diff, header included.
	Patch string `json:"-"`
	Class Class  `json:"class"`
}

// ParseDiff splits a unified git diff (including the synthetic sections the
// harness writes for untracked files) into files, in diff order.
func ParseDiff(diff []byte) []File {
	var files []File
	var cur *File
	var patch bytes.Buffer
	flush := func() {
		if cur != nil {
			cur.Patch = patch.String()
			files = append(files, *cur)
		}
		patch.Reset()
	}
	inHunk := false
	for _, line := range bytes.SplitAfter(diff, []byte("\n")) {
		text := strings.TrimRight(string(line), "\n")
		if strings.HasPrefix(text, "diff --git ") {
			flush()
			cur = &File{Status: Modified}
			cur.OldPath, cur.Path = gitPaths(text)
			inHunk = false
		}
		if cur == nil {
			continue
		}
		patch.Write(line)
		if inHunk {
			// Inside a hunk every line is content, even one that looks like a
			// header ("--- " is a removed line starting with "-- ").
			switch {
			case strings.HasPrefix(text, "@@"):
			case strings.HasPrefix(text, "+"):
				cur.Added++
				if text == "+[binary file omitted]" {
					cur.Binary = true
				}
			case strings.HasPrefix(text, "-"):
				cur.Removed++
			}
			continue
		}
		switch {
		case strings.HasPrefix(text, "@@"):
			inHunk = true
		case strings.HasPrefix(text, "new file mode"):
			cur.Status = Added
		case strings.HasPrefix(text, "deleted file mode"):
			cur.Status = Deleted
		case strings.HasPrefix(text, "rename from "):
			cur.Status = Renamed
			cur.OldPath = strings.TrimPrefix(text, "rename from ")
		case strings.HasPrefix(text, "rename to "):
			cur.Path = strings.TrimPrefix(text, "rename to ")
		case strings.HasPrefix(text, "Binary files ") || strings.HasPrefix(text, "GIT binary patch"):
			cur.Binary = true
		case strings.HasPrefix(text, "+++ "):
			if p := strings.TrimPrefix(text[4:], "b/"); p != "/dev/null" {
				cur.Path = p
			}
		}
	}
	flush()
	if len(files) > 0 && len(diff) > 0 && diff[len(diff)-1] != '\n' {
		// A diff cut off mid-line was truncated by the collector.
		files[len(files)-1].Patch += "\n[diff truncated]\n"
	}
	return files
}

// gitPaths reads "diff --git a/x b/y". Paths with spaces are ambiguous in this
// header, so the +++ line, when present, has the final say.
func gitPaths(header string) (string, string) {
	rest := strings.TrimPrefix(header, "diff --git ")
	if i := strings.Index(rest, " b/"); i >= 0 && strings.HasPrefix(rest, "a/") {
		return rest[2:i], rest[i+3:]
	}
	return "", rest
}

// Class is what kind of file a change touches, which decides whether a
// reviewer must read it.
type Class string

const (
	// Code is always reviewed in full.
	Code Class = "code"
	// Asset, Lockfile, Generated, and Binary changes are summarised.
	Asset     Class = "asset"
	Lockfile  Class = "lockfile"
	Generated Class = "generated"
	BinaryC   Class = "binary"
	// Other covers prose, data, and configuration: it needs judgement.
	Other Class = "other"
)

var codeExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".py": true, ".rb": true, ".rs": true, ".java": true, ".kt": true, ".kts": true, ".scala": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".cs": true, ".swift": true, ".m": true,
	".php": true, ".lua": true, ".ex": true, ".exs": true, ".erl": true, ".hs": true, ".clj": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".ps1": true,
	".sql": true, ".proto": true, ".graphql": true, ".gql": true, ".tf": true, ".hcl": true,
	".bzl": true, ".mk": true, ".cmake": true, ".gradle": true, ".vue": true, ".svelte": true,
}

var codeNames = map[string]bool{
	"Makefile": true, "makefile": true, "GNUmakefile": true, "Dockerfile": true, "Containerfile": true,
	"BUILD": true, "BUILD.bazel": true, "WORKSPACE": true, "MODULE.bazel": true, "Justfile": true,
	"Rakefile": true, "Gemfile": true, "Jenkinsfile": true, "Taskfile": true, "go.mod": true,
	"CMakeLists.txt": true, "Vagrantfile": true,
}

var assetExtensions = map[string]bool{
	".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".ico": true,
	".bmp": true, ".tiff": true, ".pdf": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".eot": true, ".mp3": true, ".mp4": true, ".mov": true, ".wav": true, ".zip": true, ".gz": true,
	".tar": true, ".jar": true, ".wasm": true,
}

var lockfiles = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"bun.lockb": true, "go.sum": true, "Cargo.lock": true, "poetry.lock": true, "Pipfile.lock": true,
	"uv.lock": true, "Gemfile.lock": true, "composer.lock": true, "mix.lock": true, "flake.lock": true,
	"MODULE.bazel.lock": true, "gradle.lockfile": true, "packages.lock.json": true, "Podfile.lock": true,
}

// generatedMarkers are the conventional headers of generated files.
var generatedMarkers = []string{"code generated", "do not edit", "@generated", "autogenerated", "auto-generated"}

// generatedSuffixes are names that are generated by convention.
var generatedSuffixes = []string{".pb.go", ".pb.gw.go", "_pb2.py", "_pb2_grpc.py", ".pb.ts", "_pb.ts", "_pb.js", ".gen.go", ".generated.go", "_generated.go", ".min.js", ".min.css", ".map", ".snap"}

var generatedDirs = []string{"dist/", "build/", "vendor/", "node_modules/", "gen/", "generated/", "__generated__/", "third_party/"}

// Classify assigns a file its class from its name and its first added lines.
func Classify(f *File) {
	name := path.Base(f.Path)
	ext := strings.ToLower(path.Ext(name))
	switch {
	case f.Binary:
		f.Class = BinaryC
	case lockfiles[name]:
		f.Class = Lockfile
	case assetExtensions[ext]:
		f.Class = Asset
	case generated(f):
		f.Class = Generated
	case codeExtensions[ext] || codeNames[name] || strings.HasSuffix(name, ".mk"):
		f.Class = Code
	default:
		f.Class = Other
	}
}

func generated(f *File) bool {
	lower := strings.ToLower(f.Path)
	for _, s := range generatedSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	for _, d := range generatedDirs {
		if strings.HasPrefix(lower, d) || strings.Contains(lower, "/"+d) {
			return true
		}
	}
	// Generators put their marker in the first lines of the file.
	seen := 0
	for _, line := range strings.Split(f.Patch, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		low := strings.ToLower(line)
		for _, m := range generatedMarkers {
			if strings.Contains(low, m) {
				return true
			}
		}
		if seen++; seen >= 5 {
			break
		}
	}
	return false
}

// Summary is a one-line description of a file for a reviewer who is not shown
// its patch.
func (f File) Summary() string {
	s := f.Path + " (" + string(f.Status) + ", " + string(f.Class)
	if f.Binary {
		s += ", binary"
	} else {
		s += ", +" + strconv.Itoa(f.Added) + "/-" + strconv.Itoa(f.Removed)
	}
	return s + ")"
}
