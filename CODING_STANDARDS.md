# Coding standards

Read during review. Terms (Upload Root, Caller, Credential, Scope, Destination Path, Upload, Staging Area, Hidden Entry, Conflict) are defined in `GLOSSARY.md`; decisions in `docs/adr/`. `go vet`, tests, the image smoke test and the gitleaks scan run in CI; this file covers only what they cannot judge.

## Filesystem safety

- **Upload and list only (ADR-0001).** No code path reads file contents back to a Caller, deletes, moves or overwrites anything a Caller put in the Upload Root. A Conflict is refused, never resolved by overwriting or renaming. The only removals allowed are of the service's own data in the Staging Area.
- **Every Caller-supplied path goes through one validator** that resolves it against the Upload Root and the Caller's Scope and rejects escapes (`..`, absolute paths, symlinks out of the Upload Root) and Hidden Entries. Code that calls `os`/`filepath` on a string built from request input without that validator is a hard violation.
- **Hidden Entries are never listed and never a valid Destination Path**, including the Staging Area and DSM's `@`/`#` folders.
- **Completing an Upload is an atomic rename** from the Staging Area, which lives on the Upload Root's filesystem (ADR-0003); never a copy into place. Use a create-exclusive or link-based step so a file that appeared at the Destination Path meanwhile is a Conflict, not overwritten.

## HTTP

- Only `/healthz` is unauthenticated. Every other route requires a Credential (ADR-0004), compared in constant time (`crypto/subtle`); add new routes behind the auth middleware, not beside it.
- Error responses never echo absolute NAS paths or token material.
- Every write and every rejection emits one structured audit log line naming the Caller and the Destination Path.

## Config

- Settings come from `NAS_FILE_API_*` env vars parsed in `internal/config` (`main` passes it `os.Getenv`); nothing else reads the environment. A new variable is added to `config.EnvVars` and `.env.example` (`internal/config/envdocs_test.go` checks both ways).
- Secrets live only in `.env` on the NAS or CI secrets, never in the repo. Docs and examples use placeholders (`nas-file-api.<tailnet>.ts.net`, `<token>`), never the owner's real values.

## Deployment

- `compose.yaml` publishes no host port and uses no host networking (ADR-0002; CI checks this). The API listens on loopback and is reached only through `tailscale serve`.
- The image runs as a non-root UID/GID and is the static binary on distroless `static` (no shell, no package manager).

## Go

- Standard library first; a dependency needs a reason (e.g. `tusd` for ADR-0003).
- Wrap errors with context (`fmt.Errorf("...: %w", err)`); no `panic` for expected failures.
- Pass `context.Context` through request-scoped work; no goroutine outlives its request or the server without a shutdown path.

## Tests

- Behaviour is tested end to end through the HTTP handler (`httptest`) with a temp Upload Root, not by unit-testing private helpers.
- A safety rule above gets a test that tries to break it: path escape, symlink out of the Upload Root, Hidden Entry access, Conflict, out-of-Scope write, missing or wrong Credential.
