package httpserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

type mediaRefreshConnection struct {
	kind, digest, username, password, serverName string
	cloud                                        bool
	client                                       *mediaserver.Client
}
type mediaRefreshAccountEntry struct {
	client *mediaserver.Client
	err    error
}

func (api *mediaRefreshAPI) spec() (mediaRefreshConnection, error) {
	s := mediaRefreshConnection{}
	snapshot, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		return s, err
	}
	s.kind = strings.ToLower(strings.TrimSpace(text(objectValue(snapshot["media"])["media_server"])))
	if s.kind == "" {
		s.kind = "emby"
	}
	values := objectValue(snapshot[s.kind])
	s.digest = mediaserver.RefreshDigest(map[string]any{"kind": s.kind, "settings": values})
	credential := text(values["api_key"])
	if s.kind == "plex" {
		credential = text(values["token"])
	}
	client, err := mediaserver.New(s.kind, text(values["host"]), credential, api.connection.transport)
	if err == nil {
		s.client = client
		return s, nil
	}
	if s.kind != "plex" {
		return s, err
	}
	if text(values["host"]) != "" && credential != "" {
		return s, err
	}
	s.username, s.password, s.serverName = text(values["username"]), text(values["password"]), text(values["servername"])
	if s.username == "" || s.password == "" || s.serverName == "" {
		return s, err
	}
	s.cloud = true
	return s, nil
}
func (api *mediaRefreshAPI) connect(ctx context.Context, c mediaserver.RefreshControl) (*mediaserver.Client, error) {
	if _, err := api.scope(ctx, c); err != nil {
		return nil, err
	}
	s, err := api.spec()
	if err != nil {
		return nil, err
	}
	if s.kind != c.Server || s.digest != c.ServerDigest {
		return nil, mediaserver.ErrRefreshState
	}
	return api.resolve(ctx, s, c)
}
func (api *mediaRefreshAPI) resolve(ctx context.Context, s mediaRefreshConnection, c mediaserver.RefreshControl) (*mediaserver.Client, error) {
	if !s.cloud {
		return s.client, nil
	}
	if !c.PlexAccount {
		return nil, mediaserver.ErrRefreshState
	}
	api.connectionMu.Lock()
	defer api.connectionMu.Unlock()
	if c.Authorized {
		if _, err := api.scope(ctx, c); err != nil {
			return nil, err
		}
	}
	key := s.digest + ":" + c.Epoch
	if entry, yes := api.accountEntries[key]; yes {
		return entry.client, entry.err
	}
	if len(api.accountEntries) >= 128 {
		return nil, mediaserver.ErrRefreshState
	}
	if api.accountEntries == nil {
		api.accountEntries = map[string]mediaRefreshAccountEntry{}
	}
	client, err := mediaserver.ResolvePlexAccount(ctx, s.username, s.password, s.serverName, c.Epoch, c.PlexHTTP, api.connection.transport)
	api.accountEntries[key] = mediaRefreshAccountEntry{client, err}
	return client, err
}

// Reserve unsupported methods/subactions of the implemented plugin before the
// generic mux. An all-method literal pattern would conflict with its existing
// parameterized GET/PUT patterns in Go's ServeMux.
func (api *mediaRefreshAPI) reservePlugin(next http.Handler) http.Handler {
	const base = "/api/v1/plugins/LibraryRefresh"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == base || strings.HasPrefix(p, base+"/") {
			allowed := p == base && (r.Method == "GET" || r.Method == "PUT" || r.Method == "DELETE") || p == base+"/install" && r.Method == "POST" || p == base+"/page" && r.Method == "GET"
			if !allowed {
				if _, ok := api.authorize(w, r); !ok {
					return
				}
				writeAPIError(w, 405, 405, "unsupported native media refresh plugin method or action")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
