package msgauth

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestCheckSPF(t *testing.T) {
	dns := newStub()
	dns.txt["ngm.dev"] = []string{"v=spf1 ip4:10.20.0.0/24 include:mail.partner.com -all"}
	dns.txt["mail.partner.com"] = []string{"v=spf1 a:relay.partner.com ~all"}
	dns.withA("relay.partner.com", "203.0.113.7")
	dns.txt["soft.example"] = []string{"v=spf1 ip4:10.20.0.1 ~all"}
	dns.txt["neutral.example"] = []string{"v=spf1 ?all"}
	dns.txt["broken.example"] = []string{"v=spf1 ip4:not-an-address -all"}
	dns.txt["helo.example"] = []string{"v=spf1 ip4:10.20.0.5 -all"}
	dns.fail["down.example"] = &net.DNSError{Err: "server misbehaving", IsTemporary: true}

	cases := []struct {
		name           string
		ip, helo, from string
		want           Result
		wantDomain     string
	}{
		{
			name: "ip4 mechanism matches", ip: "10.20.0.5", helo: "client.invalid",
			from: "a@ngm.dev", want: ResultPass, wantDomain: "ngm.dev",
		},
		{
			name: "include is followed", ip: "203.0.113.7", helo: "client.invalid",
			from: "a@ngm.dev", want: ResultPass, wantDomain: "ngm.dev",
		},
		{
			name: "-all refuses an unlisted address", ip: "198.51.100.1",
			helo: "client.invalid", from: "a@ngm.dev", want: ResultFail, wantDomain: "ngm.dev",
		},
		{
			name: "~all is a softfail", ip: "198.51.100.1", helo: "client.invalid",
			from: "a@soft.example", want: ResultSoftFail, wantDomain: "soft.example",
		},
		{
			name: "?all is neutral", ip: "198.51.100.1", helo: "client.invalid",
			from: "a@neutral.example", want: ResultNeutral, wantDomain: "neutral.example",
		},
		{
			name: "no record at all is none", ip: "198.51.100.1", helo: "client.invalid",
			from: "a@nothing.example", want: ResultNone, wantDomain: "nothing.example",
		},
		{
			name: "a malformed record is permerror", ip: "198.51.100.1",
			helo: "client.invalid", from: "a@broken.example",
			want: ResultPermError, wantDomain: "broken.example",
		},
		{
			name: "a DNS outage is temperror", ip: "198.51.100.1",
			helo: "client.invalid", from: "a@down.example",
			want: ResultTempError, wantDomain: "down.example",
		},
		{
			// RFC 7208 §2.4: with no sender the identity is postmaster@<helo>,
			// so a bounce still gets an answer. This gateway both sends and
			// receives bounces, so it is not a corner case here.
			name: "a null sender falls back to the HELO name", ip: "10.20.0.5",
			helo: "helo.example", from: "", want: ResultPass, wantDomain: "helo.example",
		},
		{
			// An IPv4 client on a dual-stack listener arrives as ::ffff:10.20.0.5.
			// Without the unmap an ip4: mechanism cannot match its 16-byte form,
			// so every such sender would fail SPF against a record that permits it.
			name: "an IPv4-mapped IPv6 address is unmapped first",
			ip:   "::ffff:10.20.0.5", helo: "client.invalid", from: "a@ngm.dev",
			want: ResultPass, wantDomain: "ngm.dev",
		},
		{
			name: "no identity to check is none", ip: "10.20.0.5", helo: "", from: "",
			want: ResultNone, wantDomain: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, err := netip.ParseAddr(c.ip)
			if err != nil {
				t.Fatalf("bad IP %q: %v", c.ip, err)
			}
			got := CheckSPF(context.Background(), dns, ip, c.helo, c.from)
			if got.Value != c.want {
				t.Errorf("result = %s, want %s (reason: %s)", got.Value, c.want, got.Reason)
			}
			if got.Domain != c.wantDomain {
				t.Errorf("domain = %q, want %q", got.Domain, c.wantDomain)
			}
			if !got.Checked() {
				t.Error("Checked() is false for an evaluation that ran")
			}
			// The library reports the mechanism that decided the outcome as an
			// error, even on success. Those names are jargon in a header
			// ("spf=pass reason=\"matched ip\"") and misleading on a refusal,
			// where "matched all" means the -all mechanism was REACHED. A real
			// diagnosis — a DNS outage, a malformed record — is kept.
			switch got.Value {
			case ResultTempError, ResultPermError:
				if got.Reason == "" {
					t.Error("an error result should explain itself")
				}
			default:
				if strings.HasPrefix(got.Reason, "matched ") {
					t.Errorf("mechanism jargon leaked into the reason: %q", got.Reason)
				}
			}
		})
	}
}

// TestSPFResult_ZeroValueIsNotChecked pins the distinction the header renderer
// depends on: an empty Value means no check happened, where "none" means a check
// ran and found no policy. Reporting the first as the second would put a claim
// in Authentication-Results that this gateway never made.
func TestSPFResult_ZeroValueIsNotChecked(t *testing.T) {
	if (SPFResult{}).Checked() {
		t.Error("the zero SPFResult reports as checked")
	}
	if !(SPFResult{Value: ResultNone}).Checked() {
		t.Error("a none result reports as not checked")
	}
}

func TestSPFIdentity(t *testing.T) {
	cases := []struct{ helo, from, want string }{
		{"client.invalid", "a@ngm.dev", "ngm.dev"},
		{"client.invalid", "a@NGM.DEV", "ngm.dev"},
		{"client.invalid", "a@ngm.dev.", "ngm.dev"},
		{"client.invalid", "", "client.invalid"},
		{"client.invalid.", "", "client.invalid"},
		{"", "", ""},
		// No "@" at all: Domain() returns "", so the HELO name is used. Note
		// this diverges from Haraka's Route.js:16, which returned the whole
		// string as if it were a domain.
		{"client.invalid", "postmaster", "client.invalid"},
	}
	for _, c := range cases {
		if got := spfIdentity(c.helo, c.from); got != c.want {
			t.Errorf("spfIdentity(%q, %q) = %q, want %q", c.helo, c.from, got, c.want)
		}
	}
}
