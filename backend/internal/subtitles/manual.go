package subtitles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

var ErrMissing = errors.New("subtitle receipt not found")
var ErrConsumer = errors.New("an installed subtitle consumer has not been migrated")

// Snapshot contains no credentials. It binds the original installed list,
// complete configuration text and native plugin generation independently of
// the public display of the configured service.
type Snapshot struct {
	Basis, ConfigDigest, Origin string
	HTTP                        bool
	Payload                     Payload
}
type Reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func SnapshotFor(ctx context.Context, tx *sql.Tx, file, kind string, disc bool) (Snapshot, error) {
	out := Snapshot{}
	c, digest, enabled, err := configured(ctx, tx)
	if err != nil {
		return out, err
	}
	if !enabled {
		return out, ErrConfig
	}
	raw := []string{}
	for _, key := range []string{"UserInstalledPlugins", ConfigKey, EpochKey} {
		v, e := setting(ctx, tx, key)
		if e != nil {
			return out, e
		}
		raw = append(raw, v)
	}
	var ids []string
	if json.Unmarshal([]byte(raw[0]), &ids) != nil {
		return out, ErrConfig
	}
	for _, id := range ids {
		if id == "OpenSubtitles" {
			other, e := setting(ctx, tx, "plugin.OpenSubtitles")
			if e != nil {
				return out, e
			}
			raw = append(raw, other)
			if other != "" {
				var values map[string]json.RawMessage
				if len(other) > 64<<10 || json.Unmarshal([]byte(other), &values) != nil || values == nil {
					return out, ErrConsumer
				}
				if v, ok := values["enable"]; ok {
					var enabled bool
					if json.Unmarshal(v, &enabled) != nil || enabled {
						return out, ErrConsumer
					}
				}
			}
		}
	}
	payload, err := c.Payload(file, kind)
	if disc {
		payload, err = c.BluRayPayload(file, kind)
	}
	if err != nil {
		return out, err
	}
	endpoint, err := endpoint(c.Host)
	if err != nil {
		return out, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return out, ErrConfig
	}
	return Snapshot{Basis: bus.Hash(raw), ConfigDigest: digest, Origin: u.Scheme + "://" + u.Host, HTTP: u.Scheme == "http", Payload: payload}, nil
}
func (s *Store) Snapshot(ctx context.Context, file, kind string, disc bool) (Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	return SnapshotFor(ctx, tx, file, kind, disc)
}
func Read(ctx context.Context, r Reader, id string) (Receipt, error) {
	var exists bool
	if err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SUBTITLE_JOBS' AND type='table')`).Scan(&exists); err != nil {
		return Receipt{}, err
	}
	if !exists {
		return Receipt{}, ErrMissing
	}
	p, err := scan(r.QueryRowContext(ctx, selectSQL+` WHERE ID=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrMissing
	}
	if err != nil {
		return p, err
	}
	if p.ID != bus.Hash([]string{p.JobID, strconv.Itoa(p.Ordinal)}) && p.ID != IntakeID(p.JobID, p.Ordinal) || p.Ordinal < 0 || p.JobID == "" || len(p.JobID) > 128 || len(p.OriginDigest) != 64 || len(p.ConfigDigest) != 64 || len(p.LocalPath) > 4096 || len(p.Name) > 600 || strings.ContainsAny(p.Name, "\x00\r") {
		return p, ErrState
	}
	switch p.State {
	case "pending", "sending", "submitted", "uncertain", "blocked":
	default:
		return p, ErrState
	}
	if p.State == "submitted" && !ValidID(p.ExternalID) {
		return p, ErrState
	}
	if _, e := time.Parse(time.RFC3339Nano, p.Updated); e != nil {
		return p, ErrState
	}
	return p, nil
}
func (s *Store) Get(ctx context.Context, id string) (Receipt, error) { return Read(ctx, s.db, id) }
