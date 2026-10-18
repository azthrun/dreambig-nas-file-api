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
