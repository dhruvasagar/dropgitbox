# dropgitbox documentation

New here? Read [how it works](how-it-works.md), then try the
[local demo](demo.md), which runs a server and two devices on one machine.

**Setting up**

- [Deployment](deployment.md): install the server and make it reachable
  (Tailscale, a reverse proxy, TLS), and harden it for the internet.
- [Running the server](server.md): settings, data and backups, inviting,
  checking on and revoking devices, monitoring, the HTTP API.
- [Using a device](devices.md): joining, choosing folders, the sync
  daemon, status, tokens, moving and removing folders.

**Day to day**

- [Conflicts](conflicts.md): what happens when two devices change the same
  thing, how to resolve it, and how syncing resumes.
- [History and restoring](history.md): browse history, bring back old
  versions and deleted files, use plain git.
- [Notifications and status](notifications.md): desktop notifications,
  status bars, the event stream, hook commands.
- [Troubleshooting](troubleshooting.md): error messages and what to do.

**Reference**

- [Command line](cli.md): conventions shared by all commands, help, the man
  page, shell completion.
- [Command reference](cli-reference.md): every command and flag.
- [Configuration](configuration.md): every setting, file locations,
  environment variables.
- [Security](security.md): what the server protects against, and how.
- [Development](development.md): building, testing, CI and releasing.
