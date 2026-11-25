package database

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Keep the original CREATE declaration, including column order, constraints,
// generated expressions, comments and table options. Only replace the tokens
// SQLite identified as the old declared types at the start of column clauses.
var ddlTokens = regexp.MustCompile("(?s)'(?:''|[^'])*'|\"(?:\"\"|[^\"])*\"|`(?:``|[^`])*`|\\[[^]]*\\]|--[^\\n]*|/\\*.*?(?:\\*/|\\z)|[a-zA-Z0-9_$\\x{0080}-\\x{10ffff}]+|\\s+|.")

func schemaTokens(sql string) [][]int {
	var result [][]int
	for _, span := range ddlTokens.FindAllStringIndex(sql, -1) {
		raw := sql[span[0]:span[1]]
		if strings.TrimSpace(raw) != "" && !strings.HasPrefix(raw, "--") && !strings.HasPrefix(raw, "/*") {
			result = append(result, span)
		}
	}
	return result
}

func ddlName(raw string) string {
	switch raw[0] {
	case '[', '"', '\'', '`':
		if len(raw) < 2 {
			return ""
		}
		inside := raw[1 : len(raw)-1]
		if raw[0] == '[' {
			return inside
		}
		q := raw[:1]
		return strings.ReplaceAll(inside, q+q, q)
	default:
		return raw
	}
}

func (change historicalTypes) revisedDDL(ddl, shadow string, changed map[string]bool) (string, error) {
	tokens := schemaTokens(ddl)
	raw := func(i int) string { return ddl[tokens[i][0]:tokens[i][1]] }
	if len(tokens) < 5 || !strings.EqualFold(raw(0), "CREATE") || !strings.EqualFold(raw(1), "TABLE") || !strings.EqualFold(ddlName(raw(2)), change.table) || raw(3) != "(" {
		return "", errors.New("unexpected SQLite table declaration")
	}
	var out strings.Builder
	out.WriteString("CREATE TABLE " + quoted(shadow) + " ")
	last := tokens[3][0]
	depth := 0
	replaced := map[string]bool{}
	for i := 3; i < len(tokens); i++ {
		part := raw(i)
		if part == ";" && i != len(tokens)-1 {
			return "", errors.New("invalid table declaration")
		}
		startColumn := part == "(" && depth == 0 || part == "," && depth == 1
		switch part {
		case "(":
			depth++
		case ")":
			depth--
		}
		if !startColumn || i+2 >= len(tokens) {
			continue
		}
		name := strings.ToUpper(ddlName(raw(i + 1)))
		if !changed[name] {
			continue
		}
		if replaced[name] || !strings.EqualFold(ddlName(raw(i+2)), change.from) {
			return "", fmt.Errorf("unexpected declaration for %s", name)
		}
		out.WriteString(ddl[last:tokens[i+2][0]])
		out.WriteString(change.to)
		last = tokens[i+2][1]
		replaced[name] = true
	}
	if depth != 0 || len(replaced) != len(changed) {
		return "", errors.New("incomplete column declarations")
	}
	out.WriteString(ddl[last:])
	return out.String(), nil
}

// sqlite_schema normalizes CREATE prefixes. Disallow trailing statements;
// semicolons inside trigger bodies and quoted literals are part of one object.
func singleSchemaObject(ddl string) bool {
	tokens := schemaTokens(ddl)
	raw := func(i int) string { return strings.ToUpper(ddl[tokens[i][0]:tokens[i][1]]) }
	if len(tokens) < 4 || raw(0) != "CREATE" {
		return false
	}
	trigger := raw(1) == "TRIGGER"
	if !trigger && raw(1) != "INDEX" && !(raw(1) == "UNIQUE" && raw(2) == "INDEX") {
		return false
	}
	body := false
	for i := 2; i < len(tokens); i++ {
		switch raw(i) {
		case "BEGIN":
			if trigger {
				body = true
			}
		case ";":
			if !body {
				return i == len(tokens)-1
			}
			if i+1 < len(tokens) && raw(i+1) == "END" {
				return i+2 == len(tokens) || i+3 == len(tokens) && raw(i+2) == ";"
			}
		}
	}
	return !trigger
}
