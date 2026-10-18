# dreambig-nas-file-api

A small service on a Synology NAS that lets trusted Callers put files and folder trees into one dedicated folder on the NAS (the **Upload Root**), and see what is already there. Nothing is downloaded, deleted or overwritten ([ADR-0001](docs/adr/0001-upload-and-list-only.md)).

- Reachable only through a Tailscale sidecar, over HTTPS on a `*.ts.net` name, with no host port ([ADR-0002](docs/adr/0002-tailscale-sidecar-with-https.md)).
- Resumable Uploads with tus ([ADR-0003](docs/adr/0003-resumable-uploads-with-tus.md)). Named static bearer tokens per Caller ([ADR-0004](docs/adr/0004-named-static-tokens.md)).
- Vocabulary (Upload Root, Caller, Credential, Scope, Upload, Staging Area, ...): [GLOSSARY.md](GLOSSARY.md).

Target: DS220+ (Intel, `linux/amd64`), DSM 7.x, Container Manager.

## Synology setup

You need:
- DSM admin access.
- Admin access to the [Tailscale admin console](https://login.tailscale.com/admin).
- A Mac already on the tailnet (see `dreambig-nas-mcp`'s README, "Remote access").
- For the negative test in step 9, a second tailnet device that must *not* get access, e.g. your phone with the Tailscale app. If you don't have one, the NAS's own Tailscale node works too (step 9.3, option B).

Placeholders used below. Use your own values everywhere:

| Placeholder | Meaning |
| --- | --- |
| `uploads` | The Upload Root shared folder (`/volume1/uploads`) |
| `nas-file-api` | The dedicated DSM user, and also the tailnet machine name |
| `<tailnet>` | Your tailnet name (`<tailnet>.ts.net`) |
| `<UID>`, `<GID>` | The dedicated user's numeric IDs (step 3) |

`/volume1` assumes the shared folders are on Volume 1. **Control Panel > Shared Folder** shows each folder's volume; use `/volume2/...` etc. if yours differ.

### 1. Create the Upload Root shared folder

1. DSM > **Control Panel > Shared Folder > Create > Create Shared Folder**.
2. Name `uploads`, location Volume 1. Leave the recycle bin, encryption and advanced options at their defaults. **Next** through the wizard.
3. **Configure user permissions**: give your own admin account **Read/Write**, so you can manage the files in File Station and SMB. Leave everyone else at **No access**. **Apply**.

### 2. Create the dedicated DSM user

1. **Control Panel > User & Group > User > Create**.
2. Name `nas-file-api`. Set a long random password (nobody logs in as this user). Tick **Disallow the user to change account password**. **Next**.
3. **Join groups**: keep only `users`. **Next**.
4. **Assign shared folders permissions**: **Read/Write** on `uploads`, **No access** on every other folder. **Next**.
5. Quota: skip. **Application permissions**: tick **Deny** for every application. Speed limit: skip. **Done**.
6. Check:
   - **Control Panel > Shared Folder >** `uploads` **> Edit > Permissions** lists `nas-file-api` with Read/Write.
   - **User & Group > Group > users > Edit > Permissions**: the `users` group must not grant access to other shared folders, because `nas-file-api` inherits whatever that group has.

### 3. Find the user's UID and GID (no SSH needed)

The container runs as this user, never as root.

1. **Control Panel > Task Scheduler > Create > Scheduled Task > User-defined script**.
2. **General** tab: Task name `nas-file-api-id`, User `root`, untick **Enabled** (you run it by hand).
3. **Task Settings** tab, **Run command**: `id nas-file-api > /volume1/uploads/id.txt`. **OK**, then confirm the warning.
4. Select the task > **Run**. In **File Station**, open `uploads/id.txt`. The output looks like `uid=1031(nas-file-api) gid=100(users) ...`. Note both numbers.
5. Delete `id.txt`. Keep the task: you will reuse it in step 9.

### 4. Tailnet: enable HTTPS, define the tag, restrict access

In the admin console:

1. **DNS** tab: make sure **MagicDNS** is on. Under **HTTPS Certificates**, click **Enable HTTPS** and acknowledge the prompt. Machine names appear in public Certificate Transparency logs; `nas-file-api.<tailnet>.ts.net` reveals nothing sensitive.
2. **Access controls > JSON editor**. Copy the current policy somewhere as a backup. Merge in the parts below, then **Save**. Keep your existing `hosts` and rules (e.g. the one for `dreambig-nas-mcp`).

   ```json
   {
     "tagOwners": {
       "tag:nas-file-api": ["autogroup:admin"]
     },
     "acls": [
       // existing rules stay here
       { "action": "accept", "src": ["macbook"], "dst": ["tag:nas-file-api:443"] }
     ]
   }
   ```

   Add other devices to `src` only if they should be able to upload. Don't use `*` or `autogroup:member`. Every request still needs a Credential; the policy is the outer fence.

### 5. Generate the Tailscale auth key

1. Admin console > **Settings > Keys > Generate auth key**.
2. Settings:
   - **Reusable** off, **Ephemeral** off.
   - **Pre-approved** on, *if shown*. The toggle only appears when **Settings > Device management > Device approval** is enabled. No toggle means new devices are approved automatically; skip it.
   - **Tags** on, with `tag:nas-file-api`.
   - **Expiration** 1 day. The key is used only for the first login; after that the node's identity lives in the state folder (step 7).
3. Copy the `tskey-auth-...` value. It is shown only once. Treat it like a password.

### 6. Make the GHCR image pullable

The image is `ghcr.io/azthrun/dreambig-nas-file-api` (`linux/amd64`). CI publishes it when a `v*` tag is pushed. Every release gets its version tag (e.g. `:v1.2.0`); only non-pre-release tags also move `:latest`, so `v0.0.1-alpha` has no `:latest`.

1. Once, on github.com: profile > **Packages > dreambig-nas-file-api > Package settings > Change visibility > Public**. Skip if it is already public.
2. Check on the Mac (no Docker needed). This gets an anonymous pull token, then fetches the image manifest:

   ```sh
   TOKEN=$(curl -s "https://ghcr.io/token?scope=repository:azthrun/dreambig-nas-file-api:pull" | sed -E 's/.*"token":"([^"]+)".*/\1/')
   curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
     -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
     https://ghcr.io/v2/azthrun/dreambig-nas-file-api/manifests/latest
   ```

   - `200`: public, and the tag exists.
   - `401`/`403`: still private.
   - `404`: no such tag. Replace `latest` in the URL with the version you want; if it answers `200`, use that tag in the `image:` line in step 8.

### 7. Prepare the Project folder, `.env` and `tailscale-serve.json`

1. **File Station**: create the folder `docker/dreambig-nas-file-api` (outside `uploads`). Inside it, create an empty folder `tailscale-state` for the sidecar's state. It must stay outside the Upload Root.
2. Get the exact NAS path of `tailscale-state`: right-click it > **Properties**. **Location** shows `/docker/dreambig-nas-file-api/tailscale-state`. Put the volume in front: `/volume1/docker/dreambig-nas-file-api/tailscale-state`.
3. On the Mac, copy [`.env.example`](.env.example) to a file named `env.txt` and fill it in (no quotes, no trailing spaces):
   - `NAS_FILE_API_UID`, `NAS_FILE_API_GID`: the numbers from step 3.
   - `NAS_FILE_API_UPLOAD_ROOT_NAS_PATH=/volume1/uploads`
   - `NAS_FILE_API_TS_STATE_NAS_PATH`: the path from step 7.2.
   - `NAS_FILE_API_TS_AUTHKEY`: the key from step 5.
   - `NAS_FILE_API_TS_HOSTNAME=nas-file-api` (or your own machine name).
   - `NAS_FILE_API_CALLERS`: one entry per Caller, comma-separated, as `name:token` or `name:token:/scope` (format in `.env.example`). Generate each token on the Mac with `openssl rand -hex 32` and give it only to that Caller. The service refuses to start on placeholder or short tokens, or duplicate names.
   - Optional: `NAS_FILE_API_UPLOAD_EXPIRY_DAYS` (default `7`): an incomplete Upload untouched this many days is removed from the Staging Area (checked at startup and hourly; each removal is in the audit log).
   - Leave `NAS_FILE_API_ADDR` and `NAS_FILE_API_UPLOAD_ROOT` as they are. `compose.yaml` overrides the first; the second is already `/data`.
4. Upload `env.txt` into `docker/dreambig-nas-file-api` and rename it to `.env`. To see it afterwards, tick **File Station > Settings > General > Show hidden files**.
5. Upload [`tailscale-serve.json`](tailscale-serve.json) into the same folder, keeping its name. `compose.yaml` mounts it as `./tailscale-serve.json`. If it is missing, Docker creates an empty folder with that name and the sidecar won't serve HTTPS.
6. Keep `.env` out of git and out of the Upload Root.

### 8. Create the Container Manager Project

1. Install **Container Manager** from Package Center if it isn't installed.
2. **Container Manager > Project > Create**.
3. **Project name** `dreambig-nas-file-api`. **Path** `/docker/dreambig-nas-file-api`. **Source**: **Create docker-compose.yml**, and paste [`compose.yaml`](compose.yaml). If step 6 told you to use a version tag, change the `api` service's `image:` line to it. **Next**.
4. **Web portal settings**: leave Web Station off. **Next**. Tick **Start the project once it is created** > **Done**.
5. **Container Manager > Container**: both `dreambig-nas-file-api-tailscale-1` and `dreambig-nas-file-api-api-1` show **Running**. `api` turns healthy after about 15 s.
6. Admin console > **Machines**: a machine named `nas-file-api` appears, tagged `tag:nas-file-api`. Tagged nodes have key expiry disabled by default.

### 9. Verify

1. **HTTPS from an allowed device** (the Mac):

   ```sh
   curl -v https://nas-file-api.<tailnet>.ts.net/healthz
   ```

   Prints `ok` with status `200`, and no certificate warnings. The very first request can take 10–30 s while the certificate is issued.
2. **Not on the LAN**: `curl -m 5 http://<nas-lan-ip>:8080/healthz` and `curl -m 5 -k https://<nas-lan-ip>/healthz` must both fail (connection refused or timeout, never `200`). Also, **Container Manager > Container >** `dreambig-nas-file-api-api-1` **> Details** shows no port mappings.
3. **Not from a disallowed tailnet device**:
   - Option A (phone): with Tailscale connected, open `https://nas-file-api.<tailnet>.ts.net/healthz` in the browser. It must time out.
   - Option B (the NAS's own Tailscale node): in Task Scheduler, select `nas-file-api-id` > **Edit > Task Settings**. Set the command to

     ```
     curl -m 5 -s -o /dev/null -w '%{http_code}\n' https://nas-file-api.<tailnet>.ts.net/healthz > /volume1/uploads/neg.txt 2>&1
     ```

     **OK**, then **Run**. `uploads/neg.txt` must contain `000` (timeout), not `200`. Delete `neg.txt`.
4. **Files are owned by the dedicated user**. This runs **on the NAS** through Task Scheduler; the Mac needs no Docker.
   1. Edit the `nas-file-api-id` task's command to:

      ```
      /usr/local/bin/docker run --rm --volumes-from dreambig-nas-file-api-api-1 --user <UID>:<GID> alpine sh -c 'echo ok > /data/probe.txt' > /volume1/uploads/probe-log.txt 2>&1
      ```

      `--volumes-from` reuses the API container's exact mount (the Upload Root is at `/data`), so this tests the real config. **OK**, then **Run**. The first run downloads `alpine`; wait about 30 s.
   2. Edit the command to `ls -ln /volume1/uploads > /volume1/uploads/ls.txt 2>&1`. **OK**, then **Run**.
   3. In `uploads/ls.txt`, the `probe.txt` line must show `<UID> <GID>` before the size. `probe-log.txt` and `ls.txt` show `0 0`; that is expected, because the task itself runs as root. If `probe.txt` is missing, read `probe-log.txt`.
   4. Delete `probe.txt`, `probe-log.txt` and `ls.txt`.
5. Delete the `nas-file-api-id` task.

### Troubleshooting

Logs: **Container Manager > Container >** select the container **> Details > Log**.

- **Project creation says a path does not exist**: a `*_NAS_PATH` value in `.env` doesn't match a real folder. Recheck step 7.2 (the volume number, spelling, no quotes or trailing spaces), fix `.env`, upload it again, and retry.
- **A compose error names `NAS_FILE_API_UID`** (or another variable): `.env` is missing from the Project folder, or lacks that value.
- **Image pull fails with "not found"**: the `image:` tag doesn't exist. See step 6.
- **`invalid key` in the `tailscale` log**: the auth key is wrong, expired or already used. Generate a new one (step 5), put it in `.env`, then **Project > Action > Stop**, **Action > Build**.
- **`healthz` over HTTPS fails, but the `api` container is healthy**: check `tailscale-serve.json` is a file (not a folder) in the Project folder, and HTTPS Certificates are enabled (step 4.1).

### Updating

**Container Manager > Project >** `dreambig-nas-file-api` **> Action > Stop**, then **Action > Build**. This pulls the `image:` tag again and recreates the containers. If you pinned a version tag, change it first under **YAML configurations** (or **Edit**).

The sidecar keeps its tailnet identity in `tailscale-state`, so an update needs no new auth key. If that folder is emptied or deleted, generate a new key (step 5) and update `.env`.

## Using the API from scripts

There is no first-party client. Scripts use curl and any tus client. Every route except `/healthz` lives under `/api/v1/` and needs `Authorization: Bearer <token>`. Paths are relative to the Upload Root, and a Caller with a Scope sees only its subtree.

### Add a Caller

1. On the Mac, generate the Caller's Credential: `openssl rand -hex 32`. Keep it in a password manager or a file only the script can read. Give each script its own token.
2. **File Station** > `docker/dreambig-nas-file-api` > `.env` (tick **Settings > General > Show hidden files** if you can't see it). Right-click > **Download**, edit it on the Mac, and append the Caller to `NAS_FILE_API_CALLERS`, comma-separated, as `name:token` or `name:token:/scope`. For example, `laptop:<token>,photo-backup:<token>:/backups/photos` gives `photo-backup` only `/backups/photos`. Upload it back under the same name and overwrite.
3. **Container Manager > Project >** `dreambig-nas-file-api` **> Action > Stop**, then **Action > Build**, so the containers are recreated with the new `.env`.
4. **Container Manager > Container >** `dreambig-nas-file-api-api-1` **> Details > Log** shows `"callers":N` with the new count. If it says `refusing to start: bad config`, the message names the entry that is wrong.

To revoke a Caller, delete its entry and do step 3 again.

The machine running the script must also be allowed through the tailnet policy (step 4 of the setup). For a server or CI runner, tag it and add the tag to `src`:

```json
{
  "tagOwners": {
    "tag:nas-file-api": ["autogroup:admin"],
    "tag:upload-script": ["autogroup:admin"]
  },
  "acls": [
    { "action": "accept", "src": ["macbook", "tag:upload-script"], "dst": ["tag:nas-file-api:443"] }
  ]
}
```

### Shell setup

The examples below use these two variables:

```sh
export NAS_API=https://nas-file-api.<tailnet>.ts.net/api/v1
export NAS_TOKEN=<token>   # better: NAS_TOKEN="$(cat ~/.config/nas-file-api/token)", keeping it out of shell history
```

### curl

Percent-encode path segments (a space is `%20`). Every error is JSON, `{"error":"..."}`.

```sh
# Create a folder, with any missing parents: 201 if something was created, 200 if it already existed.
curl -sS -X PUT -H "Authorization: Bearer $NAS_TOKEN" "$NAS_API/folders/photos/2024%20trip"
# {"path":"/photos/2024 trip","created":true}

# List one folder (the Upload Root is "$NAS_API/folders/").
curl -sS -H "Authorization: Bearer $NAS_TOKEN" "$NAS_API/folders/photos"
# {"path":"/photos","entries":[{"name":"2024 trip","type":"folder","size":0,"mtime":"2026-10-18T15:17:20.807799107Z"},...]}

# Everything below a folder, streamed as one JSON line per entry (paths relative to that folder).
curl -sS -N -H "Authorization: Bearer $NAS_TOKEN" "$NAS_API/tree/photos"
# {"path":"2024 trip","type":"folder","size":0,"mtime":"2026-10-18T15:17:20.807799107Z"}
# {"path":"2024 trip/img_001.jpg","type":"file","size":2483012,"mtime":"2024-06-01T10:00:00Z"}

# Stat one file; ?hash=sha256 adds its SHA-256 (the server reads the whole file each time).
curl -sS -H "Authorization: Bearer $NAS_TOKEN" "$NAS_API/files/photos/2024%20trip/img_001.jpg?hash=sha256"
# {"path":"/photos/2024 trip/img_001.jpg","type":"file","size":2483012,"mtime":"2024-06-01T10:00:00Z","sha256":"<64 hex>"}
```

The tree listing is streamed. If it fails partway, its last line is `{"error":"..."}` and the listing is incomplete.

Status codes: `400` bad path or name, `401` missing or wrong Credential, `403` outside your Scope, `404` not found, `409` a file is in the way.

### Uploads (tus)

Uploads use the [tus protocol](https://tus.io/protocols/resumable-upload) at `$NAS_API/uploads/`. Any tus 1.0 client works. Send in the Upload metadata:

| Key | Value |
| --- | --- |
| `path` | Destination Path, relative to the Upload Root (e.g. `docs/report.pdf`); required. Missing folders are created. |
| `mtime` | The file's modification time, RFC 3339 (e.g. `2021-03-04T05:06:07Z`); required. The file gets this mtime. |
| `sha256` | The whole file's SHA-256, hex; optional. If it doesn't match, the Upload is refused and discarded. |

The response that completes the Upload carries the server's SHA-256 of the file in `Upload-Sha256`. Nothing is ever overwritten: if the Destination Path already holds a file, creating the Upload (or completing it, if the file appeared meanwhile) fails with `409`. A SHA-256 mismatch fails with `460`. An unfinished Upload can be resumed by the Caller that created it.

#### With curl

tus metadata values are base64. This creates the Upload, then sends the whole file in one request:

```sh
b64() { printf %s "$1" | base64 | tr -d '\n'; }

f=report.pdf
dest=docs/report.pdf
size=$(wc -c < "$f" | tr -d ' ')
mtime=$(date -u -r "$f" +%Y-%m-%dT%H:%M:%SZ)   # GNU and BSD date
sum=$(shasum -a 256 "$f" | cut -d' ' -f1)     # or: sha256sum "$f" | cut -d' ' -f1

upload=$(curl -sS -D - -o /dev/null -X POST "$NAS_API/uploads/" \
  -H "Authorization: Bearer $NAS_TOKEN" -H "Tus-Resumable: 1.0.0" \
  -H "Upload-Length: $size" \
  -H "Upload-Metadata: path $(b64 "$dest"),mtime $(b64 "$mtime"),sha256 $(b64 "$sum")" \
  | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
echo "$upload"   # the Upload's URL; empty if creating failed (run the POST without -D - -o /dev/null and the pipe to see why)

curl -sS -D - -o /dev/null -X PATCH "$upload" \
  -H "Authorization: Bearer $NAS_TOKEN" -H "Tus-Resumable: 1.0.0" \
  -H "Content-Type: application/offset+octet-stream" -H "Upload-Offset: 0" \
  -T "$f"
# HTTP/1.1 204 No Content ... Upload-Offset: <size> ... Upload-Sha256: <64 hex>
```

A 0-byte file completes on creation; skip the `PATCH`. If the `PATCH` is cut off, ask for the offset the server has and send the rest:

```sh
offset=$(curl -sS -I "$upload" \
  -H "Authorization: Bearer $NAS_TOKEN" -H "Tus-Resumable: 1.0.0" \
  | tr -d '\r' | sed -n 's/^[Uu]pload-[Oo]ffset: //p')

tail -c +$((offset + 1)) "$f" | curl -sS -D - -o /dev/null -X PATCH "$upload" \
  -H "Authorization: Bearer $NAS_TOKEN" -H "Tus-Resumable: 1.0.0" \
  -H "Content-Type: application/offset+octet-stream" -H "Upload-Offset: $offset" \
  -T -
```

#### With Python (tus-py-client)

```sh
python3 -m venv ~/.venvs/nas-file-api && ~/.venvs/nas-file-api/bin/pip install tuspy
```

```python
import datetime, hashlib, os
from tusclient import client
from tusclient.exceptions import TusCommunicationError

src, dest = "report.pdf", "docs/report.pdf"

mtime = datetime.datetime.fromtimestamp(os.stat(src).st_mtime, datetime.timezone.utc).isoformat()
h = hashlib.sha256()
with open(src, "rb") as f:
    for block in iter(lambda: f.read(1 << 20), b""):
        h.update(block)

tus = client.TusClient(os.environ["NAS_API"] + "/uploads/",
                       headers={"Authorization": "Bearer " + os.environ["NAS_TOKEN"]})
# Without chunk_size, tuspy reads the whole file into memory and sends it in one request.
uploader = tus.uploader(src, chunk_size=64 << 20,
                        metadata={"path": dest, "mtime": mtime, "sha256": h.hexdigest()})
try:
    uploader.upload()
except TusCommunicationError as e:  # e.status_code: 409 Conflict, 460 SHA-256 mismatch, ...
    raise SystemExit("upload failed: %s %s" % (e.status_code, e.response_content))
if uploader.request:  # None for a 0-byte file, which completes on creation
    print("server sha256:", uploader.request.response_headers["upload-sha256"])
```

Run it with `~/.venvs/nas-file-api/bin/python3 upload.py`.

### Folder Upload

[`examples/folder_upload.py`](examples/folder_upload.py) rebuilds a local folder under a Destination Path, one Upload per file. It lists the destination once with the tree listing, skips files already there with the same size and mtime (to the second), reports files that are there but differ as Conflicts (never overwritten), creates the empty folders, and uploads the rest with their mtime and SHA-256. A dropped connection is resumed; running it again after a failure skips what already arrived. Symlinks are skipped.

```sh
~/.venvs/nas-file-api/bin/python3 examples/folder_upload.py --dry-run ~/Pictures/2024 backups/pictures/2024
~/.venvs/nas-file-api/bin/python3 examples/folder_upload.py ~/Pictures/2024 backups/pictures/2024
# mkdir    empty-album
# uploaded img_001.jpg (2483012 bytes, sha256 <64 hex>)
# ...
# 120 uploaded, 1 folders created, 0 skipped, 0 conflicts, 0 failed
```

It reads `NAS_API` and `NAS_TOKEN` from the environment and exits `1` if anything failed or was a Conflict. Its diff logic has tests: `python3 -m unittest discover -s examples`.

## Development

Go (version in [`go.mod`](go.mod)). Coding standards: [CODING_STANDARDS.md](CODING_STANDARDS.md).

```sh
go vet ./...
go test ./...
token="$(openssl rand -hex 32)"
mkdir -p /tmp/upload-root
NAS_FILE_API_ADDR=127.0.0.1:8080 NAS_FILE_API_UPLOAD_ROOT=/tmp/upload-root \
  NAS_FILE_API_CALLERS="dev:$token" go run ./cmd/nas-file-api
curl http://127.0.0.1:8080/healthz
# in a second terminal, with the same $token:
curl -H "Authorization: Bearer $token" http://127.0.0.1:8080/api/v1/folders/
```

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs on every push: vet, tests, build, a secrets scan, compose validation, and an image build plus smoke test. A `v*` tag also publishes the image to GHCR.
