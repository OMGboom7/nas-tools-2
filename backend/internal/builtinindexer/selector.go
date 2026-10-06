package builtinindexer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

type Document struct{ Root *html.Node }

func ParseDocument(ctx context.Context, body []byte) (Document, error) {
	if len(body) > 4<<20 || !utf8.Valid(body) {
		return Document{}, ErrResponse
	}
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return Document{}, ErrResponse
	}
	type item struct {
		node  *html.Node
		depth int
	}
	stack := []item{{root, 0}}
	count := 0
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return Document{}, err
		}
		last := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		count++
		if count > 50000 || last.depth > 128 {
			return Document{}, ErrResponse
		}
		for child := last.node.FirstChild; child != nil; child = child.NextSibling {
			stack = append(stack, item{child, last.depth + 1})
		}
	}
	return Document{Root: root}, nil
}

// PyQuery accepts quoted selector arguments in :has(). Cascadia takes the
// selector itself. Only unwrap outside attribute values/ordinary CSS strings.
func normalizeHas(raw string) (string, error) {
	var output strings.Builder
	var quote byte
	brackets := 0
	for pos := 0; pos < len(raw); {
		c := raw[pos]
		if quote != 0 {
			output.WriteByte(c)
			pos++
			if c == '\\' && pos < len(raw) {
				output.WriteByte(raw[pos])
				pos++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			output.WriteByte(c)
			pos++
			continue
		}
		if c == '[' {
			brackets++
		}
		if c == ']' {
			brackets--
		}
		if brackets == 0 && strings.HasPrefix(raw[pos:], ":has(") {
			start := pos + 5
			for start < len(raw) && raw[start] == ' ' {
				start++
			}
			if start < len(raw) && (raw[start] == '\'' || raw[start] == '"') {
				q := raw[start]
				end := start + 1
				var argument strings.Builder
				closed := false
				for end < len(raw) {
					if raw[end] == q {
						closed = true
						end++
						break
					}
					if raw[end] == '\\' && end+1 < len(raw) && (raw[end+1] == q || raw[end+1] == '\\') {
						end++
						argument.WriteByte(raw[end])
						end++
						continue
					}
					argument.WriteByte(raw[end])
					end++
				}
				for end < len(raw) && raw[end] == ' ' {
					end++
				}
				if !closed || end >= len(raw) || raw[end] != ')' {
					return "", ErrConfig
				}
				output.WriteString(":has(")
				output.WriteString(argument.String())
				output.WriteByte(')')
				pos = end + 1
				continue
			}
		}
		output.WriteByte(c)
		pos++
	}
	return output.String(), nil
}

// The HTML5 parser inserts tbody nodes omitted by tracker HTML. Preserve
// explicit tbody selectors and additionally match legacy table > tr rules.
func selectorVariants(raw string) ([]string, error) {
	variants := []string{""}
	previous := 0
	var quote byte
	brackets := 0
	for pos := 0; pos < len(raw); pos++ {
		c := raw[pos]
		if quote != 0 {
			if c == '\\' {
				pos++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '[' {
			brackets++
		}
		if c == ']' {
			brackets--
		}
		if c != '>' || brackets != 0 {
			continue
		}
		end := pos + 1
		for end < len(raw) && (raw[end] == ' ' || raw[end] == '\t') {
			end++
		}
		if !strings.HasPrefix(raw[end:], "tr") {
			continue
		}
		end += 2
		if end < len(raw) && ((raw[end] >= 'a' && raw[end] <= 'z') || (raw[end] >= 'A' && raw[end] <= 'Z') || (raw[end] >= '0' && raw[end] <= '9') || raw[end] == '-' || raw[end] == '_' || raw[end] == '\\' || raw[end] >= 128) {
			continue
		}
		if len(variants) >= 32 {
			return nil, ErrConfig
		}
		next := make([]string, 0, len(variants)*2)
		for _, prefix := range variants {
			prefix += raw[previous:pos]
			next = append(next, prefix+raw[pos:end], prefix+"> tbody > tr")
		}
		variants = next
		previous = end
		pos = end - 1
	}
	for index := range variants {
		variants[index] += raw[previous:]
	}
	return variants, nil
}

func compileSelector(raw string) (cascadia.Selector, error) {
	if raw == "" || len(raw) > 8192 {
		return nil, ErrConfig
	}
	normalized, err := normalizeHas(raw)
	if err != nil {
		return nil, err
	}
	variants, err := selectorVariants(normalized)
	if err != nil {
		return nil, err
	}
	selector, err := cascadia.Compile(strings.Join(variants, ","))
	if err != nil {
		return nil, ErrConfig
	}
	return selector, nil
}

func Select(ctx context.Context, root *html.Node, raw string) ([]*html.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	selector, err := compileSelector(raw)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, ErrResponse
	}
	nodes := cascadia.QueryAll(root, selector)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

func cloneNode(node *html.Node) *html.Node {
	copy := &html.Node{Type: node.Type, DataAtom: node.DataAtom, Data: node.Data, Namespace: node.Namespace, Attr: append([]html.Attribute(nil), node.Attr...)}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		copy.AppendChild(cloneNode(child))
	}
	return copy
}

func nodeText(node *html.Node) string {
	var output strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.TextNode {
			output.WriteString(node.Data)
		}
		if node.Type == html.ElementNode && node.Data == "br" {
			output.WriteByte('\n')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(node)
	return strings.TrimSpace(output.String())
}

type FieldSpec struct {
	Selector  string          `json:"selector"`
	Selectors string          `json:"selectors"`
	Attribute string          `json:"attribute"`
	Remove    string          `json:"remove"`
	Index     *int            `json:"index"`
	Contents  *int            `json:"contents"`
	Filters   []Filter        `json:"filters"`
	Case      json.RawMessage `json:"case"`
	Text      json.RawMessage `json:"text"`
}

// Values never mutate the shared document when excluding tags. Attribute
// values remain exact; text preserves line breaks for legacy contents rules.
func Values(ctx context.Context, root *html.Node, field FieldSpec) ([]string, error) {
	raw := field.Selector
	if raw == "" {
		raw = field.Selectors
	}
	nodes, err := Select(ctx, root, raw)
	if err != nil {
		return nil, err
	}
	values := []string{}
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		copy := cloneNode(node)
		if field.Remove != "" {
			remove, err := Select(ctx, copy, field.Remove)
			if err != nil {
				return nil, err
			}
			for _, node := range remove {
				if node.Parent != nil {
					node.Parent.RemoveChild(node)
				}
			}
		}
		value := nodeText(copy)
		if field.Attribute != "" {
			value = ""
			for _, attribute := range copy.Attr {
				if attribute.Key == field.Attribute {
					value = attribute.Val
					break
				}
			}
		}
		values = append(values, value)
	}
	return values, nil
}

func FieldValue(ctx context.Context, root *html.Node, field FieldSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	filters, err := compileFiltersAt(field.Filters, dateClock(ctx))
	if err != nil {
		return "", err
	}
	values, err := Values(ctx, root, field)
	if err != nil || len(values) == 0 {
		return "", err
	}
	index := 0
	if field.Contents != nil {
		values = strings.Split(values[0], "\n")
		index = *field.Contents
	} else if field.Index != nil {
		index = *field.Index
	}
	if index < 0 {
		index += len(values)
	}
	if index < 0 || index >= len(values) {
		return "", nil
	}
	return applyFilters(ctx, strings.TrimSpace(values[index]), filters)
}
