package mediaserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

type WebhookControl struct {
	Revision string `json:"revision"`
	Enabled  bool   `json:"enabled"`
	Actor    string `json:"actor"`
	Server   string `json:"server"`
	Scope    string `json:"scope"`
	Changed  string `json:"changed"`
}

type WebhookReceipt struct {
	ID           string       `json:"id"`
	Received     string       `json:"received"`
	Event        WebhookEvent `json:"event"`
	EventID      string       `json:"eventId"`
	Deduplicated bool         `json:"deduplicated"`
}
type WebhookStore struct{ db *sql.DB }

func NewWebhookStore(db *sql.DB) *WebhookStore { return &WebhookStore{db} }

func decodeWebhookControl(raw string) (WebhookControl, error) {
	c := WebhookControl{}
	if raw == "" {
		return c, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 64<<10 || !json.Valid([]byte(raw)) || d.Decode(&c) != nil || !refreshHex(c.Revision, 32) || c.Actor == "" || len(c.Actor) > 256 || webhookIcon(c.Server) == "" || !refreshHex(c.Scope, 64) {
		return c, ErrWebhookState
	}
	if _, err := time.Parse(time.RFC3339Nano, c.Changed); err != nil {
		return c, ErrWebhookState
	}
	return c, nil
}
func webhookControl(ctx context.Context, r refreshReader) (WebhookControl, error) {
	raw, err := refreshSetting(ctx, r, WebhookControlKey)
	if err != nil {
		return WebhookControl{}, err
	}
	return decodeWebhookControl(raw)
}
func (s *WebhookStore) Control(ctx context.Context) (WebhookControl, error) {
	return webhookControl(ctx, s.db)
}
func (s *WebhookStore) SetControl(ctx context.Context, expected, next WebhookControl, validate func() error) error {
	if validate == nil || next.Revision == expected.Revision {
		return ErrWebhookState
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err := decodeWebhookControl(string(raw)); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := webhookControl(ctx, tx)
	if err != nil || current != expected {
		return ErrWebhookState
	}
	if err := validate(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS SYSTEM_DICT(ID INTEGER PRIMARY KEY AUTOINCREMENT,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`); err != nil {
		return err
	}
	var previous int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, WebhookControlKey).Scan(&previous); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, string(raw), WebhookControlKey)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if previous > 0 && n != previous {
		return ErrWebhookState
	}
	if previous == 0 {
		res, err = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE) VALUES('SystemConfig',?,?,'')`, WebhookControlKey, string(raw))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		if err != nil || n != 1 {
			return ErrWebhookState
		}
	}
	saved, err := webhookControl(ctx, tx)
	if err != nil || saved != next {
		return ErrWebhookState
	}
	var matches int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? AND VALUE=?`, WebhookControlKey, string(raw)).Scan(&matches); err != nil || matches != max(previous, 1) {
		return ErrWebhookState
	}
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit()
}
func webhookSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_MEDIA_WEBHOOK_RECEIPTS(ID TEXT PRIMARY KEY,REVISION TEXT NOT NULL,DIGEST TEXT NOT NULL,DOCUMENT TEXT NOT NULL,DOCUMENT_FP TEXT NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS GO_MEDIA_WEBHOOK_CREATED ON GO_MEDIA_WEBHOOK_RECEIPTS(CREATED,ID)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func readWebhookReceipt(ctx context.Context, r refreshReader, id string) (WebhookReceipt, string, string, error) {
	p := WebhookReceipt{}
	yes, err := refreshTable(ctx, r, "GO_MEDIA_WEBHOOK_RECEIPTS")
	if err != nil {
		return p, "", "", err
	}
	if !yes {
		return p, "", "", ErrWebhookMissing
	}
	var epoch, digest, raw, fp string
	err = r.QueryRowContext(ctx, `SELECT REVISION,DIGEST,DOCUMENT,DOCUMENT_FP FROM GO_MEDIA_WEBHOOK_RECEIPTS WHERE ID=?`, id).Scan(&epoch, &digest, &raw, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return p, "", "", ErrWebhookMissing
	}
	if err != nil {
		return p, "", "", err
	}
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &p) != nil || RefreshDigest(p) != fp || !refreshHex(p.ID, 32) || p.ID != id || !refreshHex(epoch, 32) || !refreshHex(digest, 64) || p.EventID != bus.Hash([]string{"mediaserver_message", p.ID}) || p.Deduplicated || p.Event.Validate() != nil {
		return p, "", "", ErrWebhookState
	}
	if _, err := time.Parse(time.RFC3339Nano, p.Received); err != nil {
		return p, "", "", ErrWebhookState
	}
	return p, epoch, digest, nil
}
func (s *WebhookStore) Get(ctx context.Context, id string) (WebhookReceipt, error) {
	if !refreshHex(id, 32) {
		return WebhookReceipt{}, ErrWebhookMissing
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WebhookReceipt{}, err
	}
	defer tx.Rollback()
	p, _, _, err := readWebhookReceipt(ctx, tx, id)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}
func (s *WebhookStore) List(ctx context.Context, page int) ([]WebhookReceipt, error) {
	out := []WebhookReceipt{}
	if page < 1 || page > 10000 {
		return out, ErrWebhook
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	yes, err := refreshTable(ctx, tx, "GO_MEDIA_WEBHOOK_RECEIPTS")
	if err != nil {
		return nil, err
	}
	if !yes {
		return out, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT ID FROM GO_MEDIA_WEBHOOK_RECEIPTS ORDER BY CREATED DESC,ID DESC LIMIT 30 OFFSET ?`, (page-1)*30)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		p, _, _, err := readWebhookReceipt(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, tx.Commit()
}

func (s *WebhookStore) Receive(ctx context.Context, c WebhookControl, p WebhookReceipt, digest string, validate func() error) (WebhookReceipt, error) {
	if !c.Enabled || !refreshHex(p.ID, 32) || !refreshHex(digest, 64) || p.Event.Server != c.Server || p.Deduplicated || p.Event.Validate() != nil || validate == nil {
		return p, ErrWebhookState
	}
	p.EventID = bus.Hash([]string{"mediaserver_message", p.ID})
	if _, err := time.Parse(time.RFC3339Nano, p.Received); err != nil {
		return p, ErrWebhookState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	current, err := webhookControl(ctx, tx)
	if err != nil || current != c {
		return p, ErrWebhookState
	}
	if err := validate(); err != nil {
		return p, err
	}
	old, revision, original, err := readWebhookReceipt(ctx, tx, p.ID)
	if err == nil {
		if revision != c.Revision || original != digest {
			return p, ErrWebhookState
		}
		if err := verifyWebhookBusinessEvent(ctx, tx, old); err != nil {
			return p, err
		}
		if err := validate(); err != nil {
			return p, err
		}
		if err := tx.Commit(); err != nil {
			return p, err
		}
		old.Deduplicated = true
		return old, nil
	}
	if !errors.Is(err, ErrWebhookMissing) {
		return p, err
	}
	if err := webhookSchema(ctx, tx); err != nil {
		return p, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_WEBHOOK_RECEIPTS(ID,REVISION,DIGEST,DOCUMENT,DOCUMENT_FP,CREATED) VALUES(?,?,?,?,?,?)`, p.ID, c.Revision, digest, string(raw), RefreshDigest(p), p.Received)
	if err != nil {
		return p, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return p, ErrWebhookState
	}
	title, body := p.Event.Message(p.Received)
	if err := bus.EmitData(ctx, tx, "mediaserver_message", p.ID, "media-webhook:"+p.ID, title, body, p.Event); err != nil {
		return p, err
	}
	saved, epoch, original, err := readWebhookReceipt(ctx, tx, p.ID)
	if err != nil || epoch != c.Revision || original != digest || RefreshDigest(saved) != RefreshDigest(p) {
		return p, ErrWebhookState
	}
	if err := verifyWebhookBusinessEvent(ctx, tx, p); err != nil {
		return p, err
	}
	current, err = webhookControl(ctx, tx)
	if err != nil || current != c {
		return p, ErrWebhookState
	}
	if err := validate(); err != nil {
		return p, err
	}
	return p, tx.Commit()
}
func verifyWebhookBusinessEvent(ctx context.Context, r refreshReader, p WebhookReceipt) error {
	for _, table := range []string{"GO_BUSINESS_EVENTS", "GO_BUSINESS_EVENT_DATA"} {
		yes, err := refreshTable(ctx, r, table)
		if err != nil {
			return err
		}
		if !yes {
			return ErrWebhookState
		}
	}
	title, body := p.Event.Message(p.Received)
	var matches bool
	raw, _ := json.Marshal(p.Event)
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_BUSINESS_EVENTS e JOIN GO_BUSINESS_EVENT_DATA d ON e.ID=d.EVENT_ID WHERE e.ID=? AND e.KIND='mediaserver_message' AND e.REFERENCE=? AND e.TITLE=? AND e.BODY=? AND d.DATA=?)`, p.EventID, "media-webhook:"+p.ID, bus.Bounded(title, 600), bus.Bounded(body, 10000), string(raw)).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return ErrWebhookState
	}
	return nil
}

// An explicit sender identity deduplicates only within its original receiver
// generation. Missing identity gets a fresh receipt; content/time-window guesses
// would discard legitimate repeated playback of the same item.
func WebhookReceiptID(revision, server, identity, nonce string) (string, error) {
	if !refreshHex(revision, 32) || webhookIcon(server) == "" {
		return "", ErrWebhookState
	}
	if identity == "" {
		if !refreshHex(nonce, 32) {
			return "", ErrWebhookState
		}
		return nonce, nil
	}
	if len(identity) > 512 || strings.TrimSpace(identity) != identity || strings.ContainsAny(identity, "\r\n\t\x00") {
		return "", ErrWebhook
	}
	return RefreshDigest([]string{revision, server, identity})[:32], nil
}
func WebhookPayloadDigest(server string, raw []byte) (string, error) {
	v, err := webhookJSON(raw)
	if err != nil {
		return "", err
	}
	return RefreshDigest([]any{server, v}), nil
}

func WebhookPage(raw string) (int, error) {
	if raw == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 10000 {
		return 0, ErrWebhook
	}
	return n, nil
}
