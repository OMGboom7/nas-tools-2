package httpserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/mediaarchive"
)

func (service pluginService) nativeMetadataInstalled(ctx context.Context) ([]string, error) {
	raw, err := service.system.Get(ctx, "UserInstalledPlugins")
	if err != nil {
		return nil, err
	}
	return decodeInstalledPlugins(raw)
}

func (service pluginService) nativeMetadataReadConfig(ctx context.Context, id string) (map[string]any, bool, error) {
	raw, err := service.system.Get(ctx, "plugin."+id)
	if err != nil {
		return nil, false, err
	}
	values, err := decodeMetadataConfig(raw)
	if err != nil {
		return nil, false, err
	}
	if service.nativeHosts(id) {
		input, err := nativeHostsInput(values["hosts"])
		if err != nil {
			return nil, false, err
		}
		service.hosts.mu.Lock()
		active := service.hosts.active
		service.hosts.mu.Unlock()
		return map[string]any{"hosts": input, "enable": values["enable"] == true, "err_hosts": values["err_hosts"]}, active, nil
	}
	if err := validateMetadataConfig(ctx, id, values); err != nil {
		return nil, false, err
	}
	key := "release_groups"
	if id == "Customization" {
		key = "customization"
	}
	// Only declared metadata fields belong in the legacy editor response.
	// Unknown persisted state can contain private caches and must not leak.
	config := map[string]any{}
	for _, field := range []string{key, "separator"} {
		if value, found := values[field]; found {
			config[field] = value
		}
	}
	active := text(values[key]) != ""
	if id == "CustomReleaseGroups" {
		active = active || text(values["separator"]) != ""
	}
	return config, active, nil
}

func (service pluginService) serveNativeMetadataStatus(response http.ResponseWriter, request *http.Request, id string) {
	installed, err := service.nativeMetadataInstalled(request.Context())
	if err != nil {
		writeAPIError(response, 502, 502, "installed plugin state is unavailable")
		return
	}
	for _, current := range installed {
		if current != id {
			continue
		}
		_, active, err := service.nativeMetadataReadConfig(request.Context(), id)
		if err != nil {
			writeAPIError(response, 502, 502, "plugin state is unavailable")
			return
		}
		writeJSON(response, 200, map[string]any{"code": 0, "state": active})
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "state": nil})
}

func (service pluginService) metadataLegacyListRoute() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		administrator, ok := service.catalogReader(response, request)
		if !ok {
			return
		}
		installed, err := service.nativeMetadataInstalled(request.Context())
		if err != nil {
			writeAPIError(response, 502, 502, "installed plugin state is unavailable")
			return
		}
		catalog, err := bundledPluginCatalog()
		if err != nil {
			writeAPIError(response, 500, 500, "bundled plugin catalog is invalid")
			return
		}
		result := map[string]any{}
		var nativeIDs []string
		seen := map[string]bool{}
		var needsLegacy, privateConfig bool
		for _, id := range installed {
			if seen[id] {
				continue
			}
			seen[id] = true
			privateConfig = privateConfig || service.nativeTorrentTransfer(id) || service.nativeTorrentRemover(id) || service.nativeTorrentMark(id) || service.nativeCookieCloud(id) || service.nativeAutoBackup(id) || service.nativeSyncTimer(id) || service.nativeOpenSubtitles(id)
			entry, bundled := catalog[id]
			if !administrator && (!bundled || entry.AuthLevel > 1) {
				continue
			}
			if service.ownsPlugin(id) {
				nativeIDs = append(nativeIDs, id)
				continue
			}
			needsLegacy = true
			name := entry.Name
			if !bundled {
				name = id
			}
			result[id] = map[string]any{"name": name, "version": entry.Version, "installed": true, "native": false, "state": nil, "stateKnown": false, "actionsAvailable": false}
		}
		if needsLegacy && service.legacyURL != "" {
			if privateConfig {
				writeAPIError(response, 501, 501, "混合插件配置请使用各自的配置页")
				return
			}
			request.Body = http.MaxBytesReader(response, request.Body, 128<<10)
			if err := request.ParseForm(); err != nil {
				writeAPIError(response, 400, 400, "invalid plugin list request")
				return
			}
			ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
			defer cancel()
			legacy, err := service.legacyInstalledPlugins(ctx, request.Header.Get("Authorization"), request.PostForm)
			if !handlePluginResult(response, legacy, err, "plugin list is unavailable") {
				return
			}
			rows, ok := legacy["result"].(map[string]any)
			if !ok {
				writeAPIError(response, 502, 502, "invalid installed plugin list")
				return
			}
			// Only entries selected from the local installation and visible
			// to this user can inherit their old editor configuration.
			for id := range result {
				if row, ok := rows[id].(map[string]any); ok {
					result[id] = row
				}
			}
		}
		for _, id := range nativeIDs {
			if service.nativeTorrentTransfer(id) {
				actor, ok := service.torrentTransfer.login.actor(response, request, false)
				if !ok {
					return
				}
				st, err := service.transferConfig(request.Context(), actor)
				if err != nil {
					writeAPIError(response, 409, 409, "无法读取转种配置")
					return
				}
				running, known := service.torrentTransfer.scheduleSummary(request.Context())
				result[id] = map[string]any{"name": st.detail.Name, "version": "1.0", "config": st.detail.Values, "fields": indexerLegacyFields(st.detail), "state": running, "stateKnown": known, "native": true, "fingerprint": st.detail.Meta.Fingerprint}
				continue
			}
			if service.nativeTorrentRemover(id) {
				api := service.torrentRemover
				actor, ok := api.actor(response, request, false)
				if !ok {
					return
				}
				st, err := api.store.Settings(request.Context())
				if err != nil {
					removalError(response)
					return
				}
				authority, _, err := api.login.authority(actor)
				if err != nil {
					removalError(response)
					return
				}
				detail, err := api.detail(actor, st, authority)
				if err != nil {
					removalError(response)
					return
				}
				running, known := api.summary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeTorrentMark(id) {

				api := service.torrentMark
				actor, ok := api.actor(response, request, false)
				if !ok {
					return
				}
				detail, _, _, _, err := api.detail(request.Context(), actor)
				if err != nil {
					markError(response, err)
					return
				}
				running, known := api.summary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeSyncTimer(id) {

				api := service.syncTimer
				if !api.authorize(response, request) {
					return
				}
				claims, err := api.auth.service.VerifyToken(request.Header.Get("Authorization"))
				if err != nil {
					writeAPIError(response, 401, 401, "请重新登录")
					return
				}
				settings, err := service.syncTimerPluginSettings(request.Context())
				if err != nil {
					writeOrganizationError(response, err)
					return
				}
				detail := syncTimerDetail(claims.Username, settings)
				running, known := service.syncTimerRunning(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeDouban(id) {
				raw, err := service.system.ReadMany(request.Context(), doubanKeys)
				if err != nil {
					subscriptionHistoryError(response, err)
					return
				}
				claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
				if err != nil {
					writeAPIError(response, 401, 401, "请重新登录")
					return
				}
				cfg, err := douban.ReadSettings(raw[doubanSettingsKey])
				if err != nil {
					subscriptionHistoryError(response, err)
					return
				}
				detail := doubanDetail(claims.Username, raw, cfg)
				running, known := service.doubanSync.summary(raw)
				result[id] = map[string]any{"name": detail.Name, "version": catalog[id].Version, "page": id, "state": running, "stateKnown": known, "native": true, "fields": indexerLegacyFields(detail), "config": detail.Values, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeArchiveRunner(id) {
				raw, err := service.system.ReadMany(request.Context(), archiveKeys)
				if err != nil {
					archivePageError(response, err)
					return
				}
				running, known := service.archiveStatus(raw)
				row := map[string]any{"name": "媒体库归档", "version": "1.1", "page": id, "state": running, "stateKnown": known, "native": true}
				if administrator {
					claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
					if err != nil {
						writeAPIError(response, 401, 401, "请重新登录")
						return
					}
					cfg, _, err := mediaarchive.Parse(raw[archiveSettingsKey])
					if err != nil {
						archivePageError(response, err)
						return
					}
					detail := service.archiveSettings(claims.Username, raw, cfg)
					row["fields"], row["config"], row["fingerprint"] = indexerLegacyFields(detail), detail.Values, detail.Meta.Fingerprint
				}
				result[id] = row
				continue
			}
			if service.nativeAutoBackup(id) {
				api := service.autoBackup
				actor, ok := api.actor(response, request, false)
				if !ok {
					return
				}
				state, err := api.store.State(request.Context())
				if err != nil {
					backupError(response, err)
					return
				}
				detail := api.detail(actor, state)
				running, known := api.summary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeCookieCloud(id) {
				actor, ok := service.cookieCloud.actor(response, request, false)
				if !ok {
					return
				}
				scope, err := service.cookieCloud.scope(request.Context(), actor)
				if err != nil {
					cookieCloudError(response, err)
					return
				}
				detail := cookieCloudDetail(scope)
				running, known := service.cookieCloud.summary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.2", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeCloudflare(id) {
				actor, ok := service.cloudflare.actor(response, request, false)
				if !ok {
					return
				}
				detail, err := service.cloudflare.detail(request.Context(), actor)
				if err != nil {
					cfError(response, err)
					return
				}
				c, err := service.cloudflare.store.Control(request.Context())
				if err != nil {
					cfError(response, err)
					return
				}
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "state": service.cloudflare.validate(request.Context(), c) == nil, "stateKnown": true, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeSignin(id) {
				actor, ok := service.siteSignin.actor(response, request, false)
				if !ok {
					return
				}
				detail, err := service.siteSignin.detail(request.Context(), actor)
				if err != nil {
					signinError(response, err)
					return
				}
				running, known := service.signinSummary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeOpenSubtitles(id) {
				actor, ok := service.openSubtitles.actor(response, request, false)
				if !ok {
					return
				}
				raw, e := service.system.ReadMany(request.Context(), openSubtitleKeys)
				if e != nil {
					openSubtitleError(response, e)
					return
				}
				detail := service.openSubtitles.detail(raw, actor)
				running, known := service.openSubtitleSummary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeMovieLike(id) {
				actor, ok := service.movieLike.authorize(response, request)
				if !ok {
					return
				}
				raw, err := service.system.ReadMany(request.Context(), movieLikeKeys)
				if err != nil {
					movieLikeError(response, err)
					return
				}
				detail, err := service.movieLike.detail(raw, actor)
				if err != nil {
					movieLikeError(response, err)
					return
				}
				running, known := service.movieLikeSummary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeMediaSyncDelete(id) {
				actor, ok := service.mediaSyncDelete.authorize(response, request)
				if !ok {
					return
				}
				raw, err := service.system.ReadMany(request.Context(), mediaSyncDeleteKeys)
				if err != nil {
					mediaSyncDeleteError(response, err)
					return
				}
				detail, err := service.mediaSyncDelete.detail(raw, actor)
				if err != nil {
					mediaSyncDeleteError(response, err)
					return
				}
				running, known := service.mediaSyncDeleteSummary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeSpeedLimiter(id) {
				actor, ok := service.speedLimiter.authorize(response, request)
				if !ok {
					return
				}
				raw, err := service.system.ReadMany(request.Context(), speedLimiterKeys)
				if err != nil {
					speedLimiterError(response, err)
					return
				}
				detail, err := service.speedLimiter.detail(request.Context(), raw, actor)
				if err != nil {
					speedLimiterError(response, err)
					return
				}
				running, known := service.speedLimiterSummary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeEventWebhook(id) {
				actor, ok := service.eventWebhook.authorize(response, request)
				if !ok {
					return
				}
				raw, err := service.system.ReadMany(request.Context(), eventWebhookKeys)
				if err != nil {
					eventWebhookError(response, err)
					return
				}
				values, err := decodeMetadataConfig(raw["plugin.Webhook"])
				if err != nil {
					eventWebhookError(response, err)
					return
				}
				detail := service.eventWebhook.detail(raw, values, actor)
				running, known := service.eventWebhookSummary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeMediaRefresh(id) {
				actor, ok := service.mediaRefresh.authorize(response, request)
				if !ok {
					return
				}
				state, err := service.mediaRefresh.read(request.Context())
				if err != nil {
					mediaRefreshError(response, err)
					return
				}
				detail := service.mediaRefresh.pluginDetail(state.raw, state.values, actor)
				running, known := service.mediaRefreshSummary(request.Context())
				result[id] = map[string]any{"name": detail.Name, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": running, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeLibrary(id) {
				if !service.library.authorize(response, request) {
					return
				}
				raw, err := service.system.ReadMany(request.Context(), libraryPluginKeys)
				if err != nil {
					writeOrganizationError(response, err)
					return
				}
				values, err := decodeMetadataConfig(raw[libraryPluginKey])
				if err != nil {
					writeOrganizationError(response, err)
					return
				}
				detail, err := service.library.libraryDetail(request.Context(), values, raw)
				if err != nil {
					writeOrganizationError(response, err)
					return
				}
				running, known := service.librarySummary(request.Context())
				var state any
				if known {
					state = running
				}
				result[id] = map[string]any{"name": "媒体库刮削", "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": state, "stateKnown": known, "native": true, "fingerprint": detail.Meta.Fingerprint}
				continue
			}
			if service.nativeSubtitle(id) {
				raw, err := service.system.Get(request.Context(), "plugin.ChineseSubFinder")
				values, decodeErr := decodeMetadataConfig(raw)
				if err != nil || decodeErr != nil {
					writeAPIError(response, 502, 502, "subtitle configuration unavailable")
					return
				}
				running, known := service.subtitleSummary(request.Context())
				var state any
				if known {
					state = running
				}
				detail := subtitleDetail(values)
				result[id] = map[string]any{"name": id, "version": "1.0", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": state, "stateKnown": known, "native": true}
				continue
			}
			if service.nativeIndexer(id) {
				values, _, _, err := service.indexers.read(request.Context(), id)
				if err != nil {
					writeAPIError(response, 502, 502, "indexer configuration unavailable")
					return
				}
				running, known := service.indexers.summary(request.Context(), id)
				var state any
				if known {
					state = running
				}
				detail := indexerDetail(id, values)
				// Never return host, key, password or unknown private caches through
				// the legacy aggregate list, including for administrators.
				result[id] = map[string]any{"name": id, "version": "1.5", "config": detail.Values, "fields": indexerLegacyFields(detail), "page": id, "state": state, "native": true, "stateKnown": known}
				continue
			}
			config, active, err := service.nativeMetadataReadConfig(request.Context(), id)
			if err != nil {
				writeAPIError(response, 502, 502, "plugin configuration is unavailable")
				return
			}
			if service.nativeHosts(id) {
				if service.auth == nil {
					writeAPIError(response, 503, 503, "native authentication is unavailable")
					return
				}
				claims, authErr := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
				if authErr != nil {
					writeAPIError(response, 401, 401, "authorization token is invalid or expired")
					return
				}
				if !service.auth.service.IsAdministrator(claims.Username) {
					continue
				}
				fields := []any{map[string]any{"type": "div", "content": []any{
					[]any{map[string]any{"title": "Hosts 映射", "type": "textarea", "content": map[string]any{"id": "hosts", "rows": 5}}},
					[]any{map[string]any{"title": "无效映射", "type": "textarea", "readonly": true, "content": map[string]any{"id": "err_hosts", "rows": 3}}},
					[]any{map[string]any{"title": "开启 Hosts 同步", "type": "switch", "id": "enable"}},
				}}}
				result[id] = map[string]any{"name": "自定义Hosts", "desc": "修改系统hosts文件，加速网络访问。", "version": "1.0", "icon": "hosts.png", "prefix": "customhosts_", "color": "#02C4E0", "fields": fields, "config": config, "state": active}
				continue
			}
			detail := metadataPluginFields(id, config)
			key, description, color, icon := "release_groups", "添加无法识别的制作组/字幕组，自定义多个组间分隔符", "#00ADEF", "teamwork.png"
			if id == "Customization" {
				key, description, color, icon = "customization", "添加自定义占位符识别正则，自定义多个结果间分隔符", "#E64D1C", "regex.png"
			}
			fields := []any{map[string]any{"type": "div", "content": []any{
				[]any{map[string]any{"title": detail.Fields[0].Title, "type": "textarea", "content": map[string]any{"id": key, "rows": 5, "placeholder": detail.Fields[0].Placeholder}}},
				[]any{map[string]any{"title": "自定义分隔符", "type": "text", "content": []any{map[string]any{"id": "separator", "placeholder": "留空使用 @"}}}},
			}}}
			result[id] = map[string]any{"name": detail.Name, "desc": description, "version": "1.0", "icon": icon, "prefix": strings.ToLower(id) + "_", "color": color, "fields": fields, "config": config, "state": active}
		}
		writeJSON(response, 200, map[string]any{"code": 0, "result": result})
	})
}
