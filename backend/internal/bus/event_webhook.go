package bus

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const EventWebhookPlugin = "Webhook"
const EventWebhookKey = "plugin.Webhook"
const EventWebhookControlKey = "GoEventWebhook"
const MaxEventWebhookPayload = 2 << 20

var ErrEventWebhook = errors.New("event webhook scope or receipt changed")
var ErrEventWebhookMissing = errors.New("event webhook receipt not found")
var ErrEventWaiting = errors.New("original event prerequisite is still running")

type EventWebhookSpec struct {
	URL         string `json:"webhook_url"`
	Method      string `json:"method"`
	GetEncoding string `json:"get_encoding"`
}
type EventWebhookControl struct {
	Epoch        string `json:"epoch"`
	Actor        string `json:"actor"`
	Authority    string `json:"authority"`
	ConfigDigest string `json:"configDigest"`
	Created      string `json:"created"`
	Authorized   bool   `json:"authorized"`
	HTTP         bool   `json:"http"`
}
type EventWebhookEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}
type EventWebhookDefinition struct {
	Version      int                  `json:"version"`
	TargetDigest string               `json:"targetDigest"`
	ID           string               `json:"id"`
	EventID      string               `json:"eventId"`
	Reference    string               `json:"reference"`
	Control      EventWebhookControl  `json:"control"`
	Envelope     EventWebhookEnvelope `json:"envelope"`
	Source       *EventWebhookSource  `json:"source,omitempty"`
}
type EventWebhookSource struct {
	Kind, Reference, Origin, ScraperBinding string
	Ordinal                                 int
}

func (s EventWebhookSource) Valid() bool {
	return s.Kind == "subtitle_intake" && eventHookHex(s.Reference, 32) && eventHookHex(s.Origin, 64) && s.Ordinal >= 0 && s.Ordinal < 100000 && (s.ScraperBinding == "" || eventHookHex(s.ScraperBinding, 64))
}

type EventWebhookReceipt struct {
	ID          string                 `json:"id"`
	EventID     string                 `json:"eventId"`
	Type        string                 `json:"type"`
	Reference   string                 `json:"reference"`
	State       string                 `json:"state"`
	Reason      string                 `json:"reason"`
	Actor       string                 `json:"actor"`
	Created     string                 `json:"created"`
	Updated     string                 `json:"updated"`
	Status      int                    `json:"status"`
	Definition  EventWebhookDefinition `json:"-"`
	Fingerprint string                 `json:"-"`
}

func eventHookHex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidSignalType(v string) bool {
	switch v {
	case "emby.webhook", "jellyfin.webhook", "plex.webhook", "download.add", "download.fail", "transfer.finished", "transfer.fail", "subtitle.download", "subscribe.add", "subscribe.finished", "message.incoming", "search.start", "sourcefile.deleted", "libraryfile.deleted", "media.scrap.start", "plugin.reload", "douban.sync", "autoseed.start", "refresh.mediaserver", "site.signin":
		return true
	}
	return false
}
func DecodeEventWebhookSpec(raw string) (EventWebhookSpec, error) {
	s := EventWebhookSpec{GetEncoding: "legacy"}
	if raw == "" {
		return s, nil
	}
	var v map[string]any
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &v) != nil || v == nil {
		return s, ErrEventWebhook
	}
	for key, target := range map[string]*string{"webhook_url": &s.URL, "method": &s.Method, "get_encoding": &s.GetEncoding} {
		if v[key] != nil {
			value, ok := v[key].(string)
			if !ok {
				return s, ErrEventWebhook
			}
			*target = value
		}
	}
	s.Method = strings.ToLower(s.Method)
	if s.GetEncoding == "" {
		s.GetEncoding = "legacy"
	}
	if s.Method != "" && s.Method != "post" && s.Method != "get" || s.GetEncoding != "legacy" && s.GetEncoding != "json" {
		return s, ErrEventWebhook
	}
	if s.URL == "" {
		return s, nil
	}
	u, err := url.Parse(s.URL)
	if err != nil || len(s.URL) > 16000 || !utf8.ValidString(s.URL) || u.Hostname() == "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" || strings.ContainsAny(s.URL, "\r\n\x00") {
		return s, ErrEventWebhook
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return s, ErrEventWebhook
	}
	return s, nil
}
func DecodeEventWebhookControl(raw string) (EventWebhookControl, error) {
	c := EventWebhookControl{}
	if raw == "" {
		return c, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 64<<10 || !json.Valid([]byte(raw)) || d.Decode(&c) != nil || !eventHookHex(c.Epoch, 32) || c.Actor == "" || len(c.Actor) > 256 || !eventHookHex(c.Authority, 64) || !eventHookHex(c.ConfigDigest, 64) {
		return c, ErrEventWebhook
	}
	if _, err := time.Parse(time.RFC3339Nano, c.Created); err != nil {
		return c, ErrEventWebhook
	}
	return c, nil
}

type eventHookReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func eventHookTable(ctx context.Context, r eventHookReader, name string) (bool, error) {
	var yes bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&yes)
	return yes, err
}
func eventHookSetting(ctx context.Context, r eventHookReader, key string) (string, error) {
	yes, err := eventHookTable(ctx, r, "SYSTEM_DICT")
	if err != nil || !yes {
		return "", err
	}
	var raw string
	err = r.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
func eventHookSnapshot(ctx context.Context, r eventHookReader) (EventWebhookControl, EventWebhookSpec, bool, error) {
	c, s := EventWebhookControl{}, EventWebhookSpec{}
	raw, err := eventHookSetting(ctx, r, "UserInstalledPlugins")
	if err != nil {
		return c, s, false, err
	}
	if raw == "" {
		return c, s, false, nil
	}
	var installed []string
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &installed) != nil || len(installed) > 1024 {
		return c, s, false, ErrEventWebhook
	}
	found := false
	for _, id := range installed {
		found = found || id == EventWebhookPlugin
	}
	if !found {
		return c, s, false, nil
	}
	raw, err = eventHookSetting(ctx, r, EventWebhookControlKey)
	if err != nil {
		return c, s, false, err
	}
	c, err = DecodeEventWebhookControl(raw)
	if err != nil {
		return c, s, false, err
	}
	if !c.Authorized {
		return c, s, false, nil
	}
	raw, err = eventHookSetting(ctx, r, EventWebhookKey)
	if err != nil {
		return c, s, false, err
	}
	s, err = DecodeEventWebhookSpec(raw)
	if err != nil || HashRawEventConfig(raw) != c.ConfigDigest || s.URL == "" || s.Method == "" {
		return c, s, false, ErrEventWebhook
	}
	u, _ := url.Parse(s.URL)
	if u.Scheme == "http" && !c.HTTP {
		return c, s, false, ErrEventWebhook
	}
	return c, s, true, nil
}
func HashRawEventConfig(raw string) string {
	var v map[string]any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return ""
	}
	return Hash(v)
}
func (s *Store) EventWebhookScope(ctx context.Context) (EventWebhookControl, EventWebhookSpec, bool, error) {
	return eventHookSnapshot(ctx, s.db)
}
func eventHookSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_EVENT_WEBHOOK_OUTBOX(ID TEXT PRIMARY KEY,EVENT_ID TEXT NOT NULL,TYPE TEXT NOT NULL,REFERENCE TEXT NOT NULL,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,ACTOR TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,STATUS INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS GO_EVENT_WEBHOOK_PENDING ON GO_EVENT_WEBHOOK_OUTBOX(STATE,CREATED,ID)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func canonicalSignalData(data any) (json.RawMessage, error) {
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > MaxEventWebhookPayload || !utf8.Valid(raw) {
		return nil, ErrEventWebhook
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var object map[string]any
	if d.Decode(&object) != nil || object == nil || len(object) > 10000 {
		return nil, ErrEventWebhook
	}
	raw, err = json.Marshal(object)
	if err != nil {
		return nil, ErrEventWebhook
	}
	return raw, nil
}

// PublishSignal participates in the caller's actual domain transaction. It
// never creates a sender, scans old facts, or invokes an external receiver.
// A missing/closed grant leaves the domain operation independent of this plugin.
func PublishSignal(ctx context.Context, tx *sql.Tx, kind, key, reference string, data any, source ...EventWebhookSource) error {
	if !ValidSignalType(kind) || key == "" || len(key) > 4096 || len(reference) > 4096 {
		return ErrEventWebhook
	}
	if len(source) > 1 || len(source) == 1 && (!source[0].Valid() || kind != "subtitle.download" || source[0].Reference != reference) {
		return ErrEventWebhook
	}
	c, spec, active, err := eventHookSnapshot(ctx, tx)
	if errors.Is(err, ErrEventWebhook) {
		return nil
	}
	if err != nil {
		return err
	}
	if !active {
		return nil
	}
	raw, err := canonicalSignalData(data)
	if err != nil {
		return err
	}
	eventID := Hash([]string{kind, key})
	d := EventWebhookDefinition{Version: 1, TargetDigest: Hash(spec), EventID: eventID, Reference: reference, Control: c, Envelope: EventWebhookEnvelope{Type: kind, Data: raw}}
	if len(source) == 1 {
		original := source[0]
		d.Source = &original
	}
	d.ID = Hash([]string{c.Epoch, eventID})
	document, err := json.Marshal(d)
	if err != nil || len(document) > MaxEventWebhookPayload+64<<10 {
		return ErrEventWebhook
	}
	if err := eventHookSchema(ctx, tx); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_EVENT_WEBHOOK_OUTBOX(ID,EVENT_ID,TYPE,REFERENCE,DOCUMENT,FINGERPRINT,STATE,REASON,ACTOR,CREATED,UPDATED,STATUS) VALUES(?,?,?,?,?,?,'pending','','',?,?,0) ON CONFLICT(ID) DO NOTHING`, d.ID, d.EventID, kind, reference, string(document), Hash(d), now, now)
	if err != nil {
		return err
	}
	if _, err := res.RowsAffected(); err != nil {
		return err
	}
	p, err := eventHookRead(ctx, tx, d.ID)
	if err != nil || p.Fingerprint != Hash(d) || Hash(p.Definition) != Hash(d) {
		return ErrEventWebhook
	}
	current, currentSpec, currentActive, err := eventHookSnapshot(ctx, tx)
	if err != nil || !currentActive || current != c || Hash(currentSpec) != d.TargetDigest {
		return ErrEventWebhook
	}
	return nil
}

const eventHookSelect = `SELECT ID,EVENT_ID,TYPE,REFERENCE,DOCUMENT,FINGERPRINT,STATE,REASON,ACTOR,CREATED,UPDATED,STATUS FROM GO_EVENT_WEBHOOK_OUTBOX`

func scanEventHook(row interface{ Scan(...any) error }) (EventWebhookReceipt, error) {
	p := EventWebhookReceipt{}
	var raw string
	err := row.Scan(&p.ID, &p.EventID, &p.Type, &p.Reference, &raw, &p.Fingerprint, &p.State, &p.Reason, &p.Actor, &p.Created, &p.Updated, &p.Status)
	if err != nil {
		return p, err
	}
	if len(raw) > MaxEventWebhookPayload+64<<10 || json.Unmarshal([]byte(raw), &p.Definition) != nil || p.Definition.Version != 1 || !eventHookHex(p.Definition.TargetDigest, 64) || !eventHookHex(p.ID, 64) || p.Definition.ID != p.ID || p.Definition.EventID != p.EventID || p.Definition.Reference != p.Reference || p.Definition.Envelope.Type != p.Type || !ValidSignalType(p.Type) || Hash(p.Definition) != p.Fingerprint {
		return p, ErrEventWebhook
	}
	craw, _ := json.Marshal(p.Definition.Control)
	if _, err := DecodeEventWebhookControl(string(craw)); err != nil {
		return p, err
	}
	if !p.Definition.Control.Authorized || !eventHookHex(p.EventID, 64) || p.ID != Hash([]string{p.Definition.Control.Epoch, p.EventID}) || p.Status < 0 || p.Status > 599 {
		return p, ErrEventWebhook
	}
	if source := p.Definition.Source; source != nil && (!source.Valid() || p.Type != "subtitle.download" || source.Reference != p.Reference) {
		return p, ErrEventWebhook
	}
	if _, err := time.Parse(time.RFC3339Nano, p.Created); err != nil {
		return p, ErrEventWebhook
	}
	if _, err := time.Parse(time.RFC3339Nano, p.Updated); err != nil {
		return p, ErrEventWebhook
	}
	switch p.State {
	case "pending", "intent", "sent", "uncertain", "blocked", "dismissed":
	default:
		return p, ErrEventWebhook
	}
	if p.State == "sent" && (p.Status < 200 || p.Status > 299) || p.State == "dismissed" && (p.Actor == "" || strings.TrimSpace(p.Reason) == "") {
		return p, ErrEventWebhook
	}
	return p, nil
}
func eventHookRead(ctx context.Context, r eventHookReader, id string) (EventWebhookReceipt, error) {
	yes, err := eventHookTable(ctx, r, "GO_EVENT_WEBHOOK_OUTBOX")
	if err != nil {
		return EventWebhookReceipt{}, err
	}
	if !yes {
		return EventWebhookReceipt{}, ErrEventWebhookMissing
	}
	p, err := scanEventHook(r.QueryRowContext(ctx, eventHookSelect+` WHERE ID=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrEventWebhookMissing
	}
	return p, err
}
func (s *Store) EventWebhook(ctx context.Context, id string) (EventWebhookReceipt, error) {
	if !eventHookHex(id, 64) {
		return EventWebhookReceipt{}, ErrEventWebhookMissing
	}
	return eventHookRead(ctx, s.db, id)
}
func (s *Store) EventWebhooks(ctx context.Context, page int, pending bool) ([]EventWebhookReceipt, error) {
	out := []EventWebhookReceipt{}
	if page < 1 || page > 10000 {
		return nil, ErrEventWebhook
	}
	yes, err := eventHookTable(ctx, s.db, "GO_EVENT_WEBHOOK_OUTBOX")
	if err != nil || !yes {
		return out, err
	}
	query, args := eventHookSelect+` ORDER BY CREATED DESC,ID DESC LIMIT 30 OFFSET ?`, []any{(page - 1) * 30}
	if pending {
		query, args = eventHookSelect+` WHERE STATE='pending' ORDER BY CREATED,ID LIMIT 20`, nil
		if waits, e := eventHookTable(ctx, s.db, "GO_EVENT_WEBHOOK_WAITS"); e != nil {
			return nil, e
		} else if waits {
			query = eventHookSelect + ` WHERE STATE='pending' AND NOT EXISTS(SELECT 1 FROM GO_EVENT_WEBHOOK_WAITS w WHERE w.ID=GO_EVENT_WEBHOOK_OUTBOX.ID AND w.NEXT_CHECK>?) ORDER BY CREATED,ID LIMIT 20`
			args = []any{time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")}
		}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanEventHook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) ClaimEventWebhook(ctx context.Context, expected EventWebhookReceipt, authority func(EventWebhookControl) error, source ...func(*sql.Tx, EventWebhookSource) error) (EventWebhookReceipt, EventWebhookSpec, error) {
	if len(source) > 1 {
		return expected, EventWebhookSpec{}, ErrEventWebhook
	}
	var spec EventWebhookSpec
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return expected, spec, err
	}
	defer tx.Rollback()
	p, err := eventHookRead(ctx, tx, expected.ID)
	if err != nil || p.State != "pending" || p.Fingerprint != expected.Fingerprint {
		return p, spec, ErrEventWebhook
	}
	c, current, active, scopeErr := eventHookSnapshot(ctx, tx)
	state, reason := "intent", ""
	if scopeErr != nil || !active || c != p.Definition.Control || Hash(current) != p.Definition.TargetDigest || authority == nil || authority(c) != nil {
		state, reason = "blocked", "original plugin, target configuration or administrator authority changed"
	} else {
		spec = current
	}
	if state == "intent" && p.Definition.Source != nil {
		if len(source) != 1 || source[0] == nil {
			state, reason = "blocked", "original source verifier unavailable; no event sent"
		} else if e := source[0](tx, *p.Definition.Source); errors.Is(e, ErrEventWaiting) {
			return p, spec, e
		} else if e != nil {
			state, reason = "blocked", "original media or metadata prerequisite changed; no event sent"
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_EVENT_WEBHOOK_OUTBOX SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='pending' AND FINGERPRINT=?`, state, reason, time.Now().UTC().Format(time.RFC3339Nano), p.ID, p.Fingerprint)
	if err != nil {
		return p, spec, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return p, spec, ErrEventWebhook
	}
	saved, err := eventHookRead(ctx, tx, p.ID)
	if err != nil || saved.State != state || saved.Fingerprint != p.Fingerprint {
		return p, spec, ErrEventWebhook
	}
	if state == "intent" {
		final, finalSpec, finalActive, err := eventHookSnapshot(ctx, tx)
		if err != nil || !finalActive || final != c || Hash(finalSpec) != p.Definition.TargetDigest || authority(c) != nil {
			return p, spec, ErrEventWebhook
		}
		if p.Definition.Source != nil && source[0](tx, *p.Definition.Source) != nil {
			return p, spec, ErrEventWebhook
		}
	}
	if err := tx.Commit(); err != nil {
		return p, spec, err
	}
	if state == "blocked" {
		return saved, spec, ErrEventWebhook
	}
	return saved, spec, nil
}
func (s *Store) FinishEventWebhook(ctx context.Context, p EventWebhookReceipt, state string, status int) error {
	if p.State != "intent" || state != "sent" && state != "uncertain" || state == "sent" && (status < 200 || status > 299) {
		return ErrEventWebhook
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE GO_EVENT_WEBHOOK_OUTBOX SET STATE=?,STATUS=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='intent' AND FINGERPRINT=?`, state, status, map[bool]string{true: "", false: "receiver response not confirmed; do not retry"}[state == "sent"], time.Now().UTC().Format(time.RFC3339Nano), p.ID, p.Fingerprint)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrEventWebhook
	}
	saved, err := eventHookRead(ctx, tx, p.ID)
	if err != nil || saved.State != state || saved.Status != status || saved.Fingerprint != p.Fingerprint {
		return ErrEventWebhook
	}
	return tx.Commit()
}
func (s *Store) DismissEventWebhook(ctx context.Context, id, actor, reason string) error {
	if !eventHookHex(id, 64) || actor == "" || strings.TrimSpace(reason) == "" || len(reason) > 1000 || strings.ContainsRune(reason, '\x00') {
		return ErrEventWebhook
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := eventHookRead(ctx, tx, id)
	if err != nil {
		return err
	}
	if p.State == "sent" || p.State == "dismissed" {
		return ErrEventWebhook
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_EVENT_WEBHOOK_OUTBOX SET STATE='dismissed',ACTOR=?,REASON=?,UPDATED=? WHERE ID=? AND STATE=? AND FINGERPRINT=?`, actor, reason, time.Now().UTC().Format(time.RFC3339Nano), id, p.State, p.Fingerprint)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrEventWebhook
	}
	saved, err := eventHookRead(ctx, tx, id)
	if err != nil || saved.State != "dismissed" || saved.Actor != actor || saved.Reason != reason || saved.Fingerprint != p.Fingerprint || saved.Status != p.Status {
		return ErrEventWebhook
	}
	return tx.Commit()
}

// A wire operation uses a new connection, disables redirects and never loops
// over retries or fallbacks. Receiver-defined idempotency can use this stable ID.
func SendEventWebhook(ctx context.Context, transport http.RoundTripper, spec EventWebhookSpec, p EventWebhookReceipt) (int, bool) {
	if p.State != "intent" || Hash(p.Definition) != p.Fingerprint || Hash(spec) != p.Definition.TargetDigest {
		return 0, false
	}
	raw, err := json.Marshal(p.Definition.Envelope)
	if err != nil {
		return 0, false
	}
	address := spec.URL
	method, body := "POST", io.Reader(bytes.NewReader(raw))
	if spec.Method == "get" {
		method, body = "GET", nil
		query := "type=" + url.QueryEscape(p.Type)
		if spec.GetEncoding == "json" {
			query += "&data=" + url.QueryEscape(string(p.Definition.Envelope.Data))
		} else {
			var object map[string]json.RawMessage
			if json.Unmarshal(p.Definition.Envelope.Data, &object) != nil {
				return 0, false
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				query += "&data=" + url.QueryEscape(key)
			}
		}
		if strings.Contains(address, "?") {
			address += "&" + query
		} else {
			address += "?" + query
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return 0, false
	}
	req.Header.Set("X-NASTool-Event-ID", p.EventID)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		// Prevent implicit replay of an HTTP/2 stream on a second connection.
		clone.Protocols = new(http.Protocols)
		clone.Protocols.SetHTTP1(true)
		transport = clone
		defer clone.CloseIdleConnections()
	}
	client := http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return response.StatusCode, response.StatusCode >= 200 && response.StatusCode < 300
}
