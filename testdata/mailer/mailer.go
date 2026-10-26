// Package mailer is the library sample: a module without a main, analysed
// through its exported API (codegraph analyze -library). Each exported function
// and method is an entry point; Sender's Send runs both implementations.
package mailer

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/zalgonoise/codegraph/testdata/mailer/internal/tmpl"
)

const relay = "http://relay:8025"

// Sender sends a message.
type Sender interface {
	Send(ctx context.Context, to, body string) error
}

// Client sends messages through the mail relay's HTTP API.
type Client struct{ http *http.Client }

// New returns a client of the relay.
func New() *Client { return &Client{http: http.DefaultClient} }

// Send posts a message to the relay.
func (c *Client) Send(ctx context.Context, to, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relay+"/send", strings.NewReader(tmpl.Render(to, body)))
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// NewQueued returns a Sender that queues messages in the outbox table.
func NewQueued(db *sql.DB) Sender { return &queued{db: db} }

// queued is unexported: callers reach it through Sender only.
type queued struct{ db *sql.DB }

func (q *queued) Send(ctx context.Context, to, body string) error {
	_, err := q.db.ExecContext(ctx, "INSERT INTO outbox (recipient, body) VALUES (?, ?)", to, tmpl.Render(to, body))
	return err
}

// Flush is not part of the API: no exported type or interface offers it.
func (q *queued) Flush(ctx context.Context) error {
	_, err := q.db.ExecContext(ctx, "DELETE FROM outbox_sent")
	return err
}
