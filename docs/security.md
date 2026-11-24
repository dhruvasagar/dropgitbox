# Security

The server is designed to be exposed publicly, behind TLS. This page
describes what it protects against and how. For setting up TLS, proxies and
firewall bans, see [hardening a public server](deployment.md#4-hardening-a-public-server).

## The model

dropgitbox is for **one person with several devices**. Every registered
device can read and write every folder on the server; there are no
per-folder permissions. Each device has its own token, so a lost or
compromised device can be cut off without touching the others.

History can't be destroyed through the server, so even a compromised
device can only add commits: every earlier version stays recoverable.

## Authentication

- **Every route except `/healthz` requires a valid token.** This is checked
  before anything else, so without a token you can't even learn which
  folders exist.
- **Device tokens** are 256-bit random values, stored on the server only as
  SHA-256 hashes. An optional shared token (`server.token_file`) must be at
  least 24 characters; per-device tokens are better, because they can be
  revoked one at a time.
- **Revocation is immediate.** `device revoke` and `device rotate` take
  effect on the next request and close the device's open event streams.
- **Brute-force protection.** A client that presents 20 invalid tokens
  within 5 minutes is blocked for 15 minutes (HTTP 429). IPv6 clients are
  grouped per /64. Requests without credentials, such as git's first
  anonymous probe, don't count. Every failure is logged with the client's
  address, so tools like fail2ban can also block at the firewall.

## Real client addresses

Behind a reverse proxy, every request seems to come from the proxy. The
server believes `X-Forwarded-For` only from `server.trusted_proxies`
(loopback by default, for a proxy on the same machine), and then uses the
rightmost address that isn't a trusted proxy, so clients can't spoof their
address to dodge the brute-force limits. Never list addresses that
untrusted clients can connect from.

## A minimal git surface

Only git's three smart-HTTP endpoints are served. Raw repository files,
git's legacy "dumb" protocol, and paths outside a repository return 404.
For every repository, whatever its own config says, the server refuses
force-pushes (`receive.denyNonFastForwards`) and branch deletion
(`receive.denyDeletes`), and checks the integrity of everything pushed
(`receive.fsckObjects`).

## Hardened HTTP

- Header and idle timeouts, and a 64 KiB header limit. There's no overall
  request timeout, since large pushes and event streams are legitimately
  long.
- TLS 1.2 or newer when serving TLS directly.
- Size limits on API request bodies, and a per-device cap on open event
  streams.
- No internal details in error responses.
- Conservative security headers: `X-Content-Type-Options: nosniff`,
  `Cache-Control: no-store`, `frame-ancestors 'none'`.

## The packaged service

The systemd unit runs the server as an ephemeral, unprivileged user
(`DynamicUser=`), with a read-only view of the system, no access to home
directories, and no capabilities. A TLS key can be passed in with
`LoadCredential=`, so it stays readable only by root on disk; see the
comments in `dropgitbox-server.service`.

## Devices

- Tokens live in files with mode 0600. dropgitbox refuses a token file
  that other users can read.
- git gets the token through the environment, never in a URL,
  `.git/config` or a command line, so it doesn't show up in process
  listings or in the folder.
- `join` and `setup` warn when a server URL uses plain `http://`, which
  sends the token and files unencrypted.
- The daemon's control socket is mode 0600, in a 0700 directory owned by
  you.

## If a device is lost or a token leaks

```sh
sudo dropgitbox device revoke laptop     # lost: cut it off
sudo dropgitbox device rotate laptop     # leaked: new token for a device you still have
```

Then check `sudo dropgitbox device list` for devices you don't recognise.
Revoking stops syncing; it doesn't erase the copy already on the device.
