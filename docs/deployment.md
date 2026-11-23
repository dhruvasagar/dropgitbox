# Deploying dropgitbox

A dropgitbox setup is a **hub and spoke**: one server node holds the
authoritative copy of every synced folder, and every client device syncs
with it. Devices never talk to each other directly.

```
                 ┌──────────────────────────────┐
                 │ server node                  │
   laptop ─────► │ dropgitbox-server.service    │ ◄───── desktop
                 │ /var/lib/dropgitbox/*.git    │
   phone/other ► │ HTTPS :443 (or a VPN/tailnet)│ ◄───── work machine
                 └──────────────────────────────┘
```

Every device opens **outbound** HTTP(S) connections to the server:

- **git smart HTTP** (`/git/<repo>.git/...`) to fetch and push. These are
  short requests, but a push of a large file is one large request body.
- **an event stream** (`/api/v1/repos/<repo>/events`). This is one
  long-lived Server-Sent Events connection per folder, held open with a
  heartbeat every 25s. It lets the server tell devices about new changes
  the moment they are pushed.

So clients need no open ports. They work behind NAT, on hotel Wi-Fi, or
on mobile data. **Only the server must be reachable from every device**,
and that is what this guide is about. If a device is offline, its changes
keep being committed locally and sync when it reconnects; the full history
is kept either way.

## 1. Choose how devices reach the server

| Option | Best for | Open ports | TLS |
| --- | --- | --- | --- |
| **A. Tailscale / WireGuard (recommended)** | Personal use, a home server | None | Encrypted by the VPN |
| **B. Public server + reverse proxy** | A VPS, or sharing with people not on your VPN | 443 | Caddy / nginx + Let's Encrypt |
| **C. Home server, port forward** | No VPS, no VPN | 443 on your router | Caddy + Let's Encrypt, plus dynamic DNS |
| **D. Cloudflare Tunnel** | No open ports, no VPN client | None | Cloudflare |
| **E. LAN only** | All devices on one network | 8080 on the LAN | None (trusted LAN) |

### A. Tailscale (or any WireGuard mesh): recommended

Install Tailscale on the server and on every device. The server is then
reachable at a stable private name such as `box.tail1234.ts.net` from
anywhere, and nothing is exposed to the internet.

```sh
# on the server
sudo tailscale up
sudo tailscale serve --bg 8080        # HTTPS on https://box.tail1234.ts.net -> :8080
```

`tailscale serve` provisions a real TLS certificate for the machine's
tailnet name. Then set `public_url = "https://box.tail1234.ts.net"` in
`/etc/dropgitbox/config.toml`. To keep the plain port off other networks
too, set `addr = "127.0.0.1:8080"` (Tailscale serve proxies to localhost).

Plain WireGuard works the same way: bind to the WireGuard interface address
(`addr = "10.8.0.1:8080"`) and use `public_url = "http://10.8.0.1:8080"`.
The tunnel already encrypts the traffic.

### B. Public server behind a reverse proxy

Run dropgitbox on localhost and put a TLS-terminating proxy in front of it.

`/etc/dropgitbox/config.toml`:

```toml
[server]
addr = "127.0.0.1:8080"
data_dir = "/var/lib/dropgitbox"
public_url = "https://box.example.com"
```

**Caddy** obtains and renews certificates automatically:

```caddyfile
box.example.com {
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1   # stream push notifications immediately
	}
}
```

**nginx**:

```nginx
server {
    listen 443 ssl http2;
    server_name box.example.com;
    # ssl_certificate ... (e.g. from certbot)

    client_max_body_size 0;          # pushes can be large; don't cap them
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_buffering off;         # needed for the event stream
        proxy_request_buffering off; # stream large pushes
        proxy_read_timeout 1h;       # event streams are long-lived
    }
}
```

### C. Home server with port forwarding

Same as **B**, plus:

1. Forward TCP 443 (and 80, for certificate issuance) on your router to the
   server.
2. If your home IP changes, use dynamic DNS: your DNS provider's API, a
   DDNS service, or a `ddclient`-style updater. Point `box.example.com` at
   your public IP.
3. Behind carrier-grade NAT (common on mobile and some fibre ISPs) port
   forwarding cannot work. Use **A** or **D** instead.

### D. Cloudflare Tunnel

`cloudflared tunnel` publishes the server without opening ports:

```sh
cloudflared tunnel --url http://127.0.0.1:8080   # or a named tunnel with your domain
```

Server-Sent Events work through tunnels. Note that Cloudflare limits
request bodies (100 MB on the free plan), so **a push larger than that,
such as a big file, fails**. Prefer **A** if you sync large files.

### E. LAN only

```toml
[server]
addr = ":8080"
public_url = "http://homeserver.local:8080"
```

Traffic, including the device token, is unencrypted. Only do this on a
network you trust.

### Built-in TLS

If you already have a certificate (for example from `tailscale cert` or
certbot), the server can serve HTTPS itself:

```toml
[server]
addr = ":8443"
public_url = "https://box.example.com:8443"
tls_cert = "/etc/dropgitbox/tls/cert.pem"
tls_key  = "/etc/dropgitbox/tls/key.pem"
```

With the packaged service, pass the key in with `LoadCredential=` rather
than making it readable. See the comments in `dropgitbox-server.service`.

## 2. Set up the server node

```sh
# Debian/Ubuntu (download the .deb from the latest release)
sudo apt install ./dropgitbox_*_amd64.deb
# Fedora/RHEL
sudo dnf install ./dropgitbox-*.x86_64.rpm
# anything else with systemd
curl -fsSL https://raw.githubusercontent.com/dhruvasagar/dropgitbox/main/install.sh \
  | sudo sh -s -- --server --public-url https://box.example.com
```

With a package, set `public_url` in `/etc/dropgitbox/config.toml`, then:

```sh
sudo systemctl enable --now dropgitbox-server
journalctl -u dropgitbox-server -f
```

The service runs as an unprivileged, sandboxed system user, and keeps all
data in `/var/lib/dropgitbox`: one bare git repository per folder, plus
`devices.json`, the device registry, which holds only token hashes. To back
up the server, back up that directory.

## 3. Add devices

Every device gets its own token, so a lost laptop can be revoked without
touching the others. On the server:

```console
$ sudo dropgitbox device add laptop
Registered device "laptop".

On that device, install dropgitbox and run:

  dropgitbox join dgb1.eyJzZXJ2ZXIiOiJodHRwczovL2JveC5leGFtcGxlLmNvbSIs...
```

The invite contains the server URL, the device name, and the device's
token. **Treat it like a password** and send it over a private channel.
On the new device:

```sh
curl -fsSL https://raw.githubusercontent.com/dhruvasagar/dropgitbox/main/install.sh | sh
dropgitbox join dgb1.eyJ...          # writes ~/.config/dropgitbox/{config.toml,token}
dropgitbox repos                     # what is on the server
dropgitbox setup -repo docs -dir ~/Box
systemctl --user enable --now dropgitbox   # or: install.sh --service (Linux & macOS)
```

Shortcut for your second, third, ... device: copy the `[[folders]]` list
from an existing device's `~/.config/dropgitbox/config.toml` (it is
dotfiles-friendly: no secrets, and paths use `~`). On the new device, run
`join` and then `dropgitbox watch`, which sets up every listed folder.

Manage devices on the server:

```sh
sudo dropgitbox device list              # last seen, last push, sync status, version
sudo dropgitbox device show laptop       # per-folder detail: state, conflicts, last error
sudo dropgitbox device rotate laptop     # new token and invite; the old one stops working
sudo dropgitbox device revoke laptop     # takes effect immediately, no restart
```

Revoking stops a device from syncing. It does not erase the copy already on
that device.

## 4. Hardening a public server

dropgitbox already rejects requests without a valid token and blocks
clients that keep presenting bad ones (see the Security section of the
README). On a public server, also do the following.

**Make sure the server sees real client addresses.** With the proxy on
the same machine and dropgitbox on `127.0.0.1`, the defaults are correct.
Caddy sets `X-Forwarded-For` automatically; for nginx, include the
`proxy_set_header X-Forwarded-For` line above. If the proxy runs on
another machine, list its address:

```toml
[server]
trusted_proxies = ["10.0.0.5"]
```

Check by sending a bad token from outside and reading the log. The `ip=`
field must show your address, not the proxy's:

```sh
curl -s -H 'Authorization: Bearer dgb_wrong' https://box.example.com/api/v1/repos
journalctl -u dropgitbox-server | grep 'auth failed'
# ... level=WARN msg="auth failed: invalid token" ip=203.0.113.50 method=GET path=/api/v1/repos user_agent=curl/8.x
```

**Bind to localhost** (`addr = "127.0.0.1:8080"`) so the only way in is
through the proxy and its TLS.

**Optionally, ban at the firewall with fail2ban.** The built-in limiter
blocks for 15 minutes; fail2ban can ban for longer and drop the traffic
before it reaches the proxy.

`/etc/fail2ban/filter.d/dropgitbox.conf`:

```ini
[Definition]
failregex = msg="auth failed: invalid token" ip=<HOST>
journalmatch = _SYSTEMD_UNIT=dropgitbox-server.service
```

`/etc/fail2ban/jail.d/dropgitbox.local`:

```ini
[dropgitbox]
enabled  = true
backend  = systemd
filter   = dropgitbox
maxretry = 10
findtime = 10m
bantime  = 1d
# The proxy terminates connections, so ban on ports 80/443.
port     = http,https
```

**Keep tokens tight.** Give every device its own token
(`dropgitbox device add`). Rotate one you suspect has leaked
(`dropgitbox device rotate NAME`, then `join -force` on the device), and
revoke lost devices (`dropgitbox device revoke NAME`). Check
`dropgitbox device list` now and then for devices you don't recognise or
that have gone quiet.

## 5. Operations

- **Device health.** `sudo dropgitbox device list` shows each device's last
  contact, last push, client version, and whether it is in sync, paused on
  a conflict, or failing. `sudo dropgitbox device show NAME` has the
  per-folder detail.
- **Logs.** Server: `journalctl -u dropgitbox-server`. Client:
  `journalctl --user -u dropgitbox` on Linux, `~/Library/Logs/dropgitbox.log`
  on macOS.
- **Conflicts.** The client daemon pauses that folder and sends a desktop
  notification. Run `dropgitbox status`, then `dropgitbox resolve`.
- **Headless clients.** On a machine nobody logs into, run
  `sudo loginctl enable-linger $USER` so the user service keeps running.
- **Upgrades.** Install the new package, or re-run `install.sh`, then
  restart the services. Devices and the server can be upgraded
  independently.
