package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
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
	srv, err := server.New(server.Options{DataDir: st.dataDir, Token: st.token, NoAuth: st.noAuth, Logger: log})
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

	hs := &http.Server{
		Addr:              st.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
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
	return hs.Shutdown(shutdownCtx)
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
  dropgitbox device list                  list registered devices
  dropgitbox device revoke NAME           revoke a device's access immediately

Run these on the server node, as the server's user or with sudo.`
	if len(args) == 0 {
		return errors.New(help)
	}
	sub, args := args[0], args[1:]

	fs, cfgPath := newFlags("device " + sub)
	data := fs.String("data", "", "server data directory (config: server.data_dir)")
	pubURL := fs.String("url", "", "URL devices use to reach the server (config: server.public_url)")
	pos := parse(fs, args)

	st, err := loadServerSettings(*cfgPath, *data)
	if err != nil {
		return err
	}
	if isSet(fs, "url") {
		st.url = *pubURL
	}

	switch sub {
	case "add":
		if len(pos) != 1 {
			return errors.New("usage: dropgitbox device add NAME [-url URL]")
		}
		if st.url == "" {
			return errors.New("device add: the server's public URL is unknown; pass -url https://box.example.com or set server.public_url in " + configHint(st.cfg))
		}
		name := pos[0]
		token, err := server.AddDevice(st.dataDir, name)
		if err != nil {
			return err
		}
		inv := client.Invite{Server: strings.TrimRight(st.url, "/"), Token: token, Device: name}
		fmt.Printf(`Registered device %q.

On that device, install dropgitbox and run:

  dropgitbox join %s

The invite contains the device's access token: send it over a private
channel. To cut the device off later: dropgitbox device revoke %s
`, name, inv.Encode(), name)
		return nil

	case "list":
		devices, err := server.ListDevices(st.dataDir)
		if err != nil {
			return err
		}
		if len(devices) == 0 {
			fmt.Println("No devices registered. Add one with: dropgitbox device add NAME")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "DEVICE\tADDED")
		for _, d := range devices {
			fmt.Fprintf(tw, "%s\t%s\n", d.Name, d.Created.Local().Format(time.DateTime))
		}
		return tw.Flush()

	case "revoke", "remove", "rm":
		if len(pos) != 1 {
			return errors.New("usage: dropgitbox device revoke NAME")
		}
		if err := server.RevokeDevice(st.dataDir, pos[0]); err != nil {
			return err
		}
		fmt.Printf("Revoked %q. Its token no longer works; the data already on that device stays there.\n", pos[0])
		return nil
	}
	return fmt.Errorf("unknown device command %q\n\n%s", sub, help)
}

func configHint(cfg *config.Config) string {
	if cfg.Path() != "" {
		return cfg.Path()
	}
	return "the config file"
}
