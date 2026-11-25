package builtinindexer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

// TNodeCommand serves source and packaged clients that still schedule searches in Python.
// Site credentials travel on local stdin, and errors omit request data.
func TNodeCommand(ctx context.Context, in io.Reader, out io.Writer) error {
	var q struct {
		ID, Domain, Name, Cookie, UA, Keyword string
		Page, Limit                           int
		Proxy                                 *string
	}
	input := &io.LimitedReader{R: in, N: 256 << 10}
	decoder := json.NewDecoder(input)
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrConfig
	}
	var transport http.RoundTripper
	if q.Proxy != nil {
		proxy, err := siteconfig.ProxyTransport(nil, *q.Proxy)
		if err != nil {
			return ErrConfig
		}
		transport = proxy
	}
	definition := indexercatalog.Definition{ID: q.ID, Name: q.Name, Domain: q.Domain, Parser: "TNodeSpider"}
	rows, err := tnodeRows(ctx, definition, q.Cookie, q.UA, q.Keyword, q.Page, q.Limit, transport)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(rows)
}
