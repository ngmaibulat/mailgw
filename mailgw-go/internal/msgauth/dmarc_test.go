package msgauth

import (
	"context"
	"net"
	"testing"
)

func dmarcStub() *stubDNS {
	dns := newStub()
	dns.txt["_dmarc.ngm.dev"] = []string{"v=DMARC1; p=reject; rua=mailto:d@ngm.dev"}
	dns.txt["_dmarc.relaxed.example"] = []string{"v=DMARC1; p=quarantine"}
	dns.txt["_dmarc.strict.example"] = []string{"v=DMARC1; p=reject; aspf=s; adkim=s"}
	dns.txt["_dmarc.parent.example"] = []string{"v=DMARC1; p=none; sp=reject"}
	dns.txt["_dmarc.broken.example"] = []string{"v=DMARC1; p=nonsense"}
	dns.fail["_dmarc.down.example"] = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	return dns
}

func TestEvaluateDMARC(t *testing.T) {
	dns := dmarcStub()

	cases := []struct {
		name       string
		fromDomain string
		spf        SPFResult
		dkim       DKIMResult
		want       Result
		wantPolicy string
		wantSPF    bool
		wantDKIM   bool
	}{
		{
			name: "aligned SPF passes", fromDomain: "ngm.dev",
			spf:  SPFResult{Value: ResultPass, Domain: "ngm.dev"},
			dkim: DKIMResult{Value: ResultNone},
			want: ResultPass, wantPolicy: "reject", wantSPF: true,
		},
		{
			name: "aligned DKIM passes", fromDomain: "ngm.dev",
			spf:  SPFResult{Value: ResultFail, Domain: "bounces.other.example"},
			dkim: DKIMResult{Value: ResultPass, Domains: []string{"ngm.dev"}},
			want: ResultPass, wantPolicy: "reject", wantDKIM: true,
		},
		{
			// The case relaxed alignment exists for: a bounce address on a
			// subdomain, a From on the organizational domain.
			name: "relaxed alignment accepts a subdomain", fromDomain: "relaxed.example",
			spf:  SPFResult{Value: ResultPass, Domain: "bounces.relaxed.example"},
			dkim: DKIMResult{Value: ResultNone},
			want: ResultPass, wantPolicy: "quarantine", wantSPF: true,
		},
		{
			name: "strict alignment refuses a subdomain", fromDomain: "strict.example",
			spf:  SPFResult{Value: ResultPass, Domain: "bounces.strict.example"},
			dkim: DKIMResult{Value: ResultNone},
			want: ResultFail, wantPolicy: "reject",
		},
		{
			name: "an authenticated but unaligned identifier fails", fromDomain: "ngm.dev",
			spf:  SPFResult{Value: ResultPass, Domain: "evil.example"},
			dkim: DKIMResult{Value: ResultPass, Domains: []string{"evil.example"}},
			want: ResultFail, wantPolicy: "reject",
		},
		{
			name: "nothing authenticated fails", fromDomain: "ngm.dev",
			spf:  SPFResult{Value: ResultFail, Domain: "ngm.dev"},
			dkim: DKIMResult{Value: ResultFail},
			want: ResultFail, wantPolicy: "reject",
		},
		{
			name: "no published record is none", fromDomain: "nothing.example",
			spf:  SPFResult{Value: ResultPass, Domain: "nothing.example"},
			dkim: DKIMResult{Value: ResultNone},
			want: ResultNone,
		},
		{
			name: "a malformed record is permerror", fromDomain: "broken.example",
			spf:  SPFResult{Value: ResultPass, Domain: "broken.example"},
			dkim: DKIMResult{Value: ResultNone},
			want: ResultPermError,
		},
		{
			name: "a DNS outage is temperror", fromDomain: "down.example",
			spf:  SPFResult{Value: ResultPass, Domain: "down.example"},
			dkim: DKIMResult{Value: ResultNone},
			want: ResultTempError,
		},
		{
			// sp= is what the record's owner said should happen to subdomains,
			// and the message is from one — so it, not p=, is the policy that
			// applies here.
			name:       "an inherited record reports sp= as the policy",
			fromDomain: "sub.parent.example",
			spf:        SPFResult{Value: ResultFail, Domain: "elsewhere.example"},
			dkim:       DKIMResult{Value: ResultNone},
			want:       ResultFail, wantPolicy: "reject",
		},
		{
			name: "no From domain is none", fromDomain: "",
			spf:  SPFResult{Value: ResultPass, Domain: "ngm.dev"},
			dkim: DKIMResult{Value: ResultNone},
			want: ResultNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EvaluateDMARC(context.Background(), dns, c.fromDomain, c.spf, c.dkim)
			if got.Value != c.want {
				t.Errorf("result = %s, want %s (reason: %s)", got.Value, c.want, got.Reason)
			}
			if got.Policy != c.wantPolicy {
				t.Errorf("policy = %q, want %q", got.Policy, c.wantPolicy)
			}
			if got.SPFAligned != c.wantSPF {
				t.Errorf("SPFAligned = %v, want %v", got.SPFAligned, c.wantSPF)
			}
			if got.DKIMAligned != c.wantDKIM {
				t.Errorf("DKIMAligned = %v, want %v", got.DKIMAligned, c.wantDKIM)
			}
		})
	}
}

// TestEvaluateDMARC_OnlyPassingDKIMDomainsAlign is the assertion behind
// VerifyDKIM only putting passing signatures in Domains: a d= from a broken
// signature is a claim anybody can make, and crediting it would let a forger
// pass DMARC by attaching an invalid signature naming the victim's domain.
func TestEvaluateDMARC_OnlyPassingDKIMDomainsAlign(t *testing.T) {
	forged := DKIMResult{
		Value: ResultFail,
		// Domains is empty despite the signature having claimed ngm.dev.
		Signatures: []Signature{{Domain: "ngm.dev", Value: ResultFail}},
	}
	got := EvaluateDMARC(context.Background(), dmarcStub(), "ngm.dev",
		SPFResult{Value: ResultFail, Domain: "evil.example"}, forged)
	if got.Value != ResultFail || got.DKIMAligned {
		t.Fatalf("a failed signature claiming ngm.dev produced %s (aligned=%v)",
			got.Value, got.DKIMAligned)
	}
}

// TestLookupDMARC_DoesNotWalkIntoAPublicSuffix pins the approximation documented
// on lookupDMARC. Without a public suffix list the parent walk stops one label
// up and never at a bare TLD, so a DMARC record published at "example" — or at a
// public suffix — cannot be inherited by everything beneath it.
func TestLookupDMARC_DoesNotWalkIntoAPublicSuffix(t *testing.T) {
	dns := newStub()
	dns.txt["_dmarc.example"] = []string{"v=DMARC1; p=reject"}

	got := EvaluateDMARC(context.Background(), dns, "victim.example",
		SPFResult{Value: ResultFail, Domain: "evil.test"}, DKIMResult{Value: ResultNone})
	if got.Value != ResultNone {
		t.Fatalf("inherited a policy from a single-label parent: %s / %q", got.Value, got.Policy)
	}
}

func TestAligned(t *testing.T) {
	cases := []struct {
		auth, from string
		strict     bool
		want       bool
	}{
		{"ngm.dev", "ngm.dev", false, true},
		{"ngm.dev", "ngm.dev", true, true},
		{"NGM.dev", "ngm.DEV", false, true},
		{"mail.ngm.dev", "ngm.dev", false, true},
		{"ngm.dev", "mail.ngm.dev", false, true},
		{"mail.ngm.dev", "ngm.dev", true, false},
		// A suffix that is not on a label boundary must never align, or
		// "notngm.dev" would authenticate for "ngm.dev".
		{"notngm.dev", "ngm.dev", false, false},
		{"", "ngm.dev", false, false},
		{"ngm.dev", "", false, false},
	}
	for _, c := range cases {
		mode := relaxed
		if c.strict {
			mode = strict
		}
		if got := aligned(c.auth, c.from, mode); got != c.want {
			t.Errorf("aligned(%q, %q, strict=%v) = %v, want %v",
				c.auth, c.from, c.strict, got, c.want)
		}
	}
}
