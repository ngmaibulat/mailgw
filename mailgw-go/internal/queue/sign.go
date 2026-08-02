package queue

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
)

// Signer adds a DKIM-Signature to outbound mail.
//
// It lives here, in the runner's package, rather than in internal/deliver — a
// departure from what plans/M14 sketched, and a necessary one. A DKIM signature
// has to be written AHEAD of bytes the signer has already consumed, and
// deliver.Message.Body is a one-shot io.Reader with no way to rewind. The spool
// can simply be opened twice, so the seam has to be the caller that owns it.
//
// Two passes over a file also beat what go-msgauth's own dkim.Sign does, which
// is buffer the entire message in memory so it can emit the signature first.
// That would mean holding a 25 MiB message in RAM once per delivery attempt.
type Signer struct {
	keys *msgauth.Keys
	opts msgauth.SignOptions
}

// NewSigner builds a signer over a key set. Nil keys, or an empty set, returns
// nil — which every caller treats as "signing is off".
func NewSigner(keys *msgauth.Keys, headerCan, bodyCan string, headers []string, expiration time.Duration) *Signer {
	if keys == nil || keys.Len() == 0 {
		return nil
	}
	return &Signer{keys: keys, opts: msgauth.SignOptions{
		HeaderCan:  headerCan,
		BodyCan:    bodyCan,
		HeaderKeys: headers,
		Expiration: expiration,
	}}
}

// ErrNoKey reports that nothing is configured to sign this message's From
// domain. It is not a failure: a gateway relaying other people's mail signs the
// domains it is responsible for and leaves the rest alone.
var ErrNoKey = errors.New("no DKIM key for this message's From domain")

// Sign computes the DKIM-Signature header field for a message, given a function
// that opens the spooled body and the header block that will be prepended ahead
// of it at delivery.
//
// prepend is not optional decoration. Every header this gateway adds — the
// add_header actions, the Authentication-Results from an inbound check — is
// prepended at delivery, so a signature computed over the spooled body alone
// would cover a message that never goes on the wire. The plan calls this the
// ordering trap and it is the single most important line in this file.
//
// Returns the complete header field, including its trailing CRLF.
func (s *Signer) Sign(open func() (io.ReadCloser, error), prepend string) (string, error) {
	if s == nil {
		return "", ErrNoKey
	}

	// Pass one, only as far as the From header: the signing key is chosen by the
	// message's RFC5322.From domain so that d= aligns and DMARC credits the
	// signature downstream. Keying off the envelope sender would be valid, cost
	// the same bytes and buy nothing whenever the two differ — which is every
	// forwarded message and every bounce this gateway generates.
	from, err := s.fromDomain(open, prepend)
	if err != nil {
		return "", err
	}
	selector, key, ok := s.keys.For(from)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoKey, from)
	}

	// Pass two, over the whole message as it will be sent.
	body, err := open()
	if err != nil {
		return "", err
	}
	defer body.Close()

	opts := s.opts
	opts.Domain, opts.Selector, opts.Signer = from, selector, key
	return msgauth.SignDKIM(io.MultiReader(strings.NewReader(prepend), body), opts)
}

func (s *Signer) fromDomain(open func() (io.ReadCloser, error), prepend string) (string, error) {
	body, err := open()
	if err != nil {
		return "", err
	}
	defer body.Close()

	// The prepend block goes in front here too. It contains no From today, but
	// reading the two streams in a different order than the signature covers
	// them is exactly the kind of divergence this file exists to avoid.
	from, err := msgauth.SigningFromDomain(io.MultiReader(strings.NewReader(prepend), body))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoKey, err)
	}
	return from, nil
}
