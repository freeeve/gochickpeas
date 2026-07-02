package rcpg

import (
	"errors"
	"fmt"
)

// ErrCorrupt reports input bytes that are not a valid file of the expected
// format. Every structural problem in a parse surfaces as an error wrapping
// this sentinel; parsers never panic on malformed input.
var ErrCorrupt = errors.New("corrupt input")

// ErrUnsupportedVersion reports a file that is a recognized format but a
// newer, unsupported version.
var ErrUnsupportedVersion = errors.New("unsupported version")

// corruptf wraps ErrCorrupt with positional detail, mirroring the Rust
// codec's FormatError::Corrupt(String) messages.
func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

// unsupportedVersionf wraps ErrUnsupportedVersion with the format name and
// the version the file declared.
func unsupportedVersionf(format string, version uint16) error {
	return fmt.Errorf("%w: %s version %d", ErrUnsupportedVersion, format, version)
}
