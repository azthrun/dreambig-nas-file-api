module github.com/azthrun/dreambig-nas-file-api

go 1.27.2

// Why each dependency is here (CODING_STANDARDS.md: a dependency needs a reason):
require (
	// tusd: resumable Uploads over tus (ADR-0003).
	github.com/tus/tusd/v2 v2.10.1
	// x/exp: tusd's handler.Config.Logger is an x/exp/slog Logger, not log/slog; used
	// only to give tusd a discarding logger.
	golang.org/x/exp v0.0.0-20260611194520-c48552f49976
	// x/text: NFC normalization of path segments (unicode/norm); the standard library
	// has none, and macOS clients send NFD names.
	golang.org/x/text v0.43.0
)
