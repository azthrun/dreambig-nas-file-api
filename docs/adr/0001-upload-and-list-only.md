# Upload and list only: no download, delete, move or overwrite

Callers can create files and folders inside their Scope and list or stat what is there, nothing more. A Conflict is always refused rather than overwritten or renamed, because overwriting is a form of deletion. A leaked Credential can therefore add files but can never read their contents or destroy existing data. Reading, moving and deleting stay with DSM File Station and SMB, which already do those jobs.

## Considered Options

- **Upload only (no listing)**: rejected; without listing a Caller cannot skip files that are already present or check an upload.
- **Full file manager**: rejected; it widens what a leaked Credential can do for features the NAS already provides.
- **Caller-chosen overwrite behind an extra permission**: deferred; add it only if a real need appears.
