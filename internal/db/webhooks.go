package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrWebhookNotFound is returned for an unknown webhook id.
var ErrWebhookNotFound = errors.New("webhook not found")

// Webhook is a registered endpoint. Secret is only populated by Get/Active
// (for signing); it is never returned by List.
type Webhook struct {
	ID        int64    `json:"id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Active    bool     `json:"active"`
	CreatedAt int64    `json:"created_at"`
	LastFired int64    `json:"last_fired,omitempty"`
	FailCount int      `json:"fail_count"`
	Pending   int      `json:"pending"`
	Secret    string   `json:"-"`
}

// Delivery is one outbox row.
type Delivery struct {
	ID          int64           `json:"-"`
	WebhookID   int64           `json:"webhook_id"`
	DeliveryID  string          `json:"delivery_id"`
	Event       string          `json:"event"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	NextAttempt int64           `json:"next_attempt"`
	LastStatus  int             `json:"last_status,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   int64           `json:"created_at"`
	DoneAt      int64           `json:"done_at,omitempty"`
}

// Create stores a webhook and returns its id.
func (r *WebhookRepo) Create(ctx context.Context, url string, events []string, secret string) (int64, error) {
	ev, _ := json.Marshal(events)
	res, err := r.db.ExecContext(ctx, `INSERT INTO webhooks(url, events, secret, active) VALUES(?,?,?,1)`, url, string(ev), secret)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const webhookCols = `w.id, w.url, w.events, COALESCE(w.secret,''), w.active, COALESCE(w.created_at,0),
	COALESCE(w.last_fired,0), COALESCE(w.fail_count,0),
	(SELECT count(*) FROM webhook_deliveries d WHERE d.webhook_id = w.id AND d.status = 'pending')`

func scanWebhook(sc interface{ Scan(...any) error }) (Webhook, error) {
	var w Webhook
	var ev string
	err := sc.Scan(&w.ID, &w.URL, &ev, &w.Secret, &w.Active, &w.CreatedAt, &w.LastFired, &w.FailCount, &w.Pending)
	if err == nil {
		_ = json.Unmarshal([]byte(ev), &w.Events)
	}
	return w, err
}

func (r *WebhookRepo) query(ctx context.Context, where string, args ...any) ([]Webhook, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM webhooks w `+where+` ORDER BY w.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// List returns all webhooks without their secrets.
func (r *WebhookRepo) List(ctx context.Context) ([]Webhook, error) {
	ws, err := r.query(ctx, "")
	for i := range ws {
		ws[i].Secret = ""
	}
	return ws, err
}

// Active returns the active webhooks, with secrets.
func (r *WebhookRepo) Active(ctx context.Context) ([]Webhook, error) {
	return r.query(ctx, "WHERE w.active = 1")
}

// Get returns one webhook, with its secret.
func (r *WebhookRepo) Get(ctx context.Context, id int64) (*Webhook, error) {
	w, err := scanWebhook(r.db.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks w WHERE w.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWebhookNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// Delete removes a webhook and (by cascade) its outbox rows.
func (r *WebhookRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWebhookNotFound
	}
	return nil
}

// SetActive enables or disables a webhook; enabling resets its failure count.
func (r *WebhookRepo) SetActive(ctx context.Context, id int64, active bool) error {
	q := `UPDATE webhooks SET active = 0 WHERE id = ?`
	if active {
		q = `UPDATE webhooks SET active = 1, fail_count = 0 WHERE id = ?`
	}
	res, err := r.db.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrWebhookNotFound
	}
	return nil
}

// Enqueue adds one delivery to the outbox.
func (r *WebhookRepo) Enqueue(ctx context.Context, webhookID int64, deliveryID, event string, payload []byte) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO webhook_deliveries(webhook_id, delivery_id, event, payload) VALUES(?,?,?,?)`,
		webhookID, deliveryID, event, string(payload))
	return err
}

const deliveryCols = `id, webhook_id, delivery_id, event, payload, status, attempts, next_attempt,
	COALESCE(last_status,0), COALESCE(last_error,''), created_at, COALESCE(done_at,0)`

func scanDeliveries(rows *sql.Rows) ([]Delivery, error) {
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var p string
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.DeliveryID, &d.Event, &p, &d.Status, &d.Attempts, &d.NextAttempt,
			&d.LastStatus, &d.LastError, &d.CreatedAt, &d.DoneAt); err != nil {
			return nil, err
		}
		d.Payload = json.RawMessage(p)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Due returns pending deliveries of active webhooks whose next attempt is due,
// oldest first.
func (r *WebhookRepo) Due(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE status = 'pending' AND next_attempt <= ?
		  AND webhook_id IN (SELECT id FROM webhooks WHERE active = 1)
		ORDER BY id LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	return scanDeliveries(rows)
}

// Deliveries returns a webhook's most recent outbox rows, newest first.
func (r *WebhookRepo) Deliveries(ctx context.Context, webhookID int64, limit int) ([]Delivery, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE webhook_id = ? ORDER BY id DESC LIMIT ?`, webhookID, limit)
	if err != nil {
		return nil, err
	}
	return scanDeliveries(rows)
}

// Delivered marks a delivery done and resets the webhook's failure count.
func (r *WebhookRepo) Delivered(ctx context.Context, d Delivery, status int, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET status='delivered', attempts=attempts+1,
		last_status=?, last_error=NULL, done_at=? WHERE id=?`, status, now.Unix(), d.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE webhooks SET last_fired=?, fail_count=0 WHERE id=?`, now.Unix(), d.WebhookID); err != nil {
		return err
	}
	return tx.Commit()
}

// Failed records a failed attempt. With next zero the delivery is given up
// (status failed); otherwise it is retried at next. It returns the webhook's
// consecutive failure count, and disables the webhook once that reaches
// disableAfter (when > 0).
func (r *WebhookRepo) Failed(ctx context.Context, d Delivery, status int, msg string, next time.Time, now time.Time, disableAfter int) (fails int, disabled bool, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	if next.IsZero() {
		_, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET status='failed', attempts=attempts+1,
			last_status=?, last_error=?, done_at=? WHERE id=?`, status, msg, now.Unix(), d.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET attempts=attempts+1, last_status=?, last_error=?,
			next_attempt=? WHERE id=?`, status, msg, next.Unix(), d.ID)
	}
	if err != nil {
		return 0, false, err
	}
	if err = tx.QueryRowContext(ctx, `UPDATE webhooks SET fail_count = COALESCE(fail_count,0) + 1 WHERE id=? RETURNING fail_count`, d.WebhookID).Scan(&fails); err != nil {
		return 0, false, err
	}
	if disableAfter > 0 && fails >= disableAfter {
		if _, err = tx.ExecContext(ctx, `UPDATE webhooks SET active=0 WHERE id=? AND active=1`, d.WebhookID); err != nil {
			return 0, false, err
		}
		disabled = true
	}
	return fails, disabled, tx.Commit()
}

// Prune deletes delivered rows done before deliveredBefore and failed rows
// done before failedBefore. Pending rows are never pruned.
func (r *WebhookRepo) Prune(ctx context.Context, deliveredBefore, failedBefore time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM webhook_deliveries
		WHERE (status='delivered' AND done_at < ?) OR (status='failed' AND done_at < ?)`,
		deliveredBefore.Unix(), failedBefore.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
