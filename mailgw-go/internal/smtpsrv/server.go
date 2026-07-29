package smtpsrv

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
)

// NewServer builds the go-smtp server from configuration.
//
// The capability set is not incidental: tests/smtp/tests/smtp.test.ts:56-63
// requires EHLO to advertise SIZE, PIPELINING, 8BITMIME and SMTPUTF8. go-smtp
// advertises PIPELINING and 8BITMIME unconditionally, SIZE only when
// MaxMessageBytes > 0, and SMTPUTF8 only when EnableSMTPUTF8 is set — which is
// why config validation rejects a zero max.bytes.
func NewServer(b *Backend, cfg *config.Server, log *slog.Logger) (*smtp.Server, error) {
	s := smtp.NewServer(b)

	s.Domain = cfg.Hostname
	s.MaxMessageBytes = cfg.Max.Bytes
	s.MaxRecipients = cfg.Max.Recipients
	s.MaxLineLength = cfg.Max.LineLength
	s.EnableSMTPUTF8 = cfg.SMTPUTF8
	s.ReadTimeout = cfg.Inactivity.D()
	s.WriteTimeout = cfg.Inactivity.D()

	if cfg.TLS.ConfiguredPublic() {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return nil, fmt.Errorf("load TLS keypair: %w", err)
		}
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	if log != nil {
		s.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelError)
	}
	return s, nil
}

// Guard wraps a listener with the connect-stage allowlist check.
func Guard(l net.Listener, allowed AllowlistFunc, log *slog.Logger) net.Listener {
	return NewAllowlistListener(l, allowed, log)
}
