package httpserver

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/antchfx/xmlquery"
	"golang.org/x/net/html"
)

var errSubscriptionRSSUnsupported = errors.New("subscription RSS feature is not migrated")

func subscriptionDetailRules(catalog indexercatalog.Catalog, page string) (map[string]json.RawMessage, error) {
	domain := indexercatalog.Domain(page)
	var rules map[string]json.RawMessage
	for raw, value := range catalog.Conf {
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		if indexercatalog.Domain(raw) == domain {
			if rules != nil {
				return nil, errors.New("ambiguous torrent detail rules")
			}
			rules = value
		}
	}
	if rules == nil {
		return nil, errSubscriptionRSSUnsupported
	}
	if raw := rules["RENDER"]; len(raw) > 0 {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return nil, errors.New("invalid torrent render rule")
		}
		if truthy(value) || text(value) == "Y" {
			return nil, errSubscriptionRSSUnsupported
		}
	}
	for _, key := range []string{"FREE", "2XFREE", "HR"} {
		var paths []string
		if json.Unmarshal(rules[key], &paths) != nil || paths == nil || len(paths) > 32 {
			return nil, errSubscriptionRSSUnsupported
		}
	}
	return rules, nil
}

// Use the existing bounded HTML parser, then adapt its repaired tree to the
// XPath engine already used by RSS configuration. No scripts are executed.
func subscriptionDetailXML(ctx context.Context, source *html.Node) (*xmlquery.Node, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	target := &xmlquery.Node{Data: source.Data}
	switch source.Type {
	case html.DocumentNode:
		target.Type = xmlquery.DocumentNode
	case html.ElementNode:
		target.Type = xmlquery.ElementNode
		for _, attr := range source.Attr {
			target.Attr = append(target.Attr, xmlquery.Attr{Name: xml.Name{Local: attr.Key}, Value: attr.Val})
		}
	case html.TextNode:
		target.Type = xmlquery.TextNode
	case html.CommentNode:
		target.Type = xmlquery.CommentNode
	default:
		target.Type = xmlquery.CommentNode
	}
	for child := source.FirstChild; child != nil; child = child.NextSibling {
		converted, err := subscriptionDetailXML(ctx, child)
		if err != nil {
			return nil, err
		}
		xmlquery.AddChild(target, converted)
	}
	return target, nil
}

func parseSubscriptionDetail(ctx context.Context, body []byte, title string, rules map[string]json.RawMessage, resource externalindexer.Resource) (externalindexer.Resource, error) {
	doc, err := builtinindexer.ParseDocument(ctx, body)
	if err != nil {
		return resource, err
	}
	for _, selector := range []string{`input[type="password"]`, `#challenge-form`, `.cf-turnstile`} {
		nodes, err := builtinindexer.Select(ctx, doc.Root, selector)
		if err != nil || len(nodes) > 0 {
			return resource, errors.New("torrent detail page is not authenticated")
		}
	}
	root, err := subscriptionDetailXML(ctx, doc.Root)
	if err != nil {
		return resource, err
	}
	headings, err := xmlquery.QueryAll(root, "//h1 | //h2")
	if err != nil {
		return resource, err
	}
	verified := false
	normalize := func(raw string) string { return strings.ToLower(strings.Join(strings.Fields(raw), " ")) }
	for _, heading := range headings {
		if strings.Contains(normalize(heading.InnerText()), normalize(title)) {
			verified = true
			break
		}
	}
	if !verified || strings.TrimSpace(title) == "" {
		return resource, errors.New("torrent detail identity could not be verified")
	}
	match := func(key string) ([]*xmlquery.Node, error) {
		var paths []string
		if raw := rules[key]; len(raw) > 0 {
			if json.Unmarshal(raw, &paths) != nil || len(paths) > 32 {
				return nil, errors.New("invalid detail selectors")
			}
		}
		matched := []*xmlquery.Node{}
		for _, path := range paths {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if len(path) > 4096 || !strings.HasPrefix(path, "/") {
				return nil, errors.New("invalid torrent detail XPath")
			}
			nodes, err := xmlquery.QueryAll(root, path)
			if err != nil {
				return nil, err
			}
			matched = append(matched, nodes...)
			if len(matched) > 1000 {
				return nil, errors.New("torrent detail match limit exceeded")
			}
		}
		return matched, nil
	}
	hr, err := match("HR")
	if err != nil {
		return resource, err
	}
	if len(hr) > 0 {
		return resource, errSubscriptionRSSUnsupported
	}
	free, err := match("FREE")
	if err != nil {
		return resource, err
	}
	double, err := match("2XFREE")
	if err != nil {
		return resource, err
	}
	upload, download := 1.0, 1.0
	if len(free) > 0 || len(double) > 0 {
		download = 0
	}
	if len(double) > 0 {
		upload = 2
	}
	resource.UploadFactor, resource.DownloadFactor = &upload, &download
	isFree := download == 0
	resource.Freeleech = &isFree
	peers, err := match("PEER_COUNT")
	if err != nil {
		return resource, err
	}
	if len(peers) > 0 {
		raw := strings.ReplaceAll(strings.TrimSpace(peers[0].InnerText()), ",", "")
		end := 0
		for end < len(raw) && raw[end] >= '0' && raw[end] <= '9' {
			end++
		}
		if end == 0 {
			return resource, errors.New("invalid torrent detail seeder count")
		}
		n, err := strconv.ParseInt(raw[:end], 10, 64)
		if err != nil {
			return resource, err
		}
		resource.Seeders = &n
	}
	return resource, nil
}

func subscriptionSiteOriginAllowed(site siteconfig.Site, raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.Hostname() == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	for _, source := range []string{site.SignURL, site.RSSURL} {
		base, err := url.Parse(source)
		if err == nil && base.User == nil && base.Scheme == parsed.Scheme && strings.EqualFold(base.Host, parsed.Host) {
			return true
		}
	}
	return false
}
