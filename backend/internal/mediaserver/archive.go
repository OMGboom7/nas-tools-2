package mediaserver

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

type archiveTitle struct {
	name       string
	year       int
	tmdb, imdb string
}

// WriteArchive writes one level of titles per library. Episode inventories and
// physical paths are deliberately outside the Markdown format.
func (client *Client) WriteArchive(ctx context.Context, username string, w io.Writer) (int, error) {
	libraries, err := client.SyncLibraries(ctx, username)
	if err != nil || len(libraries) == 0 {
		return 0, err
	}
	var itemsURL url.URL
	if client.kind != "plex" {
		base, user, err := client.syncUser(ctx, username)
		if err != nil {
			return 0, err
		}
		itemsURL = base
		itemsURL.Path += "/Users/" + user + "/Items"
	}
	for _, lib := range libraries {
		var titles []archiveTitle
		if client.kind == "plex" {
			items, err := client.syncPlexPages(ctx, "/library/sections/"+lib.ID+"/all", url.Values{"includeGuids": {"1"}}, nil)
			if err != nil {
				return 0, err
			}
			for _, item := range items {
				if item.Type != lib.Type {
					return 0, ErrResponse
				}
				title := archiveTitle{name: item.Title, year: item.Year}
				for _, guid := range item.Guids {
					if id, ok := strings.CutPrefix(guid.ID, "tmdb://"); ok {
						title.tmdb = id
					}
					if id, ok := strings.CutPrefix(guid.ID, "imdb://"); ok {
						title.imdb = id
					}
				}
				titles = append(titles, title)
			}
		} else {
			items, err := client.syncJSONPages(ctx, itemsURL, url.Values{"ParentId": {lib.ID}, "Recursive": {"true"}, "IncludeItemTypes": {"Movie,Series"}, "Fields": {"ProviderIds,ProductionYear"}}, nil)
			if err != nil {
				return 0, err
			}
			for _, item := range items {
				if item.Type != "Movie" && item.Type != "Series" {
					return 0, ErrResponse
				}
				titles = append(titles, archiveTitle{name: item.Name, year: item.Year, tmdb: item.Providers["Tmdb"], imdb: item.Providers["Imdb"]})
			}
		}
		sort.SliceStable(titles, func(i, j int) bool {
			a, b := titles[i], titles[j]
			if a.year == b.year {
				return a.name < b.name
			}
			return a.year != 0 && (b.year == 0 || a.year < b.year)
		})
		if _, err := fmt.Fprintln(w, "- "+lib.Name); err != nil {
			return 0, err
		}
		for _, title := range titles {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if title.name == "" {
				continue
			}
			line := "  - " + title.name
			if title.year != 0 {
				line += fmt.Sprintf(" (%d)", title.year)
			}
			if title.tmdb != "" {
				line += " [TMDB:" + title.tmdb + "]"
			}
			if title.imdb != "" {
				line += " [IMDB:" + title.imdb + "]"
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return 0, err
			}
		}
	}
	return len(libraries), ctx.Err()
}
