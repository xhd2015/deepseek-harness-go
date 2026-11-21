package session

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Compression is the physical encoding of a session artifact.
type Compression string

const (
	// CompressionZstd is the DeepSeek Harness default: concatenated,
	// checksummed Zstandard frames named session.vN.jsonl.zstd.
	CompressionZstd Compression = "zstd"
	// CompressionNone is plain JSONL named session.vN.jsonl.
	CompressionNone Compression = "none"
)

const (
	// FormatVersion is the generation this store writes.
	FormatVersion = 4
	// MinReadableVersion is the oldest generation this store reads.
	MinReadableVersion = 3
	// SessionPrefix starts every canonical DeepSeek Harness session id.
	SessionPrefix = "session-"
	// NoCwdDir groups sessions whose header has no working directory.
	NoCwdDir = "_no-cwd"
	// MaxProjectSlug bounds the readable project directory component.
	MaxProjectSlug = 251
)

// generationName matches one canonical session generation filename.
var generationName = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl(\.zstd)?$`)

// isSafeSegment reports the code units encodeSegment and projectKey keep literal.
func isSafeSegment(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-':
		return true
	}
	return false
}

// escapeUnit renders one UTF-16 code unit in the ~XXXX form DSH uses.
func escapeUnit(unit uint16) string {
	return fmt.Sprintf("~%04X", unit)
}

// encodeSegment renders an arbitrary id as one injective, filesystem-safe path
// segment. Safe code units stay literal; every other unit, including '~',
// becomes ~XXXX. DSH operates on UTF-16 code units, so astral characters
// escape as their surrogate pair.
func encodeSegment(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("session: cannot encode an empty path segment")
	}
	if raw == "." {
		return "~002E", nil
	}
	if raw == ".." {
		return "~002E~002E", nil
	}
	var out strings.Builder
	for _, unit := range utf16.Encode([]rune(raw)) {
		if unit < 0x80 && unit != '~' && isSafeSegment(byte(unit)) {
			out.WriteByte(byte(unit))
			continue
		}
		out.WriteString(escapeUnit(unit))
	}
	return out.String(), nil
}

// ProjectKey renders a working directory as DSH's human-navigable project
// directory name: separator runs collapse to one '-', unsafe code units escape
// as ~XXXX, leading dashes are stripped, and the slug is bounded.
func ProjectKey(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return NoCwdDir
	}
	var readable strings.Builder
	separatorRun := false
	for _, unit := range utf16.Encode([]rune(cwd)) {
		ch := byte(unit)
		if unit < 0x80 && (ch == '/' || ch == '\\' || ch == ':') {
			if !separatorRun {
				readable.WriteByte('-')
			}
			separatorRun = true
			continue
		}
		separatorRun = false
		if unit < 0x80 && unit != '~' && isSafeSegment(ch) {
			readable.WriteByte(ch)
			continue
		}
		readable.WriteString(escapeUnit(unit))
	}
	slug := strings.TrimLeft(readable.String(), "-")
	if slug == "" {
		slug = "root"
	}
	if len(slug) > MaxProjectSlug {
		slug = slug[:MaxProjectSlug]
	}
	return "--" + slug + "--"
}

// Slug is kept for callers that only need the project directory component.
func Slug(cwd string) string { return ProjectKey(cwd) }

// SessionDir returns the directory that owns one session id.
func SessionDir(root, cwd, id string) (string, error) {
	segment, err := encodeSegment(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ProjectKey(cwd), segment), nil
}

// GenerationName returns the canonical filename for one generation.
func GenerationName(version int, compression Compression) string {
	name := fmt.Sprintf("session.v%d.jsonl", version)
	if compression == CompressionZstd {
		name += ".zstd"
	}
	return name
}

// ParseGenerationName parses one canonical generation filename. Noncanonical
// names (temporary files, versions with leading zeros, unknown suffixes) do not
// identify a committed generation.
func ParseGenerationName(name string) (version int, compression Compression, ok bool) {
	match := generationName.FindStringSubmatch(name)
	if match == nil {
		return 0, "", false
	}
	if match[2] == ".zstd" {
		compression = CompressionZstd
	} else {
		compression = CompressionNone
	}
	if match[1] == "" {
		return 0, compression, true
	}
	value, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, "", false
	}
	return value, compression, true
}

// NormalizeSessionID prefixes a bare UUID the way DSH mints ids. Ids that
// already carry the canonical prefix are returned unchanged.
func NormalizeSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, SessionPrefix) {
		return id
	}
	return SessionPrefix + id
}
