//go:build linux

package hachaos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	msc4194 = "msc4194"
	// KindMSC4194Spam is a message of the user whose events the probe
	// redacts, and kindMSC4194Redact one request to redact them.
	kindMSC4194Spam   = "msc4194-spam"
	kindMSC4194Redact = "msc4194-redact"
	// Msc4194Interval paces the redactor. It redacts at most
	// msc4194PageLimit events per request and pages while more are left,
	// so it keeps up with the spammer, who writes every mscInterval.
	msc4194Interval  = 3 * time.Second
	msc4194PageLimit = 2
	// Msc4194MaxPages bounds the pages one round of redaction asks for, so
	// a server that keeps answering is_more_events cannot hold the probe.
	msc4194MaxPages = 50
)

// redactProbe is MSC4194 traffic that runs alongside the workload: a local
// user writes into a room of its own, and the room's creator keeps
// redacting everything that user sent with
// POST /rooms/{roomId}/redact/user/{userId}, a few events per request,
// paging while is_more_events is true. Redactions are the roomserver's
// writes, so every fault also hits a moderator halfway through a batch.
//
// After the scenario the moderator redacts what is left, and every
// acknowledged message must be redacted while the moderator's own message
// is not.
type redactProbe struct {
	w *workload
	e *environment

	room      string
	spammer   *mxClient
	moderator *mxClient
	kept      string

	mu       sync.Mutex
	spam     []*sendRecord
	rounds   int
	pages    int
	redacted int
	failures []string
}

type redactUserResponse struct {
	IsMoreEvents   bool `json:"is_more_events"`
	RedactedEvents struct {
		Total      int `json:"total"`
		SoftFailed int `json:"soft_failed"`
	} `json:"redacted_events"`
}

func (c *mxClient) redactUserEvents(ctx context.Context, room, user string, limit int) (*redactUserResponse, error) {
	path := fmt.Sprintf("/_matrix/client/unstable/org.matrix.msc4194/rooms/%s/redact/user/%s?limit=%d", url.PathEscape(room), url.PathEscape(user), limit)
	var out redactUserResponse
	err := c.do(ctx, http.MethodPost, path, map[string]any{"reason": "hachaos"}, &out)
	return &out, err
}

// redactedEvent tells whether the server returns eventID redacted.
func (c *mxClient) redactedEvent(ctx context.Context, room, eventID string) (bool, error) {
	var out struct {
		Content  map[string]any `json:"content"`
		Unsigned struct {
			RedactedBecause map[string]any `json:"redacted_because"`
		} `json:"unsigned"`
	}
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", url.PathEscape(room), url.PathEscape(eventID))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return false, err
	}
	return out.Unsigned.RedactedBecause != nil && len(out.Content) == 0, nil
}

// startRedactProbe sets up the probe's room and starts its traffic on the
// workload, so it stops with it.
//
//nolint:contextcheck // Traffic runs on the workload's context, which outlives setup's deadline.
func startRedactProbe(ctx context.Context, w *workload) (*redactProbe, error) {
	e := w.e
	p := &redactProbe{w: w, e: e, moderator: w.alice}
	var err error
	if p.spammer, err = e.localUser(ctx, "sam"); err != nil {
		return nil, err
	}
	p.spammer.unavailable = &w.unavailable
	started := time.Now()
	if err = retryingSetup(ctx, func() error {
		defer e.watchSetup(ctx, "create-room-msc4194")()
		p.room, err = w.alice.patient().createRoom(ctx, "msc4194")
		return err
	}); err != nil {
		return nil, fmt.Errorf("create the MSC4194 room: %w", err)
	}
	w.noteSlowSetup("creating the MSC4194 room", started)
	if err = retrying(ctx, func() error { return p.spammer.join(ctx, p.room, "") }); err != nil {
		return nil, fmt.Errorf("join the MSC4194 room: %w", err)
	}
	txn := "msc4194-kept-" + nonce()
	if err = retrying(ctx, func() error {
		p.kept, err = p.moderator.send(ctx, p.room, txn, "msc4194 moderator message")
		return err
	}); err != nil {
		return nil, fmt.Errorf("send the moderator's message: %w", err)
	}
	running := w.ctx
	w.run(func() { p.spamWriter(running) })
	w.run(func() { p.redactor(running) })
	return p, nil
}

// spamWriter sends the messages the moderator redacts.
func (p *redactProbe) spamWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(mscInterval):
		}
		rec := &sendRecord{Txn: p.w.nextTxn(kindMSC4194Spam), Nonce: "spam" + nonce(), First: time.Now()}
		p.mu.Lock()
		p.spam = append(p.spam, rec)
		p.mu.Unlock()
		p.w.retry(ctx, kindMSC4194Spam, rec, func(ctx context.Context) error {
			eventID, err := p.spammer.send(ctx, p.room, rec.Txn, rec.Nonce)
			if err == nil {
				p.mu.Lock()
				rec.EventID = eventID
				p.mu.Unlock()
			}
			return err
		})
	}
}

// redactor redacts the spammer's events in rounds, as a moderation bot does
// after a ban. A failed request ends the round; the next round starts again
// from the newest event, which skips what is already redacted.
func (p *redactProbe) redactor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(msc4194Interval):
		}
		_ = p.redactRound(ctx, msc4194PageLimit, true)
	}
}

// redactRound pages through the spammer's unredacted events until none are
// left, returning the first error. When record is set, each request is
// recorded with the workload's attempts.
func (p *redactProbe) redactRound(ctx context.Context, limit int, record bool) error {
	p.mu.Lock()
	p.rounds++
	p.mu.Unlock()
	for range msc4194MaxPages {
		start := time.Now()
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := p.moderator.redactUserEvents(requestCtx, p.room, p.spammer.user, limit)
		cancel()
		if err != nil && ctx.Err() != nil {
			return err
		}
		if record {
			p.w.record(kindMSC4194Redact, start, err == nil)
		}
		if err != nil {
			if !retryable(err) {
				p.mu.Lock()
				p.failures = append(p.failures, fmt.Sprintf("redact the spammer's events: %v", err))
				p.mu.Unlock()
			}
			return err
		}
		p.mu.Lock()
		p.pages++
		p.redacted += res.RedactedEvents.Total
		p.mu.Unlock()
		if res.RedactedEvents.Total > limit {
			p.mu.Lock()
			p.failures = append(p.failures, fmt.Sprintf("a request with limit %d redacted %d events", limit, res.RedactedEvents.Total))
			p.mu.Unlock()
		}
		if !res.IsMoreEvents {
			return nil
		}
	}
	return fmt.Errorf("still more events to redact after %d pages", msc4194MaxPages)
}

// verifyRedactions checks that the moderator can redact everything the
// spammer sent once the scenario is over, and that nothing else was
// redacted.
func verifyRedactions(ctx context.Context, r *result, p *redactProbe, settle time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	var acked []sendRecord
	for _, rec := range p.spam {
		if rec.Acked {
			acked = append(acked, *rec)
		}
	}
	sent, rounds, pages, redacted := len(p.spam), p.rounds, p.pages, p.redacted
	failures := slices.Clone(p.failures)
	p.mu.Unlock()
	r.notef("MSC4194 probe: %d of %d spam messages acknowledged; %d redaction rounds made %d successful requests, which redacted %d events", len(acked), sent, rounds, pages, redacted)
	if len(acked) == 0 {
		r.failf("MSC4194 probe: no spam message was acknowledged")
	}

	// Writes in flight when the traffic stopped may have been stored after
	// the last round, so the moderator redacts once more, as a bot whose
	// request failed would.
	awaitAll(ctx, r, settle, "acknowledged spam message redacted by MSC4194", func() []string {
		if err := p.redactRound(ctx, 25, false); err != nil {
			return []string{err.Error()}
		}
		var missing []string
		for _, rec := range acked {
			done, err := p.moderator.redactedEvent(ctx, p.room, rec.EventID)
			if err != nil {
				return []string{err.Error()}
			}
			if !done {
				missing = append(missing, rec.Txn)
			}
		}
		return missing
	})
	if done, err := p.moderator.redactedEvent(ctx, p.room, p.kept); err != nil {
		r.failf("MSC4194 probe: read the moderator's message: %v", err)
	} else if done {
		r.failf("MSC4194 probe: the moderator's own message %s was redacted", p.kept)
	}
	// An event is redacted twice only when a request was retried after its
	// answer was lost, which is allowed and only reported.
	if twice, err := p.e.redactedMoreThanOnce(ctx, p.room); err != nil {
		r.notef("MSC4194 probe: count repeated redactions: %v", err)
	} else if twice > 0 {
		r.notef("MSC4194 probe: %d events were redacted more than once", twice)
	}
	for _, f := range failures {
		r.failf("MSC4194 probe: %s", f)
	}
}

// redactedMoreThanOnce counts the events of room with more than one
// validated redaction.
func (e *environment) redactedMoreThanOnce(ctx context.Context, room string) (int, error) {
	var n int
	err := e.pg.query(ctx, func(conn *pgx.Conn) error {
		return conn.QueryRow(ctx, `SELECT count(*) FROM (
			SELECT r.redacts_event_id FROM roomserver_redactions r
			JOIN roomserver_events ev ON ev.event_id = r.redacts_event_id
			JOIN roomserver_rooms rm ON rm.room_nid = ev.room_nid
			WHERE rm.room_id = $1 AND r.validated
			GROUP BY r.redacts_event_id HAVING count(*) > 1) AS twice`, room).Scan(&n)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
