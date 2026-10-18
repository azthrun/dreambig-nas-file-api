package uploadroot

import (
	"errors"
	"strings"
)

// Write-side path rules, applied to every path the service creates something at
// (create-folder, and an Upload's Destination Path), on top of ParsePath's rules:
//
//   - no character SMB cannot store: \ : * ? " < > |
//   - no segment ending in a dot or a space
//   - no Hidden Entry (an @/# segment, or the Staging Area)
//
// Every segment is checked, existing parents included. A refusal wraps ErrInvalidPath
// (400), including for a Hidden Entry: refusing to create one reveals nothing.

// smbUnstorable are the characters an SMB client cannot store in a name.
const smbUnstorable = `\:*?"<>|`

// ParseDestination is ParsePath plus the write-side path rules, for a path the Caller
// wants to create something at (e.g. a Destination Path from Upload metadata).
func ParseDestination(s string) (Path, error) {
	return writable(ParsePath(s))
}

// ParseEscapedDestination is ParseEscapedPath plus the write-side path rules, for a
// path taken from the request URL.
func ParseEscapedDestination(escaped string) (Path, error) {
	return writable(ParseEscapedPath(escaped))
}

// CheckWritable applies the write-side path rules to an already parsed Path.
func (p Path) CheckWritable() error {
	for _, seg := range p.segs {
		switch {
		case strings.ContainsAny(seg, smbUnstorable):
			return invalid(`a name must not contain any of \ : * ? " < > |`)
		case strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " "):
			return invalid("a name must not end in a dot or a space")
		}
	}
	return nil
}

func writable(p Path, err error) (Path, error) {
	if errors.Is(err, ErrHidden) {
		return Path{}, invalid("reserved name")
	}
	if err != nil {
		return Path{}, err
	}
	if err := p.CheckWritable(); err != nil {
		return Path{}, err
	}
	return p, nil
}
