package httpserver

import (
	"context"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func libraryDefinitionDriver(d organization.LibraryDefinition) string {
	if d.Remote != nil {
		return d.Remote.Driver
	}
	return ""
}
func libraryStorageSelection(value string) (string, string, error) {
	if driver, id, ok := strings.Cut(value, ":"); ok {
		if organization.LibraryRemoteMode(driver) == "" || id == "" {
			return "", "", organization.ErrPath
		}
		return id, driver, nil
	}
	return value, "", nil
}

func (api *libraryScraper) remoteFactory(driver string) remotestore.Factory {
	return api.intake.organizer.remoteFactory(organization.LibraryRemoteMode(driver))
}
func (api *libraryScraper) remoteDefinition(ctx context.Context, root organizationRoot, driver string) (organization.LibraryRoot, *remotestore.Binding, error) {
	if organization.LibraryRemoteMode(driver) == "" {
		return organization.LibraryRoot{}, nil, organization.ErrMode
	}
	logical, err := logicalRemoteRoot(root.Path)
	if err != nil {
		return organization.LibraryRoot{}, nil, err
	}
	c, err := api.remoteFactory(driver).Open(nil)
	if err != nil {
		return organization.LibraryRoot{}, nil, err
	}
	defer c.Close()
	if err := c.Probe(ctx, logical); err != nil {
		return organization.LibraryRoot{}, nil, err
	}
	kind := "tv"
	if root.Type == "MOV" {
		kind = "movie"
	}
	binding := c.Binding()
	return organization.LibraryRoot{ID: root.ID, Path: logical, Kind: kind, Identity: organization.RemoteTargetIdentityFor(organization.LibraryRemoteMode(driver), logical)}, &binding, nil
}

type libraryNamespaceKey struct{}

func (api *libraryScraper) libraryContext(ctx context.Context, d organization.LibraryDefinition) (context.Context, func(), error) {
	if d.Remote == nil {
		return ctx, func() {}, nil
	}
	closeGuard := func() {}
	if held, _ := ctx.Value(libraryNamespaceKey{}).(bool); !held {
		guard, err := api.intake.organizer.jobs.AcquireJob(ctx, "rclone-namespace:NASTOOL")
		if err != nil {
			return ctx, func() {}, err
		}
		closeGuard = func() { guard.Close() }
		ctx = context.WithValue(ctx, libraryNamespaceKey{}, true)
	}
	c, err := api.remoteFactory(d.Remote.Driver).Open(d.Remote)
	if err != nil {
		closeGuard()
		return ctx, func() {}, err
	}
	return organization.LibraryRemoteContext(ctx, c, d), func() { _ = c.Close(); closeGuard() }, nil
}

func (api *libraryScraper) libraryJobContext(ctx context.Context, job organization.LibraryJob) (context.Context, func(), error) {
	if job.Definition.Remote == nil {
		return ctx, func() {}, nil
	}
	call, cancel := context.WithCancel(ctx)
	if api.current(call, job) != nil {
		cancel()
		return ctx, func() {}, organization.ErrState
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-call.Done():
				return
			case <-ticker.C:
				if api.current(call, job) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return call, func() { cancel(); <-done }, nil
}

func approveLibraryRemote(d *organization.LibraryDefinition, actor, origin string, exclusive, recovery bool) error {
	if d.Remote == nil {
		return nil
	}
	if !exclusive || !recovery || actor == "" || actor != d.Actor {
		return organization.ErrState
	}
	d.RemoteApproval = &organization.RemoteApproval{Actor: actor, Origin: origin, Created: time.Now().UTC().Format(time.RFC3339Nano), Exclusive: true, RecoveryCopy: true}
	if !organization.LibraryRemoteApproved(*d) {
		return organization.ErrState
	}
	return nil
}
