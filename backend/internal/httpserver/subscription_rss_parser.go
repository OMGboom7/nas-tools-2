package httpserver

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"golang.org/x/net/html/charset"
)

type subscriptionRSSItem struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Link        string `xml:"link"`
	Enclosure   struct {
		URL    string `xml:"url,attr"`
		Length string `xml:"length,attr"`
	} `xml:"enclosure"`
	Attributes []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"attr"`
}

func parseSubscriptionRSS(body []byte) ([]externalindexer.Resource, error) {
	if len(body) > 4<<20 {
		return nil, errors.New("RSS feed exceeded size limit")
	}
	// DecodeElement skips nested tokens in the item loop below. Validate the
	// entire document first so unknown item fields cannot bypass depth limits.
	bounded := xml.NewDecoder(bytes.NewReader(body))
	bounded.CharsetReader = charset.NewReaderLabel
	nesting, tokens := 0, 0
	for {
		token, err := bounded.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		tokens++
		if tokens > 100000 {
			return nil, errors.New("RSS token limit exceeded")
		}
		switch token.(type) {
		case xml.Directive:
			return nil, errors.New("RSS directives are unsupported")
		case xml.StartElement:
			nesting++
			if nesting > 64 {
				return nil, errors.New("RSS nesting exceeded limit")
			}
		case xml.EndElement:
			nesting--
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.CharsetReader = charset.NewReaderLabel
	resources := []externalindexer.Resource{}
	depth, channels, items := 0, 0, 0
	rootSeen := false
	inChannel := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch node := token.(type) {
		case xml.Directive:
			return nil, errors.New("RSS directives are unsupported")
		case xml.StartElement:
			depth++
			if depth == 1 {
				if rootSeen || node.Name.Local != "rss" {
					return nil, errors.New("unsupported RSS document")
				}
				rootSeen = true
			}
			if depth == 2 && node.Name.Local == "channel" {
				channels++
				inChannel = true
			}
			if depth == 2 && !inChannel {
				return nil, errors.New("invalid RSS channel structure")
			}
			if depth > 64 {
				return nil, errors.New("RSS nesting exceeded limit")
			}
			if depth == 3 && node.Name.Local == "item" && inChannel {
				items++
				if items > 1000 {
					return nil, errors.New("RSS item limit exceeded")
				}
				var item subscriptionRSSItem
				if err := decoder.DecodeElement(&item, &node); err != nil {
					return nil, err
				}
				depth--
				title := strings.TrimSpace(item.Title)
				if title == "" {
					continue
				}
				if len(title) > 4096 || len(item.Description) > 64<<10 {
					return nil, errors.New("RSS item exceeded size limit")
				}
				enclosure, page := strings.TrimSpace(item.Enclosure.URL), strings.TrimSpace(item.Link)
				if enclosure == "" {
					enclosure, page = page, ""
				}
				if enclosure == "" {
					continue
				}
				if len(enclosure) > 4096 || !rssDownloadURLAllowed(enclosure) || len(page) > 4096 || page != "" && (!rssDownloadURLAllowed(page) || validMagnet(page)) {
					return nil, errors.New("invalid RSS resource locator")
				}
				resource := externalindexer.Resource{Title: title, Description: item.Description, DownloadURL: enclosure, PageURL: page}
				if item.Enclosure.Length != "" {
					n, err := strconv.ParseInt(strings.TrimSpace(item.Enclosure.Length), 10, 64)
					if err != nil || n < 0 {
						return nil, errors.New("invalid RSS resource size")
					}
					resource.Size = n
				}
				seen := map[string]bool{}
				for _, attr := range item.Attributes {
					name := strings.ToLower(attr.Name)
					if name != "seeders" && name != "downloadvolumefactor" && name != "uploadvolumefactor" && name != "minimumseedtime" && name != "minimumratio" {
						continue
					}
					if seen[name] {
						return nil, errors.New("duplicate RSS attribute")
					}
					seen[name] = true
					if name == "seeders" || name == "minimumseedtime" {
						n, err := strconv.ParseInt(attr.Value, 10, 64)
						if err != nil || n < 0 {
							return nil, errors.New("invalid RSS integer attribute")
						}
						if name == "seeders" {
							resource.Seeders = &n
						} else {
							resource.MinimumSeedTime = &n
						}
					} else {
						n, err := strconv.ParseFloat(attr.Value, 64)
						if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
							return nil, errors.New("invalid RSS numeric attribute")
						}
						switch name {
						case "minimumratio":
							resource.MinimumRatio = &n
						case "uploadvolumefactor":
							resource.UploadFactor = &n
						case "downloadvolumefactor":
							resource.DownloadFactor = &n
						}
					}
				}
				resources = append(resources, resource)
			}
		case xml.EndElement:
			if depth == 2 {
				inChannel = false
			}
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(node)) != "" {
				return nil, errors.New("unexpected RSS document text")
			}
		}
	}
	if !rootSeen || channels != 1 || depth != 0 {
		return nil, errors.New("invalid RSS document structure")
	}
	return resources, nil
}
