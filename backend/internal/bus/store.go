// Package bus persists committed business messages and immutable notification
// recipients in the same SQLite transaction as their business receipts.
package bus

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

var ErrState = errors.New("business notification requires review")

type Event struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Reference  string     `json:"reference"`
	Title      string     `json:"title"`
	Text       string     `json:"text"`
	Created    string     `json:"created"`
	Deliveries []Delivery `json:"deliveries"`
}
type Delivery struct {
	ID           string `json:"id"`
	EventID      string `json:"eventId"`
	ChannelID    int64  `json:"channelId"`
	ChannelName  string `json:"channelName"`
	ChannelType  string `json:"channelType"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Actor        string `json:"actor,omitempty"`
	Updated      string `json:"updated"`
	ConfigDigest string `json:"-"`
}
type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db} }
func Hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Bounded(s string, n int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func schema(ctx context.Context, tx *sql.Tx) error {
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS GO_BUSINESS_EVENTS(ID TEXT PRIMARY KEY,KIND TEXT NOT NULL,REFERENCE TEXT NOT NULL,TITLE TEXT NOT NULL,BODY TEXT NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_NOTIFICATION_DELIVERIES(ID TEXT PRIMARY KEY,EVENT_ID TEXT NOT NULL,CHANNEL_ID INTEGER NOT NULL,CHANNEL_NAME TEXT NOT NULL,CHANNEL_TYPE TEXT NOT NULL,CONFIG_DIGEST TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',ACTOR TEXT NOT NULL DEFAULT '',UPDATED TEXT NOT NULL,UNIQUE(EVENT_ID,CHANNEL_ID))`,
		`CREATE INDEX IF NOT EXISTS GO_NOTIFICATION_DELIVERY_STATE ON GO_NOTIFICATION_DELIVERIES(STATE,UPDATED,ID)`,
	} {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

const channelsSQL = `SELECT ID,COALESCE(NAME,''),COALESCE(TYPE,''),COALESCE(CONFIG,''),COALESCE(SWITCHS,''),COALESCE(INTERACTIVE,0),COALESCE(ENABLED,0),COALESCE(NOTE,'') FROM MESSAGE_CLIENT`

func scanChannel(row interface{ Scan(...any) error }) (notificationconfig.Channel, error) {
	var c notificationconfig.Channel
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Switches, &c.Interactive, &c.Enabled, &c.Note)
	return c, err
}
func selected(c notificationconfig.Channel, kind string) bool {
	var switches []string
	if c.Enabled == 0 || json.Unmarshal([]byte(c.Switches), &switches) != nil {
		return false
	}
	for _, v := range switches {
		if v == kind {
			return true
		}
	}
	return false
}

func Emit(ctx context.Context, tx *sql.Tx, kind, key, reference, title, body string) error {
	switch kind {
	case "transfer_finished", "download_start", "rss_added", "rss_finished":
	default:
		return ErrState
	}
	if key == "" || title == "" {
		return ErrState
	}
	if err := schema(ctx, tx); err != nil {
		return err
	}
	id, now := Hash([]string{kind, key}), time.Now().UTC().Format(time.RFC3339Nano)
	title, body = Bounded(title, 600), Bounded(body, 10000)
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_BUSINESS_EVENTS VALUES(?,?,?,?,?,?) ON CONFLICT(ID) DO NOTHING`, id, kind, reference, title, body, now)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_BUSINESS_EVENTS WHERE ID=?)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrState
		}
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='MESSAGE_CLIENT' AND type='table')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	rows, err := tx.QueryContext(ctx, channelsSQL+` WHERE COALESCE(ENABLED,0)<>0 ORDER BY ID LIMIT 1001`)
	if err != nil {
		return err
	}
	channels := []notificationconfig.Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			rows.Close()
			return err
		}
		channels = append(channels, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(channels) > 1000 {
		return ErrState
	}
	for _, c := range channels {
		if !selected(c, kind) {
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO GO_NOTIFICATION_DELIVERIES(ID,EVENT_ID,CHANNEL_ID,CHANNEL_NAME,CHANNEL_TYPE,CONFIG_DIGEST,STATE,UPDATED) VALUES(?,?,?,?,?,?,'pending',?)`, Hash([]string{id, fmt.Sprint(c.ID)}), id, c.ID, c.Name, c.Type, Hash(c), now)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrState
		}
	}
	return nil
}
func (s *Store) exists(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_BUSINESS_EVENTS' AND type='table')`).Scan(&exists)
	return exists, err
}

const deliverySQL = `SELECT ID,EVENT_ID,CHANNEL_ID,CHANNEL_NAME,CHANNEL_TYPE,CONFIG_DIGEST,STATE,REASON,ACTOR,UPDATED FROM GO_NOTIFICATION_DELIVERIES`

func scanDelivery(row interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.EventID, &d.ChannelID, &d.ChannelName, &d.ChannelType, &d.ConfigDigest, &d.State, &d.Reason, &d.Actor, &d.Updated)
	return d, err
}
func (s *Store) Pending(ctx context.Context) ([]Delivery, error) {
	result := []Delivery{}
	exists, err := s.exists(ctx)
	if err != nil || !exists {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, deliverySQL+` WHERE STATE='pending' ORDER BY UPDATED,ID LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// Claim rechecks the exact event-time channel configuration inside the sending
// intent transaction. The returned client configuration is frozen, never a new
// endpoint read after verifying an old digest.
func (s *Store) Claim(ctx context.Context, expected Delivery) (Event, notificationconfig.Channel, error) {
	var e Event
	var c notificationconfig.Channel
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return e, c, err
	}
	defer tx.Rollback()
	d, err := scanDelivery(tx.QueryRowContext(ctx, deliverySQL+` WHERE ID=?`, expected.ID))
	if err != nil {
		return e, c, err
	}
	if d.State != "pending" || Hash(d) != Hash(expected) {
		return e, c, ErrState
	}
	if err := tx.QueryRowContext(ctx, `SELECT ID,KIND,REFERENCE,TITLE,BODY,CREATED FROM GO_BUSINESS_EVENTS WHERE ID=?`, d.EventID).Scan(&e.ID, &e.Kind, &e.Reference, &e.Title, &e.Text, &e.Created); err != nil {
		return e, c, err
	}
	c, err = scanChannel(tx.QueryRowContext(ctx, channelsSQL+` WHERE ID=?`, d.ChannelID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return e, c, err
	}
	state, reason := "sending", ""
	if errors.Is(err, sql.ErrNoRows) || Hash(c) != d.ConfigDigest || !selected(c, e.Kind) {
		state, reason = "blocked", "channel configuration changed or was removed; no delivery attempted"
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_NOTIFICATION_DELIVERIES SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='pending'`, state, reason, time.Now().UTC().Format(time.RFC3339Nano), d.ID)
	if err != nil {
		return e, c, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return e, c, err
	}
	if n != 1 {
		return e, c, ErrState
	}
	if err := tx.Commit(); err != nil {
		return e, c, err
	}
	if state == "blocked" {
		return e, c, ErrState
	}
	return e, c, nil
}
func (s *Store) Finish(ctx context.Context, id, state, reason string) error {
	if state != "sent" && state != "uncertain" && state != "blocked" {
		return ErrState
	}
	res, err := s.db.ExecContext(ctx, `UPDATE GO_NOTIFICATION_DELIVERIES SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='sending'`, state, reason, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	return err
}
func (s *Store) Dismiss(ctx context.Context, id, actor, reason string) error {
	reason = strings.TrimSpace(reason)
	if actor == "" || reason == "" || len(reason) > 1000 {
		return ErrState
	}
	res, err := s.db.ExecContext(ctx, `UPDATE GO_NOTIFICATION_DELIVERIES SET STATE='dismissed',REASON=?,ACTOR=?,UPDATED=? WHERE ID=? AND STATE IN ('pending','sending','uncertain','blocked')`, reason, actor, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	return err
}
func (s *Store) List(ctx context.Context) ([]Event, error) {
	events := []Event{}
	exists, err := s.exists(ctx)
	if err != nil || !exists {
		return events, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ID,KIND,REFERENCE,TITLE,BODY,CREATED FROM GO_BUSINESS_EVENTS ORDER BY EXISTS(SELECT 1 FROM GO_NOTIFICATION_DELIVERIES d WHERE d.EVENT_ID=GO_BUSINESS_EVENTS.ID AND d.STATE IN ('sending','uncertain','blocked','pending')) DESC,CREATED DESC,ID LIMIT 50`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		e := Event{Deliveries: []Delivery{}}
		if err := rows.Scan(&e.ID, &e.Kind, &e.Reference, &e.Title, &e.Text, &e.Created); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range events {
		rows, err := s.db.QueryContext(ctx, deliverySQL+` WHERE EVENT_ID=? ORDER BY CHANNEL_ID LIMIT 1001`, events[i].ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			d, err := scanDelivery(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			events[i].Deliveries = append(events[i].Deliveries, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(events[i].Deliveries) > 1000 {
			return nil, ErrState
		}
	}
	return events, nil
}
