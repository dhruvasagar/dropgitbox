package client

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// invitePrefix versions the invite format.
const invitePrefix = "dgb1."

// Invite carries everything a new device needs to join a server. It
// contains the device's token, so it must be treated like a password.
type Invite struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	Device string `json:"device"`
}

// Encode renders the invite as a single copy-pasteable string.
func (i Invite) Encode() string {
	b, _ := json.Marshal(i)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// InsecureURL reports whether serverURL sends tokens unencrypted to
// another machine (plain http to a non-loopback host).
func InsecureURL(serverURL string) bool {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

// DecodeInvite parses an invite produced by Encode.
func DecodeInvite(s string) (Invite, error) {
	var inv Invite
	payload, ok := strings.CutPrefix(strings.TrimSpace(s), invitePrefix)
	if !ok {
		return inv, errors.New("not a dropgitbox invite (it should start with \"" + invitePrefix + "\")")
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return inv, fmt.Errorf("invite is corrupted (was it copied completely?): %w", err)
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		return inv, fmt.Errorf("invite is corrupted: %w", err)
	}
	if inv.Token == "" {
		return inv, errors.New("invite has no token")
	}
	if u, err := url.Parse(inv.Server); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return inv, fmt.Errorf("invite has an invalid server URL %q", inv.Server)
	}
	return inv, nil
}
