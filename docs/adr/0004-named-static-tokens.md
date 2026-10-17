# Named static bearer tokens in .env

Each Caller has a name, a static token generated with `openssl rand -hex 32` and stored in plaintext in the Project's `.env`, and an optional Scope. Tokens are compared in constant time. This is the same workflow as `dreambig-nas-mcp`, managed from File Station or Container Manager, with no Docker or SSH needed to issue a token. Unlike that project's single token, each Caller still has its own token, so one can be revoked without affecting the others, Scope can be set per Caller, and the audit log shows which Caller did what.

## Considered Options

- **Hashed tokens issued by a `gen-token` subcommand**: rejected; issuing one needs Docker on the client or a shell on the NAS.
- **One shared static token**: rejected; it gives up per-Caller revocation, Scope and audit names.
- **Mutual TLS or Tailscale identity alone**: rejected; mutual TLS is painful in browsers and scripts, and Tailscale identity alone would be a single layer of protection.
