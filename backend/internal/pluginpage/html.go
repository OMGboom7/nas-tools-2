// Package pluginpage extracts the text and tables used by clients of an
// external plugin. The original plugin supplies HTML on the data host.
package pluginpage

import (
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const maxInput = 8 << 20

var link = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
var numericID = regexp.MustCompile(`^[0-9]{1,30}$`)
var archiveName = regexp.MustCompile(`^归档_[0-9]{14}\.md$`)

type rowAction struct {
	Type string `json:"type"`
	ID   string `json:"record_id"`
}
type table struct {
	Columns []string     `json:"columns"`
	Rows    [][]string   `json:"rows"`
	Actions []*rowAction `json:"row_actions,omitempty"`
}
type page struct {
	Title    string   `json:"title"`
	Sections []string `json:"sections"`
	Tables   []table  `json:"tables"`
	Omitted  bool     `json:"actions_omitted"`
}

// CleanText drops control characters and reduces URLs to hostnames before
// truncation so URL credentials, private paths and query strings stay private.
func CleanText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = link.ReplaceAllStringFunc(value, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Hostname()
	})
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

// Convert reads one page from a pipe. Never emit the original markup or
// decoder errors containing plugin-supplied content.
func Convert(reader io.Reader, writer io.Writer) error {
	input, err := io.ReadAll(io.LimitReader(reader, maxInput+1))
	if err != nil || len(input) > maxInput {
		return errors.New("plugin page input could not be read within the limit")
	}
	var source struct {
		ID, Title, HTML string
		HasAction       bool
	}
	if err := json.Unmarshal(input, &source); err != nil {
		return errors.New("invalid plugin page input")
	}
	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(source.HTML), root)
	if err != nil {
		return errors.New("invalid plugin page HTML")
	}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	data := page{Title: CleanText(source.Title, 120), Sections: []string{}, Tables: []table{}, Omitted: source.HasAction}
	stripMarkup(root, &data.Omitted)
	visitedTables := 0
	for n := range root.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		switch n.Data {
		case "table":
			visitedTables++
			if visitedTables <= 10 {
				if t := readTable(n, source.ID); len(t.Columns) > 0 {
					data.Tables = append(data.Tables, t)
				}
			}
		case "h1", "h2", "h3", "h4", "h5", "h6", "p", "li":
			if len(data.Sections) >= 80 || insideTable(n) {
				continue
			}
			value := nodeText(n, 500)
			if value != "" && (len(data.Sections) == 0 || data.Sections[len(data.Sections)-1] != value) {
				data.Sections = append(data.Sections, value)
			}
		}
	}
	return json.NewEncoder(writer).Encode(data)
}

func stripMarkup(root *html.Node, omitted *bool) {
	var remove []*html.Node
	for n := range root.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		for _, a := range n.Attr {
			if strings.HasPrefix(a.Key, "on") || a.Key == "href" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Val)), "javascript:") {
				*omitted = true
			}
		}
		switch n.Data {
		case "script", "style", "iframe", "object", "embed", "form", "button", "input", "textarea", "select", "template", "noscript", "svg", "math":
			remove = append(remove, n)
		}
	}
	for _, n := range remove {
		n.Parent.RemoveChild(n)
	}
}

func insideTable(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.DataAtom == atom.Table {
			return true
		}
	}
	return false
}
func nodeText(n *html.Node, limit int) string {
	var buf strings.Builder
	for child := range n.Descendants() {
		if child.Type == html.TextNode {
			buf.WriteString(child.Data)
			buf.WriteByte(' ')
		}
	}
	return CleanText(buf.String(), limit)
}

// rowsIn only includes rows directly owned by this table, even if a cell
// contains a nested table. The HTML parser supplies tbody for bare tr rows.
func rowsIn(table *html.Node, section atom.Atom) iter.Seq[*html.Node] {
	return func(yield func(*html.Node) bool) {
		for group := range table.ChildNodes() {
			if group.DataAtom != section {
				continue
			}
			for row := range group.ChildNodes() {
				if row.DataAtom == atom.Tr && !yield(row) {
					return
				}
			}
		}
	}
}

func cells(row *html.Node) iter.Seq2[int, *html.Node] {
	return func(yield func(int, *html.Node) bool) {
		index := 0
		for cell := range row.ChildNodes() {
			if cell.DataAtom == atom.Td || cell.DataAtom == atom.Th {
				if !yield(index, cell) {
					return
				}
				index++
			}
		}
	}
}

func readTable(node *html.Node, id string) table {
	t := table{Rows: [][]string{}}
	keep := make(map[int]int)
	for row := range rowsIn(node, atom.Thead) {
		for i, cell := range cells(row) {
			name := nodeText(cell, 120)
			if name == "" || name == "操作" || name == "动作" {
				continue
			}
			keep[i] = len(t.Columns)
			t.Columns = append(t.Columns, name)
			if len(t.Columns) == 30 {
				break
			}
		}
		break
	}
	if len(keep) == 0 {
		return t
	}
	var actions []*rowAction
	visited := 0
	for row := range rowsIn(node, atom.Tbody) {
		if visited == 200 {
			break
		}
		visited++
		values := make([]string, len(keep))
		nonempty := false
		for j, cell := range cells(row) {
			if dest, ok := keep[j]; ok {
				values[dest] = nodeText(cell, 500)
				nonempty = nonempty || values[dest] != ""
			}
		}
		if nonempty {
			t.Rows = append(t.Rows, values)
			action := recordAction(id, row)
			actions = append(actions, action)
			if action != nil || t.Actions != nil {
				t.Actions = actions
			}
		}
	}
	return t
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func recordAction(id string, row *html.Node) *rowAction {
	var prefix string
	switch id {
	case "DoubanSync":
		prefix = "douban_history_"
	case "DoubanRank":
		prefix = "movie_rank_history_"
	case "MovieRandom":
		prefix = "movie_random_history_"
	case "MediaLibraryArchive":
		for n := range row.Descendants() {
			if name := attr(n, "data-name"); name != "" {
				if archiveName.MatchString(name) {
					return &rowAction{"delete", name}
				}
				break
			}
		}
		return nil
	default:
		return nil
	}
	if key, ok := strings.CutPrefix(attr(row, "id"), prefix); ok && numericID.MatchString(key) {
		return &rowAction{"delete", key}
	}
	return nil
}
