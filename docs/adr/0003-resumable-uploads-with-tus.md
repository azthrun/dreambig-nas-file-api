# Resumable Uploads with the tus protocol

Every Upload uses tus, through `tusd` embedded in the Go server, because multi-GB files over flaky links must survive dropped connections and adding resumability later would change the API's shape. tus's file store directory is the Staging Area, on the Upload Root's filesystem, so completing an Upload is an atomic rename. A whole-file SHA-256 is optional in the Upload metadata. The server always hashes the file at completion, rejects it on a mismatch, and returns the hash, so browsers do not have to read a file twice before uploading. Folder Uploads are one Upload per file, never an archive that the server unpacks.

## Considered Options

- **Single-request uploads**: rejected; they fail on large files over unreliable links.
- **Hand-rolled chunking**: rejected; it means maintaining many edge cases ourselves, and off-the-shelf tus clients would not work with it.
- **Archive upload with server-side extraction**: rejected; it brings zip-slip and zip-bomb risks, and a failure is all-or-nothing.
- **Required up-front hash, or an explicit commit step**: rejected; the first makes browsers read every file twice, and the second breaks generic tus clients.
