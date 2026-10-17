# Reachable only through a Tailscale sidecar with HTTPS

The API container shares the network namespace of its own Tailscale sidecar container, publishes no host port, and is served over HTTPS by `tailscale serve` on a `*.ts.net` name. The tailnet policy lets only the owner's devices reach `tag:nas-file-api`. Every request still needs a Credential, so a compromised tailnet device gets nothing on its own.

This deliberately differs from `dreambig-nas-mcp` (its ADR-0002), which uses the Tailscale DSM package and plain HTTP on a host port. This API has a browser UI. Over `http://<nas-ip>` the page is not a secure context (no `crypto.subtle`), and its bearer token would cross the LAN in plaintext. A host port would also expose the API on every NAS interface, LAN included.

## Considered Options

- **Host port plus the NAS firewall allowing only the Tailscale interface**: rejected; the isolation would rest on firewall rules that are easy to get wrong, and there would still be no HTTPS.
- **Public internet through the DSM reverse proxy or a tunnel**: rejected; it exposes a write API to the internet when every Caller can run Tailscale.
