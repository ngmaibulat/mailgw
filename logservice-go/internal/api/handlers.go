package api

import (
	"encoding/json"
	"net/http"

	"github.com/ngmaibulat/mailgw/logservice-go/internal/query"
	"github.com/ngmaibulat/mailgw/logservice-go/internal/store"
	"github.com/ngmaibulat/mailgw/logservice-go/internal/validate"
)

// handleRoot is the health check the compose stacks and the e2e suite use.
// Open, and answering a fixed body.
func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, bodyOK)
}

// handleNotFound reproduces the Bun catch-all: plain text, not JSON.
func (s *Server) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notFoundBody))
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
func (s *Server) handlePostConnection(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bodyFail)
		return
	}

	var c store.Connection
	if err := json.Unmarshal(body, &c); err != nil {
		s.log().Warn("malformed connection event", "err", err)
		writeJSON(w, http.StatusBadRequest, bodyFail)
		return
	}

	ctx, cancel := dbCtx(r)
	defer cancel()
	if err := s.store().InsertConnection(ctx, c); err != nil {
		s.fail500(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bodyOK)
}

// handlePostQueue records a queue event as a Transaction row.
//
// It writes ONLY the Transaction row. The connect-stage Connection row is
// already written by /api/connection; the Bun handler's commented-out
// insertConnection call is the scar from when this double-inserted.
func (s *Server) handlePostQueue(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bodyFail)
		return
	}

	var q store.Queue
	if err := json.Unmarshal(body, &q); err != nil {
		s.log().Warn("malformed queue event", "err", err)
		writeJSON(w, http.StatusBadRequest, bodyFail)
		return
	}

	ctx, cancel := dbCtx(r)
	defer cancel()
	if err := s.store().InsertTransaction(ctx, q); err != nil {
		s.fail500(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bodyOK)
}

// handlePostDelivery records one recipient's delivery outcome. The only
// validated endpoint.
//
// The 400 body is a bare {"status":"Fail"} with no detail, matching the Bun
// service: the reason goes to the log, where an operator can see it, and not to
// a caller who could use it to map the schema. The gateway would not read it
// either — it checks the status and nothing else.
func (s *Server) handlePostDelivery(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bodyFail)
		return
	}

	d, err := validate.ParseDelivery(body)
	if err != nil {
		// Warn, not Debug: a gateway whose events are all being rejected is
		// losing its audit trail permanently, and this line is the only place
		// that says so.
		s.log().Warn("delivery event rejected", "err", err)
		writeJSON(w, http.StatusBadRequest, bodyFail)
		return
	}

	ctx, cancel := dbCtx(r)
	defer cancel()
	if err := s.store().InsertDelivery(ctx, d); err != nil {
		s.fail500(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bodyOK)
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
// handler's `Array.isArray(body) ? body : []` did — NOT a 400.
func (s *Server) handleFilterMD5(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		s.fail500(w, r, err)
		return
	}

	var list []store.Attachment
	if err := json.Unmarshal(body, &list); err != nil {
		// Not an array, or not JSON at all. Allow, and say so once — a scanner
		// that starts refusing bodies it used to accept would defer mail
		// silently.
		s.log().Warn("filter/md5 body is not an attachment array, allowing", "err", err)
		writeJSON(w, http.StatusOK, map[string]string{"action": "allow"})
		return
	}

	ctx, cancel := dbCtx(r)
	defer cancel()

	digests := make([]string, 0, len(list))
	for _, a := range list {
		if a.MD5 != nil && *a.MD5 != "" {
			digests = append(digests, *a.MD5)
		}
	}

	blocked, err := s.store().BlockedMD5s(ctx, digests)
	if err != nil {
		s.fail500(w, r, err)
		return
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

	writeJSON(w, http.StatusOK, map[string]string{"action": overall})
}

// The four searches. Each parses `q`, runs a COUNT and a page, and returns the
// {status,total,records} envelope the console's grids read.

func (s *Server) handleSearchConnection(w http.ResponseWriter, r *http.Request) {
	s.search(w, r, query.TableConnection, query.ConnectionFields)
}

func (s *Server) handleSearchDelivery(w http.ResponseWriter, r *http.Request) {
	s.search(w, r, query.TableDelivery, query.DeliveryFields)
}

func (s *Server) handleSearchTransaction(w http.ResponseWriter, r *http.Request) {
	s.search(w, r, query.TableTransaction, query.TransactionFields)
}

func (s *Server) handleSearchHashLookup(w http.ResponseWriter, r *http.Request) {
	q := s.parseQ(r)
	ctx, cancel := dbCtx(r)
	defer cancel()

	res, err := s.searcher().SearchHashLookups(ctx, q)
	if err != nil {
		s.fail500(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, table string, allowed map[string]struct{}) {
	q := s.parseQ(r)
	ctx, cancel := dbCtx(r)
	defer cancel()

	res, err := s.searcher().SearchTable(ctx, table, allowed, q)
	if err != nil {
		s.fail500(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// parseQ decodes the `q` parameter and logs a clamped page size.
//
// A malformed `q` is never an error — it yields the defaults — so there is
// nothing to report to the caller. The clamp is logged at Debug because it is
// the one place this service now behaves differently from the Bun one, and an
// operator investigating "my export only returned 1000 rows" needs somewhere to
// find out why. The `q` value itself is NOT logged: it carries sender and
// recipient addresses.
func (s *Server) parseQ(r *http.Request) query.Query {
	q := query.Parse(r.URL.Query().Get("q"))
	if _, _, clamped := q.LimitOffset(); clamped {
		s.log().Debug("search page size clamped",
			"path", r.URL.Path, "max", query.MaxLimit)
	}
	return q
}
