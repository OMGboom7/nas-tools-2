package torrenttransfer

import (
	"context"
	"encoding/json"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

// ReceiverClient resolves the daemon's control ID using its actual protocol.
// A hybrid torrent need not have the same control ID on the two endpoints.
type ReceiverClient interface {
	TargetClient
	TransferWireID(context.Context, torrentmeta.TransferMetadata) (string, error)
	AddReviewedTransfer(context.Context, []byte, string, AddIntent) (AddReceipt, error)
}

// Plan contains the reviewed task and file identities but no transport payload.
// Content is reopened under the configured root before each submission.
type Plan struct {
	GrantDigest   string
	Source        SourceReview
	Receiver      string
	WireID        string
	ContentDigest string
}

func PreparePlan(ctx context.Context, source SourceReview, receiver ReceiverClient, grantDigest string) (Plan, error) {
	if !Hex(grantDigest, 64) || receiver == nil || receiver.Address() == "" || Hash(receiver.Address()) == source.Endpoint || !source.Task.FullComplete(source.Source.Prepared) {
		return Plan{}, ErrState
	}
	id, err := receiver.TransferWireID(ctx, source.Source.Prepared)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{GrantDigest: grantDigest, Source: source, Receiver: Hash(receiver.Address()), WireID: id, ContentDigest: contentDigest(source.Source.Prepared.Content)}
	p.Source.Source.Prepared = torrentmeta.TransferMetadata{}
	return p, p.validate()
}

func (p Plan) validate() error {
	s := p.Source
	dir, err := NormalizeRemote(s.Directory)
	if !Hex(p.GrantDigest, 64) || err != nil || dir != s.Directory || !s.Task.Valid() || !Hex(s.SettingsDigest, 64) || !Hex(s.Endpoint, 64) || !Hex(p.Receiver, 64) || p.Receiver == s.Endpoint || !Hex(p.WireID, 40) || !Hex(p.ContentDigest, 64) || !Hex(s.Source.Torrent.Digest, 64) {
		return ErrState
	}
	return nil
}

func (l *Ledger) SavePlan(ctx context.Context, receipt string, p Plan) error {
	if !Hex(receipt, 64) || p.validate() != nil {
		return ErrState
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = l.db.ExecContext(ctx, `INSERT INTO GO_TORRENT_TRANSFER_PLANS(receipt,plan) VALUES(?,?) ON CONFLICT(receipt) DO NOTHING`, receipt, string(raw))
	if err != nil {
		return err
	}
	original, err := l.LoadPlan(ctx, receipt)
	if err != nil {
		return err
	}
	if Hash(original) != Hash(p) {
		return ErrState
	}
	return nil
}

func (l *Ledger) LoadPlan(ctx context.Context, receipt string) (Plan, error) {
	var p Plan
	if !Hex(receipt, 64) {
		return p, ErrState
	}
	var raw string
	if err := l.db.QueryRowContext(ctx, `SELECT plan FROM GO_TORRENT_TRANSFER_PLANS WHERE receipt=?`, receipt).Scan(&raw); err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, ErrState
	}
	return p, p.validate()
}

// ResolveScope reads the current grant, settings and both configured clients.
// The grant digest changes if the reviewed execution scope changes or expires.
type ResolveScope func(context.Context) (Settings, SourceClient, ReceiverClient, string, error)

func (p Plan) live(ctx context.Context, resolve ResolveScope) (SourceReview, ReceiverClient, error) {
	settings, src, dst, current, err := resolve(ctx)
	if err != nil {
		return SourceReview{}, nil, err
	}
	if current != p.GrantDigest || settings.Ready() != nil || Hash(settings) != p.Source.SettingsDigest || src == nil || dst == nil || Hash(src.Address()) != p.Source.Endpoint || Hash(dst.Address()) != p.Receiver {
		return SourceReview{}, nil, ErrState
	}
	root, err := OpenMetainfoRoot(settings.TorrentPath)
	if err != nil {
		return SourceReview{}, nil, err
	}
	defer root.Close()
	review, ok, err := prepareSource(ctx, settings, src, root, p.Source.Task.WireID)
	if err != nil {
		return SourceReview{}, nil, err
	}
	if !ok || !review.Task.SameInstance(p.Source.Task) || review.Directory != p.Source.Directory || Hash(review.Source) != Hash(p.Source.Source) || contentDigest(review.Source.Prepared.Content) != p.ContentDigest {
		return SourceReview{}, nil, ErrState
	}
	id, err := dst.TransferWireID(ctx, review.Source.Prepared)
	if err != nil {
		return SourceReview{}, nil, err
	}
	if id != p.WireID {
		return SourceReview{}, nil, ErrState
	}
	return review, dst, nil
}

func (l *Ledger) SubmitPlan(ctx context.Context, receipt string, resolve ResolveScope) (AddReceipt, error) {
	if resolve == nil {
		return AddReceipt{}, ErrState
	}
	p, err := l.LoadPlan(ctx, receipt)
	if err != nil {
		return AddReceipt{}, err
	}
	review, dst, err := p.live(ctx, resolve)
	if err != nil {
		return AddReceipt{}, err
	}
	intent := AddIntent{Ledger: l, Receipt: receipt, ScopeProof: Hash(p), Receiver: p.Receiver, WireID: p.WireID, Directory: p.Source.Directory, ContentDigest: p.ContentDigest, Revalidate: func(ctx context.Context) error {
		_, _, err := p.live(ctx, resolve)
		return err
	}}
	return dst.AddReviewedTransfer(ctx, review.Source.Prepared.Content, p.Source.Directory, intent)
}
