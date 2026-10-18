#!/usr/bin/env python3
"""Sample Folder Upload: rebuild a local folder tree under a Destination Path.

Usage:
    NAS_API=https://nas-file-api.<tailnet>.ts.net/api/v1 NAS_TOKEN=<token> \\
        python3 folder_upload.py [--dry-run] LOCAL_FOLDER DESTINATION

DESTINATION is a folder path relative to the Upload Root, e.g. backups/photos ("" or
"/" for the Upload Root itself).

It lists DESTINATION once (GET /tree), then:
- skips files the NAS already has with the same size and mtime (to the second),
- reports files that exist on the NAS but differ as Conflicts (the service never
  overwrites, so they are not sent),
- creates the empty folders the NAS lacks (PUT /folders),
- uploads every other file with tus, declaring its mtime and SHA-256 so the server
  verifies the whole file.

Running it again after a failure picks up where it left off, file by file: finished
files are skipped. A file whose Upload was cut off starts over; the abandoned partial
Upload is cleaned up by the server.

Needs tuspy (pip install tuspy), which also brings requests. Exits 1 if anything
failed or was a Conflict.
"""

import argparse
import datetime
import hashlib
import json
import os
import re
import stat
import sys
import time
import unicodedata
from typing import List, NamedTuple
from urllib.parse import quote

CHUNK_SIZE = 64 * 1024 * 1024  # bytes per PATCH; tuspy holds one chunk in memory
RETRIES = 5  # per file, for dropped connections; each resumes from the server's offset
RETRY_DELAY = 10  # seconds


class LocalFile(NamedTuple):
    path: str  # relative to LOCAL_FOLDER, "/"-separated, as the OS spells it
    size: int
    mtime_ns: int


class Plan(NamedTuple):
    skip: List[str]
    upload: List[LocalFile]
    conflict: List[str]
    mkdir: List[str]


class IncompleteListing(Exception):
    """The tree listing failed partway."""


_RFC3339 = re.compile(
    r"^(\d{4})-(\d\d)-(\d\d)[Tt](\d\d):(\d\d):(\d\d)(?:\.(\d+))?([Zz]|[+-]\d\d:\d\d)$"
)


def parse_rfc3339_seconds(s):
    """Whole seconds since the Unix epoch of an RFC 3339 time."""
    m = _RFC3339.match(s)
    if not m:
        raise ValueError("not RFC 3339: %r" % s)
    year, month, day, hour, minute, sec = (int(g) for g in m.groups()[:6])
    tz = m.group(8)
    if tz in ("Z", "z"):
        offset = datetime.timedelta(0)
    else:
        sign = -1 if tz[0] == "-" else 1
        offset = sign * datetime.timedelta(hours=int(tz[1:3]), minutes=int(tz[4:6]))
    t = datetime.datetime(year, month, day, hour, minute, sec, tzinfo=datetime.timezone(offset))
    return int(t.timestamp())


def format_rfc3339(mtime_ns):
    """An mtime in nanoseconds since the epoch as RFC 3339 in UTC, as the server wants it."""
    secs, nanos = divmod(mtime_ns, 1_000_000_000)
    t = datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc) + datetime.timedelta(seconds=secs)
    return t.strftime("%Y-%m-%dT%H:%M:%S") + ".%09dZ" % nanos


def nfc(path):
    """The server stores and lists names NFC-normalized."""
    return unicodedata.normalize("NFC", path)


def parse_tree(lines):
    """Entries of a GET /tree response body, one JSON object per non-empty line."""
    for line in lines:
        if not line.strip():
            continue
        entry = json.loads(line)
        if "error" in entry:
            raise IncompleteListing(entry["error"])
        yield entry


def plan(local_files, local_folders, remote):
    """What to do for each local entry, given the remote tree listing of DESTINATION.

    local_folders are "/"-separated paths relative to LOCAL_FOLDER, like file paths.
    """
    remote_by_path = {e["path"]: e for e in remote}
    p = Plan(skip=[], upload=[], conflict=[], mkdir=[])
    for f in local_files:
        name = nfc(f.path)
        r = remote_by_path.get(name)
        if r is None:
            p.upload.append(f)
        elif (
            r["type"] == "file"
            and r["size"] == f.size
            and parse_rfc3339_seconds(r["mtime"]) == f.mtime_ns // 1_000_000_000
        ):
            p.skip.append(name)
        else:
            p.conflict.append(name)
    # Uploads create their parent folders, so only folders with nothing below them need
    # an explicit create-folder.
    parents = set()
    folders = [nfc(d) for d in local_folders]
    for path in [nfc(f.path) for f in local_files] + folders:
        parts = path.split("/")
        for n in range(1, len(parts)):
            parents.add("/".join(parts[:n]))
    for d in folders:
        r = remote_by_path.get(d)
        if r is not None and r["type"] != "folder":
            p.conflict.append(d)
        elif r is None and d not in parents:
            p.mkdir.append(d)
    return p


# --- everything below talks to the filesystem or the API ---


def walk_local(root):
    """Regular files and folders below root. Symlinks and special files are skipped."""
    files, folders = [], []
    for dirpath, dirnames, filenames in os.walk(root):
        rel_dir = os.path.relpath(dirpath, root)
        rel = (lambda n: n) if rel_dir == "." else (lambda n: rel_dir.replace(os.sep, "/") + "/" + n)
        for d in list(dirnames):
            if os.path.islink(os.path.join(dirpath, d)):
                print("skip     %s (symlink)" % rel(d), file=sys.stderr)
                dirnames.remove(d)
            else:
                folders.append(rel(d))
        for n in filenames:
            st = os.lstat(os.path.join(dirpath, n))
            if not stat.S_ISREG(st.st_mode):
                print("skip     %s (symlink or special file)" % rel(n), file=sys.stderr)
                continue
            files.append(LocalFile(rel(n), st.st_size, st.st_mtime_ns))
    return files, folders


def url_path(path):
    """A "/"-separated path, each segment percent-encoded."""
    return "/".join(quote(seg, safe="") for seg in path.split("/") if seg)


def join(dest, rel):
    return rel if not dest else dest + "/" + rel


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def remote_tree(session, api, dest):
    resp = session.get(api + "/tree/" + url_path(dest), stream=True)
    if resp.status_code == 404:
        return []  # DESTINATION doesn't exist yet
    if resp.status_code != 200:
        sys.exit("listing %s: HTTP %d %s" % (dest or "/", resp.status_code, resp.text.strip()))
    return list(parse_tree(resp.iter_lines()))


def upload(tus, local_root, dest, f):
    """Uploads f; returns the server's SHA-256. Raises TusCommunicationError."""
    from tusclient.exceptions import TusCommunicationError

    os_path = os.path.join(local_root, *f.path.split("/"))
    uploader = tus.uploader(
        file_path=os_path,
        chunk_size=CHUNK_SIZE,
        metadata={
            "path": join(dest, f.path),
            "mtime": format_rfc3339(f.mtime_ns),
            "sha256": sha256_of(os_path),
        },
    )
    uploader.set_url(uploader.create_url())  # 409 here is a Conflict
    failures = 0
    while uploader.offset < f.size:
        try:
            uploader.upload_chunk()
            failures = 0
        except TusCommunicationError as e:
            # An HTTP status (409 Conflict, 460 checksum mismatch, ...) is final; only a
            # dropped connection is retried, resuming at the server's offset.
            if e.status_code is not None or failures >= RETRIES:
                raise
            failures += 1
            print("  connection lost (%s); resuming in %d s" % (e, RETRY_DELAY), file=sys.stderr)
            time.sleep(RETRY_DELAY)
            uploader.offset = uploader.get_offset()
    # The response that completes the Upload carries the server's hash. A 0-byte file
    # completes on creation, whose response tuspy doesn't keep.
    return uploader.request.response_headers.get("upload-sha256") if uploader.request else None


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("local_folder")
    ap.add_argument("destination", help='folder path relative to the Upload Root ("" for the root)')
    ap.add_argument("--dry-run", action="store_true", help="print the plan, change nothing")
    args = ap.parse_args()
    api = os.environ["NAS_API"].rstrip("/")
    token = os.environ["NAS_TOKEN"]
    dest = args.destination.strip("/")

    import requests
    from tusclient import client
    from tusclient.exceptions import TusCommunicationError

    session = requests.Session()
    session.headers["Authorization"] = "Bearer " + token
    files, folders = walk_local(args.local_folder)
    p = plan(files, folders, remote_tree(session, api, dest))

    for name in p.skip:
        print("skip     %s (same size and mtime)" % name)
    for name in p.conflict:
        print("CONFLICT %s (exists on the NAS and differs; not overwritten)" % name)
    if args.dry_run:
        for d in p.mkdir:
            print("mkdir    %s" % d)
        for f in p.upload:
            print("upload   %s (%d bytes)" % (nfc(f.path), f.size))
        return 0 if not p.conflict else 1

    made = uploaded = failed = 0
    if not files and not folders and dest:
        p.mkdir.append("")  # an empty LOCAL_FOLDER still gets its DESTINATION
    for d in p.mkdir:
        resp = session.put(api + "/folders/" + url_path(join(dest, d)))
        if resp.status_code in (200, 201):
            made += 1
            print("mkdir    %s" % (d or "(destination)"))
        else:
            failed += 1
            print("FAILED   mkdir %s: HTTP %d %s" % (d or "(destination)", resp.status_code, resp.text.strip()))

    tus = client.TusClient(api + "/uploads/", headers={"Authorization": "Bearer " + token})
    conflicts = len(p.conflict)
    for f in p.upload:
        name = nfc(f.path)
        try:
            sum_ = upload(tus, args.local_folder, dest, f)
        except TusCommunicationError as e:
            if e.status_code == 409:
                conflicts += 1
                print("CONFLICT %s (appeared on the NAS meanwhile; not overwritten)" % name)
            else:
                failed += 1
                body = (e.response_content or b"").decode("utf-8", "replace").strip()
                print("FAILED   %s: %s %s" % (name, e.status_code or e, body))
            continue
        uploaded += 1
        print("uploaded %s (%d bytes%s)" % (name, f.size, ", sha256 " + sum_ if sum_ else ""))

    print(
        "%d uploaded, %d folders created, %d skipped, %d conflicts, %d failed"
        % (uploaded, made, len(p.skip), conflicts, failed)
    )
    return 0 if not conflicts and not failed else 1


if __name__ == "__main__":
    sys.exit(main())
