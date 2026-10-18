// Package uploadroot is the one validator every Caller-supplied path goes through
// (CODING_STANDARDS.md, Filesystem safety). A Path is a syntactically valid,
// NFC-normalized, Upload Root-relative path that names no Hidden Entry; a Root turns a
// Path into an OS path, checking the Caller's Scope and refusing symlink escapes.
package uploadroot

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// StagingDirName is the Staging Area's folder directly under the Upload Root (ADR-0003).
// It is a Hidden Entry.
const StagingDirName = ".nas-file-api-staging"

// ErrInvalidPath means a path breaks a syntax rule; it never reached the filesystem.
var ErrInvalidPath = errors.New("invalid path")

// ErrHidden means a path names a Hidden Entry.
var ErrHidden = errors.New("hidden entry")

// Path is a validated path relative to the Upload Root. The zero Path is the Upload
// Root itself. Comparison is case-sensitive.
type Path struct {
	segs []string
}

// ParsePath validates s, a slash-separated path relative to the Upload Root ("" is the
// Upload Root). It rejects a leading slash, "." and ".." segments, empty segments
// (including a trailing slash), backslashes, control characters and Hidden Entries,
// and NFC-normalizes each segment. Errors wrap ErrInvalidPath or ErrHidden.
func ParsePath(s string) (Path, error) {
	if s == "" {
		return Path{}, nil
	}
	if strings.HasPrefix(s, "/") {
		return Path{}, invalid("must be relative to the Upload Root")
	}
	return ParseSegments(strings.Split(s, "/"))
}

// ParseScope parses a Scope as written in config: absolute from the Upload Root
// ("/backups"), with "/" or "" meaning the whole Upload Root.
func ParseScope(s string) (Path, error) {
	if s == "" || s == "/" {
		return Path{}, nil
	}
	if !strings.HasPrefix(s, "/") {
		return Path{}, invalid("a Scope must start with /")
	}
	return ParsePath(s[1:])
}

// ParseEscapedPath validates the escaped (still percent-encoded) URL path suffix that
// names a path, e.g. "backups/x%20y" from /api/v1/folders/backups/x%20y. Each segment
// is decoded on its own, so "%2F" or "%2e%2e" cannot smuggle a separator or a dot
// segment past the rules.
func ParseEscapedPath(escaped string) (Path, error) {
	if escaped == "" {
		return Path{}, nil
	}
	if strings.HasPrefix(escaped, "/") {
		return Path{}, invalid("must be relative to the Upload Root")
	}
	raw := strings.Split(escaped, "/")
	for i, seg := range raw {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return Path{}, invalid("bad percent-encoding")
		}
		raw[i] = dec
	}
	return ParseSegments(raw)
}

// ParseSegments validates already-split segments, e.g. URL path segments decoded one by
// one so that an encoded "/" inside a segment is rejected rather than split.
func ParseSegments(raw []string) (Path, error) {
	segs := make([]string, 0, len(raw))
	for _, seg := range raw {
		switch {
		case seg == "":
			return Path{}, invalid("empty path segment")
		case seg == "." || seg == "..":
			return Path{}, invalid("dot segment")
		case strings.ContainsAny(seg, `/\`):
			return Path{}, invalid("slash inside a segment")
		case strings.ContainsFunc(seg, unicode.IsControl):
			return Path{}, invalid("control character")
		case !utf8.ValidString(seg):
			return Path{}, invalid("not UTF-8")
		}
		seg = norm.NFC.String(seg)
		if IsHiddenName(seg) {
			return Path{}, ErrHidden
		}
		segs = append(segs, seg)
	}
	p := Path{segs: segs}
	if p.isStaging() {
		return Path{}, ErrHidden
	}
	return p, nil
}

// IsHiddenName reports whether a single entry name is a Hidden Entry wherever it
// appears: DSM's @- and #-prefixed folders (@eaDir, #recycle, #snapshot).
func IsHiddenName(name string) bool {
	return strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#")
}

// IsHiddenChild reports whether the entry name directly inside folder p is a Hidden
// Entry: an @/# name anywhere, or the Staging Area under the Upload Root. Listings use
// it to filter entries.
func (p Path) IsHiddenChild(name string) bool {
	return IsHiddenName(name) || (p.IsRoot() && name == StagingDirName)
}

func (p Path) isStaging() bool { return len(p.segs) > 0 && p.segs[0] == StagingDirName }

// IsRoot reports whether p is the Upload Root itself.
func (p Path) IsRoot() bool { return len(p.segs) == 0 }

// Within reports whether p is scope or inside it (case-sensitive, segment-wise).
func (p Path) Within(scope Path) bool {
	if len(p.segs) < len(scope.segs) {
		return false
	}
	for i, s := range scope.segs {
		if p.segs[i] != s {
			return false
		}
	}
	return true
}

// Join returns p with name appended, validating name as one segment.
func (p Path) Join(name string) (Path, error) {
	child, err := ParseSegments([]string{name})
	if err != nil {
		return Path{}, err
	}
	segs := append(append([]string{}, p.segs...), child.segs...)
	return Path{segs: segs}, nil
}

// Segments returns a copy of p's segments.
func (p Path) Segments() []string { return append([]string{}, p.segs...) }

// String renders p as Callers see it: "/" for the Upload Root, else "/a/b". It never
// contains the Upload Root's absolute NAS path.
func (p Path) String() string { return "/" + strings.Join(p.segs, "/") }

func isValidUTF8(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func invalid(why string) error { return &pathError{why: why} }

type pathError struct{ why string }

func (e *pathError) Error() string { return "invalid path: " + e.why }
func (e *pathError) Unwrap() error { return ErrInvalidPath }
