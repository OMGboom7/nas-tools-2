package organization

import (
	"context"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func ReadLibraryRemoteMedia(ctx context.Context, d LibraryDefinition, m LibraryMedia, consume func(context.Context, *remotestore.VerifiedObject) ([]byte, error)) ([]byte, error) {
	if !LibraryRemoteApproved(d) || !LibraryRemotePrepared(m) || m.Disc != nil || consume == nil {
		return nil, ErrState
	}
	if err := libraryRemoteSnapshotCurrent(ctx, d, m); err != nil {
		return nil, err
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return nil, err
	}
	o, err := c.Pin(ctx, d.Root.Path, m.Path, m.Node.Size, m.Remote.Digest, func(call context.Context) error { _, err := libraryRemoteClient(call, d); return err })
	if err != nil {
		return nil, err
	}
	defer o.Close()
	data, err := consume(ctx, o)
	if err != nil {
		return nil, err
	}
	if err := verifyLibraryRemoteMedia(ctx, d, m); err != nil {
		return nil, err
	}
	return data, nil
}
