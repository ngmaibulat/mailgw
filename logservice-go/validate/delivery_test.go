package validate

import (
	"encoding/json"
	"strings"
	"testing"
)

// valid is the body mailgw-go actually sends, byte-compatible with the golden
// fixture in mailgw-go/internal/events/payload_test.go.
func valid() map[string]any {
	return map[string]any{
		"uuid":          "abc-123",
		"dt":            float64(1753776000000),
		"sender":        "me@ngm.dev",
		"rcpt_domain":   "ngm.dev",
		"rcpt_list":     "test@ngm.dev",
		"rcpt_accepted": "test@ngm.dev",
		"tls_forced":    false,
		"tls":           true,
		"auth":          true,
		"host":          "sandbox.smtp.mailtrap.io",
		"ip":            "203.0.113.10",
		"port":          "2525",
		"response":      "250 2.0.0 Ok: queued as ABC123",
		"delay":         0.123,
	}
}

func parse(t *testing.T, body map[string]any) (*Delivery, error) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return ParseDelivery(raw)
}

func mustAccept(t *testing.T, body map[string]any) {
	t.Helper()
	if _, err := parse(t, body); err != nil {
		t.Fatalf("body rejected but should be accepted: %v", err)
	}
}

func mustReject(t *testing.T, body map[string]any, field string) {
	t.Helper()
	_, err := parse(t, body)
	if err == nil {
		t.Fatalf("body accepted but should be rejected (expected a failure on %q)", field)
	}
	if field != "" && !strings.Contains(err.Error(), field) {
		t.Errorf("rejected on %v, want a failure naming %q", err, field)
	}
}

func TestParseDelivery_AcceptsTheGatewaysPayload(t *testing.T) {
	mustAccept(t, valid())
}

func TestParseDelivery_RequiresEveryCoreField(t *testing.T) {
	for _, field := range []string{
		"uuid", "dt", "sender", "rcpt_domain", "rcpt_list", "rcpt_accepted",
		"tls_forced", "tls", "auth", "host", "ip", "port", "response", "delay",
	} {
		t.Run(field, func(t *testing.T) {
			b := valid()
			delete(b, field)
			mustReject(t, b, field)
		})
	}
}

// The null sender, MAIL FROM:<>. Every bounce and DSN uses it; requiring a valid
// address here would reject the gateway's own notification traffic.
func TestParseDelivery_AcceptsTheNullSender(t *testing.T) {
	b := valid()
	b["sender"] = ""
	mustAccept(t, b)
}

func TestParseDelivery_RejectsAMalformedSender(t *testing.T) {
	b := valid()
	b["sender"] = "not-an-email"
	mustReject(t, b, "sender")
}

// The defect this schema exists to catch: the legacy Haraka plugin sends a
// comma-joined recipient list, so every multi-recipient delivery 400s. mailgw-go
// emits one event per recipient instead.
func TestParseDelivery_RejectsACommaJoinedRecipientList(t *testing.T) {
	b := valid()
	b["rcpt_list"] = "a@x.com,b@y.com"
	mustReject(t, b, "rcpt_list")
}

// Single-label names and IP literals are accepted DELIBERATELY: an FQDN-only
// rule discarded the event for perfectly successful deliveries to `localhost`
// or a container name.
func TestParseDelivery_AcceptsSingleLabelAndLiteralHosts(t *testing.T) {
	for _, host := range []string{
		"localhost", "dev-mailhog", "mx1", "smtp.example.com",
		"192.0.2.1", "[2001:db8::1]", "2001:db8::1",
	} {
		t.Run(host, func(t *testing.T) {
			b := valid()
			b["host"] = host
			mustAccept(t, b)
		})
	}
}

func TestParseDelivery_RejectsMalformedHosts(t *testing.T) {
	for _, host := range []string{"", "has space", "under_score", "-leading-dash"} {
		t.Run(host, func(t *testing.T) {
			b := valid()
			b["host"] = host
			mustReject(t, b, "host")
		})
	}
}

func TestParseDelivery_AcceptsTheIPFormsAGatewayProduces(t *testing.T) {
	for _, ip := range []string{
		"203.0.113.10", "127.0.0.1", "::1", "2001:db8::1", "::ffff:127.0.0.1",
		"", // a delivery can fail before a peer address is known
	} {
		t.Run("ip="+ip, func(t *testing.T) {
			b := valid()
			b["ip"] = ip
			mustAccept(t, b)
		})
	}
}

func TestParseDelivery_RejectsAMalformedIP(t *testing.T) {
	for _, ip := range []string{"999.1.1.1", "not-an-ip", "300.300.300.300"} {
		t.Run(ip, func(t *testing.T) {
			b := valid()
			b["ip"] = ip
			mustReject(t, b, "ip")
		})
	}
}

// The port is a STRING of digits on the wire, into an INT column. mailgw-go
// sends it that way because of this rule; a JSON number is a 400.
func TestParseDelivery_PortMustBeADigitString(t *testing.T) {
	b := valid()
	b["port"] = float64(25)
	mustReject(t, b, "")

	b = valid()
	b["port"] = "smtp"
	mustReject(t, b, "port")
}

func TestParseDelivery_BooleansAreStrict(t *testing.T) {
	b := valid()
	b["tls"] = "yes"
	mustReject(t, b, "")
}

func TestParseDelivery_DTMustBeANumber(t *testing.T) {
	b := valid()
	b["dt"] = "1753776000000"
	mustReject(t, b, "")
}

// Zero and false are real values, not absences. A pointer-free struct would
// have accepted a body that omitted them.
func TestParseDelivery_ZeroAndFalseAreAcceptedNotTreatedAsAbsent(t *testing.T) {
	b := valid()
	b["delay"] = float64(0)
	b["tls"] = false
	b["auth"] = false
	b["tls_forced"] = false
	b["dt"] = float64(0)
	mustAccept(t, b)
}

// Optional since migration 023, and they must stay optional: requiring them
// would 400 every event from a gateway older than that migration, invisibly.
func TestParseDelivery_GatewayAndRouteRuleAreOptional(t *testing.T) {
	mustAccept(t, valid()) // neither present

	b := valid()
	b["gateway"] = "11111111-2222-3333-4444-555555555555"
	b["route_rule"] = "partner subdomains over the TLS relay"
	mustAccept(t, b)
}

func TestParseDelivery_GatewayIsBoundedAt64Characters(t *testing.T) {
	b := valid()
	b["gateway"] = strings.Repeat("x", 65)
	mustReject(t, b, "gateway")

	b["gateway"] = strings.Repeat("x", 64)
	mustAccept(t, b)
}

func TestParseDelivery_RouteRuleIsBoundedAt255Characters(t *testing.T) {
	b := valid()
	b["route_rule"] = strings.Repeat("x", 256)
	mustReject(t, b, "route_rule")

	b["route_rule"] = strings.Repeat("x", 255)
	mustAccept(t, b)
}

// Unknown keys are ignored rather than rejected — gateways have sent fields this
// schema does not know before and will again.
func TestParseDelivery_IgnoresUnknownKeys(t *testing.T) {
	b := valid()
	b["state"] = "queued"
	b["pipelining"] = true
	mustAccept(t, b)
}

func TestParseDelivery_RejectsMalformedJSON(t *testing.T) {
	if _, err := ParseDelivery([]byte("{not json")); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}
