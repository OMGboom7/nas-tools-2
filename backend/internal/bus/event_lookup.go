package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func eventData(ctx context.Context, r interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, e *Event) error {
	var exists bool
	if err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_BUSINESS_EVENT_DATA')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var raw string
	err := r.QueryRowContext(ctx, `SELECT DATA FROM GO_BUSINESS_EVENT_DATA WHERE EVENT_ID=?`, e.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > 64<<10 || !json.Valid([]byte(raw)) {
		return ErrState
	}
	e.Data = json.RawMessage(raw)
	return nil
}

// Stable lookup does not depend on whether an event remains in the inbox's
// most recent fifty. Old webhook receipts retain their original delivery facts.
func (s *Store) Get(ctx context.Context, id string) (Event, error) {
	e := Event{Deliveries: []Delivery{}}
	exists, err := s.exists(ctx)
	if err != nil {
		return e, err
	}
	if !exists {
		return e, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return e, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT ID,KIND,REFERENCE,TITLE,BODY,CREATED FROM GO_BUSINESS_EVENTS WHERE ID=?`, id).Scan(&e.ID, &e.Kind, &e.Reference, &e.Title, &e.Text, &e.Created); err != nil {
		return e, err
	}
	if err := eventData(ctx, tx, &e); err != nil {
		return e, err
	}
	rows, err := tx.QueryContext(ctx, deliverySQL+` WHERE EVENT_ID=? ORDER BY CHANNEL_ID LIMIT 1001`, id)
	if err != nil {
		return e, err
	}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			rows.Close()
			return e, err
		}
		e.Deliveries = append(e.Deliveries, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return e, err
	}
	if len(e.Deliveries) > 1000 {
		return e, ErrState
	}
	return e, tx.Commit()
}
