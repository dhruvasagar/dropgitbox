package client

import "testing"

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
