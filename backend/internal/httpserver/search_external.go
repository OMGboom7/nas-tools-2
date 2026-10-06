package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type nativeExternalResourceSearch struct {
	system      *systemconfig.Store
	auth        *nativeAuthentication
	resources   *searchcache.Store
	transport   http.RoundTripper
	recognition *mediaNameAPI
	images      *mediaImageProxy
	sites       *siteconfig.Store
	config      *config.Store
	catalogPath string
}

var errNativeSearchPermission = errors.New("resource search permission is required")

func (service *nativeExternalResourceSearch) search(ctx context.Context, token, keyword string, quick bool) (searchData, bool, error) {
	return service.searchSelected(ctx, token, keyword, quick, nil)
}

func (service *nativeExternalResourceSearch) searchSelected(ctx context.Context, token, keyword string, quick bool, requested []string) (searchData, bool, error) {
	data := searchData{Keyword: keyword, Items: []searchMedia{}}
	claims, err := service.auth.service.VerifyToken(token)
	if err != nil {
		return data, true, err
	}
	user, err := service.auth.service.FindUser(ctx, claims.Username)
	if err != nil {
		return data, true, err
	}
	permitted := false
	for _, permission := range user.Permissions {
		permitted = permitted || permission == "资源搜索"
	}
	if !permitted {
		return data, true, errNativeSearchPermission
	}
	resources, err := service.fetchResources(ctx, keyword, requested)
	if err != nil {
		return data, true, err
	}
	if len(resources) == 0 {
		return data, true, nil
	}
	labels, err := mediameta.ReadLabelOptions(ctx, service.system)
	if err != nil {
		return data, true, err
	}
	items := make([]searchResource, 0, len(resources))
	for _, resource := range resources {
		meta, err := mediameta.ParseWithOptions(ctx, resource.Title, resource.Description, labels)
		if err != nil {
			return data, true, err
		}
		known := resource.DownloadFactor != nil && resource.UploadFactor != nil
		seedersKnown := resource.Seeders != nil
		item := searchResource{Name: resource.Title, Site: resource.Indexer, Description: resource.Description, Size: formatBytes(resource.Size), Resolution: meta.Resolution, Medium: meta.Source, Effect: meta.Effect, ReleaseGroup: meta.Team, VideoCodec: meta.VideoCodec, Labels: []string{}, UploadFactor: 1, DownloadFactor: 1, PromotionKnown: &known, SeedersKnown: &seedersKnown}
		item.MinimumSeedTime, item.MinimumRatio = resource.MinimumSeedTime, resource.MinimumRatio
		// Page URLs may contain private tracker tokens too. The native quick
		// view displays names without outbound links until safe URL policy is ready.
		if resource.Seeders != nil {
			item.Seeders = int(*resource.Seeders)
		}
		if resource.DownloadFactor != nil {
			item.DownloadFactor = *resource.DownloadFactor
		}
		if resource.UploadFactor != nil {
			item.UploadFactor = *resource.UploadFactor
		}
		items = append(items, item)
	}
	groups, err := service.group(ctx, items, resources, quick)
	if err != nil {
		return data, true, err
	}
	ids, err := service.resources.Put(strconv.FormatInt(user.ID, 10)+":"+user.Name, resources)
	if err != nil {
		return data, true, err
	}
	for index := range items {
		items[index].ID = ids[index]
	}
	data.Total = len(items)
	for _, group := range groups {
		if group.inventoryUnknown && len(data.Warnings) == 0 {
			data.Warnings = []string{"部分资源的媒体库状态无法确认，请检查媒体库配置或连接。"}
		}
		for index := range group.Resources {
			group.Resources[index].ID = ids[group.indexes[index]]
		}
		sort.SliceStable(group.Resources, func(i, j int) bool { return group.Resources[i].Seeders > group.Resources[j].Seeders })
		data.Items = append(data.Items, group.searchMedia)
	}
	return data, true, nil
}

type nativeSearchGroup struct {
	searchMedia
	indexes          []int
	inventoryUnknown bool
}

func (service *nativeExternalResourceSearch) group(ctx context.Context, items []searchResource, resources []externalindexer.Resource, quick bool) ([]*nativeSearchGroup, error) {
	groups := []*nativeSearchGroup{}
	byKey := map[string]*nativeSearchGroup{}
	unknown := false
	for index, item := range items {
		media := searchMedia{Key: "native-unidentified", Title: "未识别资源", Type: "未知", ExistsKnown: &unknown, Resources: []searchResource{}}
		if !quick {
			if service.recognition == nil {
				return nil, errors.New("native recognition is unavailable")
			}
			result, failure := service.recognition.recognize(ctx, resources[index].Title, resources[index].Description)
			if failure != nil {
				return nil, errors.New("resource identity lookup failed")
			}
			if result != nil {
				media.Key = result.kind + ":" + strconv.FormatInt(result.detail.ID, 10)
				media.Title, media.Year, media.Type, media.TMDBID = text(result.data["title"]), text(result.data["year"]), text(result.data["type"]), strconv.FormatInt(result.detail.ID, 10)
				media.Overview = text(result.detail.Attributes["overview"])
				if value, ok := result.detail.Attributes["vote_average"]; ok {
					media.Vote = fmt.Sprintf("%.1f", number(value))
				}
				if service.images != nil {
					media.Poster = service.images.rewrite(tmdbImageURL("https://image.tmdb.org", text(result.detail.Attributes["poster_path"])))
				}
				item.Season = text(result.data["season_episode"])
				item.Resolution, item.Medium, item.Effect, item.ReleaseGroup, item.VideoCodec = result.meta.Resolution, result.meta.Source, result.meta.Effect, result.meta.Team, result.meta.VideoCodec
				coverage, inventoryErr := service.recognition.service.nativeMediaExistence(ctx, result.meta, result.detail, result.kind, text(result.data["category"]))
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				known := inventoryErr == nil
				exists := known && coverage.Complete
				media.ExistsKnown, media.Exists = &known, exists
				item.ExistsKnown, item.Exists = &known, &exists
			}
		}
		group := byKey[media.Key]
		if group == nil {
			group = &nativeSearchGroup{searchMedia: media}
			byKey[media.Key] = group
			groups = append(groups, group)
		} else {
			known := group.ExistsKnown != nil && *group.ExistsKnown && media.ExistsKnown != nil && *media.ExistsKnown
			group.ExistsKnown = &known
			group.Exists = known && group.Exists && media.Exists
		}
		group.inventoryUnknown = group.inventoryUnknown || item.ExistsKnown != nil && !*item.ExistsKnown
		group.Resources = append(group.Resources, item)
		group.indexes = append(group.indexes, index)
	}
	return groups, nil
}
