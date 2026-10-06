package indexercatalog

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type Definition struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Domain   string          `json:"domain"`
	Public   bool            `json:"public"`
	Encoding string          `json:"encoding"`
	Parser   string          `json:"parser"`
	Search   json.RawMessage `json:"search"`
	Category json.RawMessage `json:"category"`
	Torrents json.RawMessage `json:"torrents"`
	Browse   json.RawMessage `json:"browse"`
}

type Catalog struct {
	Indexers []Definition                          `json:"indexer"`
	Conf     map[string]map[string]json.RawMessage `json:"conf"`
}

const maxCatalogBytes = 16 << 20

func Load(path string) (Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return Catalog{}, err
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, maxCatalogBytes+1))
	if err != nil {
		return Catalog{}, err
	}
	if len(encoded) > maxCatalogBytes {
		return Catalog{}, errors.New("site catalog is too large")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return Catalog{}, errors.New("invalid site catalog encoding")
	}
	var catalog Catalog
	if json.Unmarshal(decoded, &catalog) != nil || catalog.Indexers == nil {
		return Catalog{}, errors.New("invalid site catalog document")
	}
	for _, item := range catalog.Indexers {
		if item.ID == "" || item.Name == "" || Domain(item.Domain) == "" {
			return Catalog{}, errors.New("invalid site catalog definition")
		}
	}
	return catalog, nil
}

func Domain(raw string) string {
	parsed, err := url.Parse(raw)
	// This is an identity comparison, not a request URL validator. The legacy
	// catalog contains a misspelled scheme; keep its domain/ID selectable without
	// repairing or executing that URL. HTTP clients must validate schemes separately.
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return ""
	}
	return strings.ReplaceAll(strings.ToLower(parsed.Host), "www.", "")
}

// HasTrackerRule reports whether the bundled site definition contains at
// least one rule for an attribute shown by the old site editor.
func (catalog Catalog) HasTrackerRule(raw, attribute string) bool {
	domain := Domain(raw)
	if domain == "" {
		return false
	}
	for host, rules := range catalog.Conf {
		candidate := host
		if !strings.Contains(candidate, "://") {
			candidate = "https://" + candidate
		}
		if Domain(candidate) != domain {
			continue
		}
		var expressions []json.RawMessage
		return json.Unmarshal(rules[attribute], &expressions) == nil && len(expressions) != 0
	}
	return false
}

// Selected preserves catalog IDs used by existing subscriptions. It deliberately
// handles only builtin sites; callers must supply plugin indexers separately.
func (catalog Catalog) Selected(sites []siteconfig.Site, selected []string, public bool) []Definition {
	allowed := map[string]bool{}
	for _, id := range selected {
		allowed[id] = true
	}
	byDomain := map[string]Definition{}
	for _, item := range catalog.Indexers {
		byDomain[Domain(item.Domain)] = item
	}
	result := []Definition{}
	seen := map[string]bool{}
	add := func(item Definition) {
		domain := Domain(item.Domain)
		if domain == "" || seen[domain] || !allowed[item.ID] {
			return
		}
		seen[domain] = true
		item.Domain = domain
		result = append(result, item)
	}
	for _, site := range sites {
		if site.Cookie == "" {
			continue
		}
		raw := site.SignURL
		if raw == "" {
			raw = site.RSSURL
		}
		if item, exists := byDomain[Domain(raw)]; exists {
			item.Name = site.Name
			add(item)
		}
	}
	if public {
		for _, item := range catalog.Indexers {
			if item.Public {
				add(byDomain[Domain(item.Domain)])
			}
		}
	}
	return result
}
