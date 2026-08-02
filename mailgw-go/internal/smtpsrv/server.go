package smtpsrv

import (
	"crypto/tls"
	"log/slog"
	"net"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/tlsx"
)

// NewServer builds the go-smtp server from configuration.
//
// The capability set is not incidental: tests/smtp/tests/smtp.test.ts:56-63
// requires EHLO to advertise SIZE, PIPELINING, 8BITMIME and SMTPUTF8. go-smtp
// advertises PIPELINING and 8BITMIME unconditionally, SIZE only when
// MaxMessageBytes > 0, and SMTPUTF8 only when EnableSMTPUTF8 is set — which is
// why config validation rejects a zero max.bytes.
//
// tlsCfg is passed in rather than built here because resolving a certificate is
// not a property of the SMTP server: a managed node with no configured keypair
// falls back to one it generated into its data directory, and that directory is
// a property of the process. Nil means no TLS — neither STARTTLS advertised nor
// an implicit-TLS listener possible.
func NewServer(b *Backend, cfg *config.Server, tlsCfg *tls.Config, log *slog.Logger) (*smtp.Server, error) {
	s := smtp.NewServer(b)

	s.Domain = cfg.Hostname
	s.MaxMessageBytes = cfg.Max.Bytes
	s.MaxRecipients = cfg.Max.Recipients
	s.MaxLineLength = cfg.Max.LineLength
	s.EnableSMTPUTF8 = cfg.SMTPUTF8

	// RFC 3461 DSN, gated on the same switch that decides whether this gateway
	// generates notifications at all. With dsn.enabled off, NOTIFY=FAILURE
	// would be answered 250 and then never honoured — a promise made on the
	// wire and silently dropped, which is the same failure the `unpopulated`
	// registry exists to prevent for rules.
	//
	// Advertising it commits to more than parsing: ORCPT rides through to
	// Original-Recipient, NOTIFY decides who is told, RET chooses how much of
	// the message comes back, and NOTIFY=SUCCESS earns a "relayed" report,
	// because this gateway does not pass DSN parameters to the next hop and
	// RFC 3461 §5.2.7 makes it the boundary that answers for them.
	s.EnableDSN = cfg.DSN.Enabled
	s.ReadTimeout = cfg.Inactivity.D()
	s.WriteTimeout = cfg.Inactivity.D()

	// go-smtp advertises STARTTLS purely off TLSConfig != nil (conn.go:259-261),
	// so whether the upgrade is offered is decided entirely by whether the caller
	// hands one over — which is what makes `starttls: false` expressible.
	s.TLSConfig = tlsCfg

	// AUTH is offered only over an encrypted session unless this is set —
	// go-smtp's authAllowed() (conn.go:204-206) gates both the capability and
	// the command, answering 523 5.7.10 otherwise. Off by default because M8
	// made STARTTLS an opt-out, so encryption is available on any gateway with
	// a keypair, and advertising PLAIN in the clear would undo that. The
	// legitimate case is TLS terminated in front of the gateway.
	s.AllowInsecureAuth = cfg.TLS.AllowInsecureAuth

	if log != nil {
		s.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelError)
	}
	return s, nil
}

// NewTLSConfig builds the server-side TLS configuration from a keypair on disk.
//
// The keypair is served through a reloader rather than loaded once: a
// certificate renewed in place would otherwise keep serving the expired one
// until somebody restarted the process, and restartRequired can only see the
// cert PATH change, never its contents.
func NewTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	r, err := tlsx.NewReloader(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		GetCertificate: r.GetCertificate,
		// TLS 1.0 and 1.1 are the versions a sending MTA reaches for only when
		// it has nothing better, and every mainstream MTA has had 1.2 for a
		// decade. Matches internal/central's client side.
		MinVersion: tls.VersionTLS12,
	}, nil
}

// Guard wraps a listener with the connect-stage allowlist check.
func Guard(l net.Listener, allowed AllowlistFunc, log *slog.Logger, m *obs.Metrics) net.Listener {
	return NewAllowlistListener(l, allowed, log, m)
}
