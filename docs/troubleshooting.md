# Troubleshooting

Start with `dropgitbox status` on the device and `sudo dropgitbox device
list` on the server: between them they usually say what's wrong. For more
detail, read the logs (`journalctl --user -u dropgitbox` on a Linux device,
`~/Library/Logs/dropgitbox.log` on macOS, `journalctl -u dropgitbox-server`
on the server), or run the daemon in a terminal with `dropgitbox watch -v`.

## Changes don't reach another device

1. Is the daemon running on both devices? `dropgitbox status` says
   `Daemon: running`, or `not running`.
2. What's the folder's `State`? Anything other than `in sync` or `syncing`
   is explained [in the device guide](devices.md#checking-status).
3. Run `dropgitbox sync` on the device that made the change, then on the
   other. It prints each folder's outcome.
4. On the server, `sudo dropgitbox device list` shows when each device last
   pushed and whether it's behind.

## Device messages

| Message | Cause and fix |
| --- | --- |
| `no server configured: run dropgitbox join INVITE first` | This device hasn't joined a server, or uses another config file. Run `join`, or check `dropgitbox config` |
| `not a dropgitbox folder (run dropgitbox setup first)` | The current directory isn't in a synced folder. `cd` into one, or pass `--dir` |
| `a dropgitbox daemon is already running (pid N)` | Only one daemon runs per user; the service is probably running already. Use `dropgitbox status`, or stop the service first |
| `invite is corrupted (was it copied completely?)` | Part of the invite is missing, often from a line break. Copy it again as one line |
| `invite has an invalid server URL` | The server's `public_url` is wrong. Fix it on the server, then `sudo dropgitbox device rotate NAME` for a new invite |
| `checking the invite with URL: ... connection refused` (or a timeout) | The device can't reach the server at that URL. Check the server is running and the URL is reachable from this device (VPN up? proxy configured?). See [deployment](deployment.md) |
| `the server rejected the token` during `join` | The invite was replaced or revoked after it was made. Get a new one |
| `this device is already joined to URL; pass --force` | Add `--force` to switch servers or tokens |
| `... sets client.token inline; remove it` | Delete the `token` line from `[client]` in the config, then `join` again |
| `token file ... is readable by other users; run: chmod 600 ...` | Run that `chmod` |
| `cannot use the folder name "..." as a repo name; pass --repo` | Repository names allow letters, digits, `.`, `_` and `-`. Choose one with `--repo` |
| `nested git repositories can't be synced: ... contains git repositories` | There's a git repository inside the folder. Move it out or delete its `.git`; see [nesting](devices.md#folders-cant-be-nested) |
| `nested git repositories can't be synced: ... is inside the synced folder ...` | Sync the parent folder instead, or move this folder out of it |
| `setup: DIR must be the root of its git repository` | DIR is inside an existing git repository but not at its top. Choose the repository's top, or a folder outside it |
| `unresolved merge conflict; syncing is paused` | See [conflicts](conflicts.md) |
| `not resolved, nothing changed: FILE: still has conflict markers` | `resolve` found `<<<<<<<` or `>>>>>>>` lines. Finish editing, pick a side with `--ours` or `--theirs`, or use `--force` if the lines belong there |
| `not resolved, nothing changed: FILE: ... binary` (or `deleted on the server`) | The file has no markers to edit, so plain `resolve` won't decide for you. Pick a side, or name the file to keep it as it is: `dropgitbox resolve FILE`. See [conflicts](conflicts.md#resolving-it) |
| `FILE has no conflict (conflicted: ...)` | The path isn't conflicted, or was already resolved. Paths are relative to the current directory; the message lists the ones left |
| `FILE is outside the synced folder` | A path argument points outside the folder. Paths are relative to the current directory, or to `--dir` |
| `no conflict to resolve` | The folder isn't paused. Nothing to do |
| `the dropgitbox daemon is not running` | `status --json` and `events` need the daemon. Start it |
| `gave up after 5 attempts: the server keeps changing underneath us` | Other devices pushed constantly during the sync. It's retried automatically |
| `no desktop notification mechanism found` | No desktop session, or `notify-send` isn't installed (`libnotify-bin` on Debian and Ubuntu). Other notification channels still work |
| `config FILE: unknown setting(s): ...` | A typo in the config. Compare with [configuration](configuration.md) |

## Device states

| `status` shows | Cause and fix |
| --- | --- |
| `ACCESS DENIED` | The device's token was revoked or rotated on the server. [Get a new invite](devices.md#when-the-token-changes), `join --force`, and restart the daemon |
| `OFFLINE` | Syncing has failed for over 10 minutes: no network, the server is down, or a proxy is misconfigured. The device keeps committing locally and catches up later. `dropgitbox sync` shows the current error |
| `STOPPED` | The folder can't sync until you act; the message says why. After fixing it, restart the daemon |
| `retrying after error` | One or more syncs failed recently and are being retried. Usually a passing network problem |

## Server problems

| Symptom | Cause and fix |
| --- | --- |
| `device add` warns that the URL `did not answer` | `public_url` doesn't reach this server from here: wrong host or port, or the proxy isn't forwarding. The invite was still made; fix `public_url`, then `device rotate NAME` |
| `server.public_url is not set` at startup | Set it in the config: invites need it |
| `device already exists` | Use `device rotate NAME` for a new invite, or `revoke` it first |
| `permission denied` reading the data directory | Run `device` commands with `sudo`, as the server's own files aren't readable by you |
| `tls_cert and tls_key must be set together` | Set both, or neither |
| `the shared token must be at least 24 characters` | Use a longer one: `openssl rand -hex 32` |
| HTTP 429 `too many failed authentication attempts` | That address presented 20 bad tokens in 5 minutes and is blocked for 15. Fix the client's token; the block lifts by itself |
| Large pushes fail behind Cloudflare | Cloudflare limits request bodies (100 MB on the free plan). Use another way to reach the server for large files; see [deployment](deployment.md#d-cloudflare-tunnel) |
| Pushes fail behind nginx with `413` | Set `client_max_body_size 0;` |
| Devices only sync every few minutes | The proxy buffers the event stream. Disable buffering for it (`proxy_buffering off` in nginx, `flush_interval -1` in Caddy) |
| All clients show the proxy's address in the log | Add the proxy to `server.trusted_proxies`; see [security](security.md#real-client-addresses) |
