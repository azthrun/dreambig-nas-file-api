# NAS File API

A small service on a Synology NAS that lets trusted Callers put files and folder trees into one dedicated folder on the NAS, and see what is already there.

## Language

**Upload Root**:
The single dedicated NAS folder the service is allowed to write into; every path the service knows about is relative to it.
_Avoid_: share, mount, base dir, storage

**Caller**:
A person or script that talks to the service under its own Credential.
_Avoid_: user, client, account

**Credential**:
The secret that identifies exactly one Caller and can be revoked without affecting any other Caller.
_Avoid_: password, login, token (when meaning the concept rather than its encoding)

**Scope**:
The subtree of the Upload Root a Caller may write to and list; defaults to the whole Upload Root.
_Avoid_: permission, prefix, jail

**Destination Path**:
The path, relative to the Upload Root, where a Caller wants a file to end up.
_Avoid_: target, location, remote path

**Upload**:
The transfer of one file to one Destination Path, which may span many requests and survive dropped connections before it completes.
_Avoid_: transfer, job, session

**Staging Area**:
The hidden place inside the Upload Root's filesystem where an Upload's data waits until it completes and is verified; never visible when listing.
_Avoid_: temp dir, cache, incoming

**Hidden Entry**:
Anything in the Upload Root that belongs to the NAS or the service rather than to Callers (DSM's `@`/`#` folders, the Staging Area); never listed and never a valid Destination Path.
_Avoid_: system file, ignored file

**Conflict**:
An Upload whose Destination Path already holds a file; the service always refuses it rather than overwriting or renaming.
_Avoid_: collision, duplicate

**Folder Upload**:
A set of Uploads that together rebuild a local folder tree under a chosen Destination Path; it is a Caller-side notion, not something the service tracks as a unit.
_Avoid_: batch, directory sync, archive upload
