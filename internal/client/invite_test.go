package client

import (
	"strings"
	"testing"
)

func TestInviteRoundTrip(t *testing.T) {
	in := Invite{Server: "https://box.example.com:8443", Token: "dgb_abc", Device: "phone"}
	out, err := DecodeInvite("  " + in.Encode() + "\n")
	if err != nil || out != in {
		t.Fatalf("got %+v, %v", out, err)
	}
	for _, bad := range []string{
		"",
		"dgb_abc",
		"dgb1.!!!",
		Invite{Server: "ftp://x", Token: "t"}.Encode(),
		Invite{Server: "https://x"}.Encode(),
	} {
		if _, err := DecodeInvite(bad); err == nil {
			t.Errorf("accepted bad invite %q", bad)
		}
	}
}

func TestValidServerURL(t *testing.T) {
	for _, ok := range []string{"https://box.example.com", "http://127.0.0.1:8080", "https://example.com/dropgitbox"} {
		if err := ValidServerURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "127.0.0.1:8080", "box.example.com", "ftp://x", "https://"} {
		if err := ValidServerURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	err := ValidServerURL("127.0.0.1:8080")
	if err == nil || !strings.Contains(err.Error(), "http://127.0.0.1:8080") {
		t.Errorf("no fix suggested: %v", err)
	}
}
