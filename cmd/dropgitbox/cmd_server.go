package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/config"
	"github.com/dhruvasagar/dropgitbox/internal/server"
)

// serverSettings merges flags, environment and the [server] config table.
type serverSettings struct {
	cfg     *config.Config
	addr    string
	dataDir string
	url     string
	token   string
	noAuth  bool
	tlsCert string
	tlsKey  string
	trusted []netip.Prefix
}

func cmdServer(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("server")
	addr := fs.String("addr", "", `listen address (config: server.addr, default ":8080")`)
	data := fs.String("data", "", "data directory (config: server.data_dir, default $XDG_DATA_HOME/dropgitbox)")
	token := fs.String("token", "", "optional shared token accepted besides device tokens ($DROPGITBOX_TOKEN, config: server.token)")
	noAuth := fs.Bool("no-auth", false, "disable authentication; trusted networks only (config: server.no_auth)")
	tlsCert := fs.String("tls-cert", "", "serve HTTPS with this certificate (config: server.tls_cert)")
	tlsKey := fs.String("tls-key", "", "private key for -tls-cert (config: server.tls_key)")
	verbose := fs.Bool("v", false, "verbose logging")
	parse(fs, args)

	st, err := loadServerSettings(*cfgPath, *data)
	if err != nil {
		return err
	}
	if isSet(fs, "addr") {
		st.addr = *addr
	}
	if isSet(fs, "token") {
		st.token = *token
	}
	if isSet(fs, "no-auth") {
		st.noAuth = *noAuth
	}
	if isSet(fs, "tls-cert") {
		st.tlsCert = *tlsCert
	}
	if isSet(fs, "tls-key") {
		st.tlsKey = *tlsKey
	}
	if (st.tlsCert == "") != (st.tlsKey == "") {
		return errors.New("server: tls_cert and tls_key must be set together")
	}

	log := newLogger(*verbose)
	srv, err := server.New(server.Options{
		DataDir: st.dataDir, Token: st.token, NoAuth: st.noAuth, Logger: log, TrustedProxies: st.trusted,
	})
	if err != nil {
		return err
	}
	if st.noAuth {
		log.Warn("authentication is DISABLED; anyone who can reach this server can read and write all data")
	} else if n, err := srv.NumDevices(); err != nil {
		return err
	} else if n == 0 && st.token == "" {
		log.Warn("no devices registered yet; add one with `dropgitbox device add NAME`")
	}
	if st.url == "" {
		log.Warn("server.public_url is not set: `dropgitbox device add` needs it to build invites (or pass -url each time)",
			"config", configHint(st.cfg))
	} else if err := client.ValidServerURL(st.url); err != nil {
		log.Warn("server.public_url is invalid; invites will be rejected until it is fixed", "err", err)
	}

	hs := &http.Server{
		Addr:    st.addr,
		Handler: srv.Handler(),
		// No overall read/write timeouts: pushes of large files and event
		// streams are legitimately long. Slow or idle clients are bounded by
		// these instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		// Long-lived event streams end when the server shuts down.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		if st.tlsCert != "" {
			errc <- hs.ListenAndServeTLS(st.tlsCert, st.tlsKey)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	log.Info("dropgitbox server listening", "addr", st.addr, "data", st.dataDir,
		"tls", st.tlsCert != "", "auth", !st.noAuth, "config", st.cfg.Path(), "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = hs.Shutdown(shutdownCtx)
	srv.Close() // flush device activity
	return err
}

// loadServerSettings resolves server settings from the config file and
// environment; dataFlag (if non-empty) overrides the data directory.
func loadServerSettings(cfgPath, dataFlag string) (*serverSettings, error) {
	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		return nil, err
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "dropgitbox: warning:", w)
	}
	sc := cfg.Server
	st := &serverSettings{
		cfg:     cfg,
		addr:    sc.Addr,
		url:     sc.PublicURL,
		noAuth:  sc.NoAuth,
		tlsCert: cfg.Resolve(sc.TLSCert),
		tlsKey:  cfg.Resolve(sc.TLSKey),
	}
	if st.addr == "" {
		st.addr = ":8080"
	}
	for _, c := range sc.TrustedProxies {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			a, aerr := netip.ParseAddr(c)
			if aerr != nil {
				return nil, fmt.Errorf("server.trusted_proxies: %q is not an IP or CIDR", c)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		st.trusted = append(st.trusted, p)
	}
	switch {
	case dataFlag != "":
		st.dataDir = config.ExpandHome(dataFlag)
	case sc.DataDir != "":
		st.dataDir = cfg.Resolve(sc.DataDir)
	default:
		if st.dataDir, err = config.DefaultDataDir(); err != nil {
			return nil, err
		}
	}
	if t := os.Getenv(config.EnvToken); t != "" {
		st.token = t
	} else if st.token, err = cfg.ServerToken(); err != nil {
		return nil, err
	}
	return st, nil
}

func cmdDevice(ctx context.Context, args []string) error {
	const help = `usage:
  dropgitbox device add NAME [-url URL]   register a device and print its invite
  dropgitbox device list                  list devices with last activity and sync status
  dropgitbox device show NAME             per-folder detail for one device
  dropgitbox device rotate NAME [-url URL] replace a device's token (prints a new invite)
  dropgitbox device revoke NAME           revoke a device's access immediately

Run these on the server node, as the server's user or with sudo.`
	// Flags may come before or after the subcommand.
	fs, cfgPath := newFlags("device")
	data := fs.String("data", "", "server data directory (config: server.data_dir)")
	pubURL := fs.String("url", "", "URL devices use to reach the server (config: server.public_url)")
	pos := parse(fs, args)
	if len(pos) == 0 {
		return errors.New(help)
	}
	sub, pos := pos[0], pos[1:]

	st, err := loadServerSettings(*cfgPath, *data)
	if err != nil {
		return err
	}
	if isSet(fs, "url") {
		st.url = *pubURL
	}
	oneName := func(usage string) (string, error) {
		if len(pos) != 1 {
			return "", errors.New("usage: dropgitbox device " + usage)
		}
		return pos[0], nil
	}
	needURL := func() error {
		if st.url == "" {
			return errors.New("the server's public URL is unknown; pass -url https://box.example.com or set server.public_url in " + configHint(st.cfg))
		}
		if err := client.ValidServerURL(st.url); err != nil {
			return fmt.Errorf("invalid server URL (-url or server.public_url): %w", err)
		}
		return nil
	}

	switch sub {
	case "add":
		name, err := oneName("add NAME [-url URL]")
		if err != nil {
			return err
		}
		if err := needURL(); err != nil {
			return err
		}
		token, err := server.AddDevice(st.dataDir, name)
		if err != nil {
			return err
		}
		fmt.Printf("Registered device %q.\n\n", name)
		printInvite(st.url, token, name)
		checkReachable(ctx, st.url)
		return nil

	case "rotate":
		name, err := oneName("rotate NAME [-url URL]")
		if err != nil {
			return err
		}
		if err := needURL(); err != nil {
			return err
		}
		token, err := server.RotateDevice(st.dataDir, name)
		if err != nil {
			return err
		}
		fmt.Printf("Issued a new token for %q; the old one no longer works.\n\n", name)
		printInvite(st.url, token, name)
		fmt.Println("(On that device run the join command with -force to replace the old token. Its folders stay as they are.)")
		checkReachable(ctx, st.url)
		return nil

	case "list", "ls":
		return listDevices(ctx, st)

	case "show":
		name, err := oneName("show NAME")
		if err != nil {
			return err
		}
		return showDevice(ctx, st, name)

	case "revoke", "remove", "rm":
		name, err := oneName("revoke NAME")
		if err != nil {
			return err
		}
		if err := server.RevokeDevice(st.dataDir, name); err != nil {
			return err
		}
		fmt.Printf("Revoked %q. Its token no longer works and its open connections are closed;\nthe files already on that device stay there.\n", name)
		return nil
	}
	return fmt.Errorf("unknown device command %q\n\n%s", sub, help)
}

// checkReachable warns, without failing, when the invite's URL does not
// answer like a dropgitbox server from here. It catches a wrong host or
// port before the invite is sent. It can't prove that other devices can
// reach it, and a server that isn't up yet (DNS, proxy) is fine.
func checkReachable(ctx context.Context, serverURL string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	u := strings.TrimRight(serverURL, "/") + "/healthz"
	warn := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "\nWarning: "+format+"\n", a...)
		fmt.Fprintln(os.Stderr, "The invite was still created. If the URL is wrong, fix server.public_url (or -url) and run `dropgitbox device rotate NAME`.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		warn("cannot check %s: %v", serverURL, err)
		return
	}
	req.Header.Set("User-Agent", "dropgitbox/"+version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		warn("%s did not answer from this machine (%v).\nIs the server running, and is the URL right?", serverURL, err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		warn("%s answered %s, which doesn't look like a dropgitbox server.\nCheck the URL, and that your reverse proxy forwards it to dropgitbox.", serverURL, resp.Status)
	}
}

func printInvite(serverURL, token, name string) {
	inv := client.Invite{Server: strings.TrimRight(serverURL, "/"), Token: token, Device: name}
	fmt.Printf(`On that device, install dropgitbox and run:

  dropgitbox join %s

The invite contains the device's access token: send it over a private
channel. To cut the device off later: dropgitbox device revoke %s
`, inv.Encode(), name)
}

// deviceStatus summarises a device's folders in a few words.
func deviceStatus(ctx context.Context, st *serverSettings, act *server.DeviceActivity) string {
	if act == nil || len(act.Repos) == 0 {
		return "never synced"
	}
	var conflicts, errs, behind, ok []string
	for _, name := range sortedKeys(act.Repos) {
		r := act.Repos[name]
		head, _ := server.RepoHead(ctx, st.dataDir, name)
		switch {
		case r.State == server.StateConflict:
			conflicts = append(conflicts, name)
		case r.State == server.StateError:
			errs = append(errs, name)
		case r.State == server.StateOK && r.Head != "" && r.Head != head:
			behind = append(behind, name)
		case r.State == server.StateOK:
			ok = append(ok, name)
		}
	}
	switch {
	case len(conflicts) > 0:
		return "CONFLICT in " + strings.Join(conflicts, ", ")
	case len(errs) > 0:
		return "ERROR in " + strings.Join(errs, ", ")
	case len(behind) > 0:
		return "behind in " + strings.Join(behind, ", ")
	case len(ok) > 0:
		return "in sync"
	}
	return "unknown"
}

func listDevices(ctx context.Context, st *serverSettings) error {
	devices, err := server.ListDevices(st.dataDir)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Println("No devices registered. Add one with: dropgitbox device add NAME")
		return nil
	}
	activity, err := server.ReadActivity(st.dataDir)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tLAST SEEN\tLAST PUSH\tSTATUS\tVERSION\tADDED")
	for _, d := range devices {
		act := activity[d.Name]
		seen, push, ver := "never", "-", "-"
		if act != nil {
			seen = ago(act.LastSeen)
			if act.ClientVersion != "" {
				ver = act.ClientVersion
			}
			var last time.Time
			for _, r := range act.Repos {
				if r.LastPush.After(last) {
					last = r.LastPush
				}
			}
			if !last.IsZero() {
				push = ago(last)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.Name, seen, push, deviceStatus(ctx, st, act), ver,
			d.Created.Local().Format(time.DateOnly))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("\nDetails: dropgitbox device show NAME   (activity is saved every few seconds)")
	return nil
}

func showDevice(ctx context.Context, st *serverSettings, name string) error {
	devices, err := server.ListDevices(st.dataDir)
	if err != nil {
		return err
	}
	var dev *server.Device
	for i := range devices {
		if devices[i].Name == name {
			dev = &devices[i]
		}
	}
	if dev == nil {
		return fmt.Errorf("%w: %s", server.ErrNoSuchDevice, name)
	}
	activity, err := server.ReadActivity(st.dataDir)
	if err != nil {
		return err
	}
	act := activity[name]

	fmt.Printf("Device:     %s\n", dev.Name)
	fmt.Printf("Added:      %s\n", dev.Created.Local().Format(time.DateTime))
	if !dev.Rotated.IsZero() {
		fmt.Printf("Token rotated: %s\n", dev.Rotated.Local().Format(time.DateTime))
	}
	if act == nil {
		fmt.Println("Last seen:  never")
		return nil
	}
	fmt.Printf("Last seen:  %s (%s) from %s\n", ago(act.LastSeen), act.LastSeen.Local().Format(time.DateTime), orDash(act.LastIP))
	fmt.Printf("Version:    %s\n", orDash(act.ClientVersion))
	fmt.Printf("Status:     %s\n", deviceStatus(ctx, st, act))
	if len(act.Repos) == 0 {
		return nil
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FOLDER\tSTATE\tREPORTED\tLAST FETCH\tLAST PUSH\tDEVICE HEAD\tSERVER HEAD")
	for _, rn := range sortedKeys(act.Repos) {
		r := act.Repos[rn]
		head, _ := server.RepoHead(ctx, st.dataDir, rn)
		state := orDash(r.State)
		if r.State == server.StateOK && r.Head != "" {
			if r.Head == head {
				state = "ok, in sync"
			} else {
				state = "ok, behind"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rn, state, agoOrDash(r.StateAt), agoOrDash(r.LastFetch),
			agoOrDash(r.LastPush), short(r.Head), short(head))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, rn := range sortedKeys(act.Repos) {
		r := act.Repos[rn]
		if r.State == server.StateConflict {
			fmt.Printf("\n%s: paused on a conflict in: %s\n", rn, strings.Join(r.Conflicts, ", "))
		}
		if r.State == server.StateError && r.Message != "" {
			fmt.Printf("\n%s: last sync failed: %s\n", rn, r.Message)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format(time.DateOnly)
}

func agoOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return ago(t)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func short(sha string) string {
	if sha == "" {
		return "-"
	}
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func configHint(cfg *config.Config) string {
	if cfg.Path() != "" {
		return cfg.Path()
	}
	return "the config file"
}
