package api

import (
	"encoding/json"

	"github.com/gofiber/fiber/v3"

	"github.com/ngmaibulat/mailgw/logservice-go/query"
	"github.com/ngmaibulat/mailgw/logservice-go/store"
	"github.com/ngmaibulat/mailgw/logservice-go/validate"
)

// handleRoot is the health check the compose stacks and the e2e suite use.
// Open, and answering a fixed body.
func (s *Server) handleRoot(c fiber.Ctx) error {
	return writeJSON(c, fiber.StatusOK, bodyOK)
}

// handleNotFound reproduces the Bun catch-all: plain text, not JSON.
//
// It is both the catch-all route and what errorHandler falls back to for
// Fiber's 404/405/501, so a wrong method and an unknown path are byte-identical
// — which is what the Bun service did and what logservice-go does.
func (s *Server) handleNotFound(c fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "text/plain; charset=utf-8")
	return c.Status(fiber.StatusNotFound).SendString(notFoundBody)
}

// handlePostConnection records a connect-stage event.
//
// It validates NOTHING, deliberately. Absent fields default — counters to 0,
// booleans to false, everything else to NULL — exactly as the Bun handler's `??`
// chain did. The e2e suite posts six of the fifteen fields and expects a 200.
//
// A malformed JSON body is the one refusal, and it is a 400: an unparseable body
// can never become parseable, which is precisely the condition mailgw-go's 4xx
// rule is for.
func (s *Server) handlePostConnection(c fiber.Ctx) error {
	body, err := readBody(c)
	if err != nil {
		return writeJSON(c, fiber.StatusBadRequest, bodyFail)
	}

	var conn store.Connection
	if err := json.Unmarshal(body, &conn); err != nil {
		s.log().Warn("malformed connection event", "err", err)
		return writeJSON(c, fiber.StatusBadRequest, bodyFail)
	}

	ctx, cancel := dbCtx(c)
	defer cancel()
	if err := s.store().InsertConnection(ctx, conn); err != nil {
		return s.fail500(c, err)
	}
	return writeJSON(c, fiber.StatusOK, bodyOK)
}

// handlePostQueue records a queue event as a Transaction row.
//
// It writes ONLY the Transaction row. The connect-stage Connection row is
// already written by /api/connection; the Bun handler's commented-out
// insertConnection call is the scar from when this double-inserted.
func (s *Server) handlePostQueue(c fiber.Ctx) error {
	body, err := readBody(c)
	if err != nil {
		return writeJSON(c, fiber.StatusBadRequest, bodyFail)
	}

	var q store.Queue
	if err := json.Unmarshal(body, &q); err != nil {
		s.log().Warn("malformed queue event", "err", err)
		return writeJSON(c, fiber.StatusBadRequest, bodyFail)
	}

	ctx, cancel := dbCtx(c)
	defer cancel()
	if err := s.store().InsertTransaction(ctx, q); err != nil {
		return s.fail500(c, err)
	}
	return writeJSON(c, fiber.StatusOK, bodyOK)
}

// handlePostDelivery records one recipient's delivery outcome. The only
// validated endpoint.
//
// The 400 body is a bare {"status":"Fail"} with no detail, matching the Bun
// service: the reason goes to the log, where an operator can see it, and not to
// a caller who could use it to map the schema. The gateway would not read it
// either — it checks the status and nothing else.
//
// Note what is NOT used here: Fiber v3's c.Bind(). Its errors would reach
// errorHandler and become Fiber's own 400 shape, and on /filter/md5 the same
// reflex would turn a bad body into a deferred message.
func (s *Server) handlePostDelivery(c fiber.Ctx) error {
	body, err := readBody(c)
	if err != nil {
		return writeJSON(c, fiber.StatusBadRequest, bodyFail)
	}

	d, err := validate.ParseDelivery(body)
	if err != nil {
		// Warn, not Debug: a gateway whose events are all being rejected is
		// losing its audit trail permanently, and this line is the only place
		// that says so.
		s.log().Warn("delivery event rejected", "err", err)
		return writeJSON(c, fiber.StatusBadRequest, bodyFail)
	}

	ctx, cancel := dbCtx(c)
	defer cancel()
	if err := s.store().InsertDelivery(ctx, d); err != nil {
		return s.fail500(c, err)
	}
	return writeJSON(c, fiber.StatusOK, bodyOK)
}

// handleFilterMD5 answers the attachment blocklist check.
//
// Every failure mode here defers real mail: mailgw-go turns a non-2xx, an
// unparseable body, a missing `action` or an unrecognised `action` into an
// error, and under `attach.fail: closed` that is an SMTP 451. So this handler
// answers `allow` for anything it can decide, and 500 only when the database
// genuinely could not be consulted.
//
// A body that is not an array is treated as an empty list, exactly as the Bun
// handler's `Array.isArray(body) ? body : []` did — NOT a 400. An oversized body
// is the one place readBody's error becomes a 500 rather than a 400, for the
// same reason: this endpoint has no 4xx that is safe.
func (s *Server) handleFilterMD5(c fiber.Ctx) error {
	body, err := readBody(c)
	if err != nil {
		return s.fail500(c, err)
	}

	var list []store.Attachment
	if err := json.Unmarshal(body, &list); err != nil {
		// Not an array, or not JSON at all. Allow, and say so once — a scanner
		// that starts refusing bodies it used to accept would defer mail
		// silently.
		s.log().Warn("filter/md5 body is not an attachment array, allowing", "err", err)
		return writeJSON(c, fiber.StatusOK, map[string]string{"action": "allow"})
	}

	ctx, cancel := dbCtx(c)
	defer cancel()

	digests := make([]string, 0, len(list))
	for _, a := range list {
		if a.MD5 != nil && *a.MD5 != "" {
			digests = append(digests, *a.MD5)
		}
	}

	blocked, err := s.store().BlockedMD5s(ctx, digests)
	if err != nil {
		return s.fail500(c, err)
	}

	// The message blocks if ANY attachment does.
	overall := "allow"
	for _, a := range list {
		action := "allow"
		if a.MD5 != nil {
			if _, ok := blocked[*a.MD5]; ok {
				action = "block"
				overall = "block"
			}
		}
		// Every attachment is recorded, allowed ones included: HashLookups is
		// the record of what was scanned, not only of what was refused.
		//
		// A failure to record must not change the verdict. The blocklist answer
		// is already known and correct; losing an audit row is worse than
		// nothing but far better than deferring a legitimate message.
		if err := s.store().RecordLookup(ctx, a, action); err != nil {
			s.log().Error("could not record an attachment lookup", "err", err)
		}
	}

	return writeJSON(c, fiber.StatusOK, map[string]string{"action": overall})
}

// The four searches. Each parses `q`, runs a COUNT and a page, and returns the
// {status,total,records} envelope the console's grids read.

func (s *Server) handleSearchConnection(c fiber.Ctx) error {
	return s.search(c, query.TableConnection, query.ConnectionFields)
}

func (s *Server) handleSearchDelivery(c fiber.Ctx) error {
	return s.search(c, query.TableDelivery, query.DeliveryFields)
}

func (s *Server) handleSearchTransaction(c fiber.Ctx) error {
	return s.search(c, query.TableTransaction, query.TransactionFields)
}

func (s *Server) handleSearchHashLookup(c fiber.Ctx) error {
	q := s.parseQ(c)
	ctx, cancel := dbCtx(c)
	defer cancel()

	res, err := s.searcher().SearchHashLookups(ctx, q)
	if err != nil {
		return s.fail500(c, err)
	}
	return writeJSON(c, fiber.StatusOK, res)
}

func (s *Server) search(c fiber.Ctx, table string, allowed map[string]struct{}) error {
	q := s.parseQ(c)
	ctx, cancel := dbCtx(c)
	defer cancel()

	res, err := s.searcher().SearchTable(ctx, table, allowed, q)
	if err != nil {
		return s.fail500(c, err)
	}
	return writeJSON(c, fiber.StatusOK, res)
}

// parseQ decodes the `q` parameter and logs a clamped page size.
//
// A malformed `q` is never an error — it yields the defaults — so there is
// nothing to report to the caller. The clamp is logged at Debug because it is
// the one place this service behaves differently from the Bun one, and an
// operator investigating "my export only returned 1000 rows" needs somewhere to
// find out why. The `q` value itself is NOT logged: it carries sender and
// recipient addresses.
func (s *Server) parseQ(c fiber.Ctx) query.Query {
	q := query.Parse(c.Query("q"))
	if _, _, clamped := q.LimitOffset(); clamped {
		s.log().Debug("search page size clamped",
			"path", c.Path(), "max", query.MaxLimit)
	}
	return q
}
