package builtinindexer

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This interpreter accepts only data expressions used by tracker definitions.
// There are no includes, file/network access, functions, or arbitrary execution.
type templateExpr func(map[string]string) (string, error)
type templateCondition func(map[string]string) (bool, error)
type templateNode struct {
	literal   string
	expr      templateExpr
	condition templateCondition
	yes, no   []templateNode
}
type FieldTemplate struct{ nodes []templateNode }

var fieldExpression = regexp.MustCompile(`^fields\[\s*['"]([A-Za-z_][A-Za-z_0-9]*)['"]\s*\](?:\[([0-9]*):([0-9]*)\])?$`)

// Find an operator only outside strings, brackets and parentheses.
func expressionOperator(raw, operator string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '(' || c == '[' {
			depth++
			continue
		}
		if c == ')' || c == ']' {
			depth--
			continue
		}
		if depth == 0 && strings.HasPrefix(raw[i:], operator) {
			return i
		}
	}
	return -1
}

func compileExpression(raw string, depth int) (templateExpr, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || depth > 32 {
		return nil, ErrConfig
	}
	if strings.HasPrefix(raw, "(") && strings.HasSuffix(raw, ")") {
		// Strip only a pair enclosing the entire expression.
		level := 0
		end := -1
		var quote byte
		for i := 0; i < len(raw); i++ {
			c := raw[i]
			if quote != 0 {
				if c == '\\' {
					i++
				} else if c == quote {
					quote = 0
				}
				continue
			}
			if c == '\'' || c == '"' {
				quote = c
				continue
			}
			if c == '(' {
				level++
			}
			if c == ')' {
				level--
				if level == 0 {
					end = i
					break
				}
			}
		}
		if end == len(raw)-1 {
			return compileExpression(raw[1:len(raw)-1], depth+1)
		}
	}
	if pos := expressionOperator(raw, " if "); pos >= 0 {
		tail := raw[pos+4:]
		otherwise := expressionOperator(tail, " else ")
		if otherwise < 0 {
			return nil, ErrConfig
		}
		yes, err := compileExpression(raw[:pos], depth+1)
		if err != nil {
			return nil, err
		}
		condition, err := compileCondition(tail[:otherwise], depth+1)
		if err != nil {
			return nil, err
		}
		no, err := compileExpression(tail[otherwise+6:], depth+1)
		if err != nil {
			return nil, err
		}
		return func(fields map[string]string) (string, error) {
			v, err := condition(fields)
			if err != nil {
				return "", err
			}
			if v {
				return yes(fields)
			}
			return no(fields)
		}, nil
	}
	for _, operator := range []string{" or ", "+", "*"} {
		pos := expressionOperator(raw, operator)
		if pos < 0 {
			continue
		}
		left, err := compileExpression(raw[:pos], depth+1)
		if err != nil {
			return nil, err
		}
		right, err := compileExpression(raw[pos+len(operator):], depth+1)
		if err != nil {
			return nil, err
		}
		var leftTruth templateCondition
		if operator == " or " {
			leftTruth, err = compileCondition(raw[:pos], depth+1)
			if err != nil {
				return nil, err
			}
		}
		return func(fields map[string]string) (string, error) {
			a, err := left(fields)
			if err != nil {
				return "", err
			}
			if operator == " or " {
				truth, err := leftTruth(fields)
				if err != nil {
					return "", err
				}
				if truth {
					return a, nil
				}
			}
			b, err := right(fields)
			if err != nil {
				return "", err
			}
			switch operator {
			case " or ":
				return b, nil
			case "+":
				if len(a)+len(b) > 64<<10 {
					return "", ErrResponse
				}
				return a + b, nil
			case "*":
				x, e1 := strconv.ParseInt(a, 10, 64)
				y, e2 := strconv.ParseInt(b, 10, 64)
				if e1 != nil || e2 != nil || x < 0 || y < 0 || x > 1e9 || y > 1e9 || x*y > 1e12 {
					return "", ErrResponse
				}
				return strconv.FormatInt(x*y, 10), nil
			}
			return "", ErrConfig
		}, nil
	}
	if pos := expressionOperator(raw, "|int"); pos >= 0 && strings.TrimSpace(raw[pos+4:]) == "" {
		inner, err := compileExpression(raw[:pos], depth+1)
		if err != nil {
			return nil, err
		}
		return func(fields map[string]string) (string, error) {
			value, err := inner(fields)
			if err != nil {
				return "", err
			}
			number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return "0", nil
			}
			return strconv.FormatInt(number, 10), nil
		}, nil
	}
	if match := fieldExpression.FindStringSubmatch(raw); match != nil {
		quote := strings.IndexAny(raw, "'\"")
		if quote < 0 || strings.IndexByte(raw[quote+1:], raw[quote]) < 0 {
			return nil, ErrConfig
		}
		start, end := 0, -1
		if strings.HasSuffix(raw, "]") && strings.Contains(raw, ":") {
			var err error
			if match[2] != "" {
				start, err = strconv.Atoi(match[2])
				if err != nil || start > 65536 {
					return nil, ErrConfig
				}
			}
			if match[3] != "" {
				end, err = strconv.Atoi(match[3])
				if err != nil || end > 65536 {
					return nil, ErrConfig
				}
			}
		}
		return func(fields map[string]string) (string, error) {
			value := fields[match[1]]
			if len(value) > 64<<10 || !utf8.ValidString(value) {
				return "", ErrResponse
			}
			if end == -1 && start == 0 {
				return value, nil
			}
			runes := []rune(value)
			s, e := start, end
			if s > len(runes) {
				s = len(runes)
			}
			if e < 0 || e > len(runes) {
				e = len(runes)
			}
			if e < s {
				e = s
			}
			return string(runes[s:e]), nil
		}, nil
	}
	if len(raw) >= 2 && (raw[0] == '\'' || raw[0] == '"') && raw[len(raw)-1] == raw[0] {
		value := raw[1 : len(raw)-1]
		if strings.ContainsAny(value, "\\'\"") {
			return nil, ErrUnsupported
		}
		return func(map[string]string) (string, error) { return value, nil }, nil
	}
	if number, err := strconv.ParseUint(raw, 10, 64); err == nil && number <= 1e9 {
		return func(map[string]string) (string, error) { return raw, nil }, nil
	}
	return nil, ErrUnsupported
}

func compileCondition(raw string, depth int) (templateCondition, error) {
	raw = strings.TrimSpace(raw)
	if depth > 32 {
		return nil, ErrConfig
	}
	if pos := expressionOperator(raw, " or "); pos >= 0 {
		left, err := compileCondition(raw[:pos], depth+1)
		if err != nil {
			return nil, err
		}
		right, err := compileCondition(raw[pos+4:], depth+1)
		if err != nil {
			return nil, err
		}
		return func(fields map[string]string) (bool, error) {
			a, err := left(fields)
			if err != nil || a {
				return a, err
			}
			return right(fields)
		}, nil
	}
	expr, err := compileExpression(raw, depth+1)
	if err != nil {
		return nil, err
	}
	// Jinja treats the string "0" as truthy, unlike the numeric literal zero.
	numeric := false
	if _, err := strconv.ParseUint(raw, 10, 64); err == nil {
		numeric = true
	}
	if expressionOperator(raw, "*") >= 0 || expressionOperator(raw, "|int") >= 0 {
		numeric = true
	}
	return func(fields map[string]string) (bool, error) {
		value, err := expr(fields)
		return value != "" && (!numeric || value != "0"), err
	}, nil
}

func parseTemplate(raw string, pos *int, depth int) ([]templateNode, string, error) {
	if depth > 16 {
		return nil, "", ErrConfig
	}
	nodes := []templateNode{}
	for *pos < len(raw) {
		next := strings.Index(raw[*pos:], "{")
		if next < 0 {
			nodes = append(nodes, templateNode{literal: raw[*pos:]})
			*pos = len(raw)
			break
		}
		next += *pos
		if next > *pos {
			nodes = append(nodes, templateNode{literal: raw[*pos:next]})
		}
		*pos = next
		if strings.HasPrefix(raw[*pos:], "{{") {
			end := strings.Index(raw[*pos+2:], "}}")
			if end < 0 {
				return nil, "", ErrConfig
			}
			end += *pos + 2
			expr, err := compileExpression(raw[*pos+2:end], 0)
			if err != nil {
				return nil, "", err
			}
			nodes = append(nodes, templateNode{expr: expr})
			*pos = end + 2
			continue
		}
		if strings.HasPrefix(raw[*pos:], "{%") {
			end := strings.Index(raw[*pos+2:], "%}")
			if end < 0 {
				return nil, "", ErrConfig
			}
			end += *pos + 2
			tag := strings.TrimSpace(raw[*pos+2 : end])
			*pos = end + 2
			if tag == "else" || tag == "endif" {
				return nodes, tag, nil
			}
			if !strings.HasPrefix(tag, "if ") {
				return nil, "", ErrUnsupported
			}
			condition, err := compileCondition(tag[3:], 0)
			if err != nil {
				return nil, "", err
			}
			yes, stop, err := parseTemplate(raw, pos, depth+1)
			if err != nil {
				return nil, "", err
			}
			var no []templateNode
			if stop == "else" {
				no, stop, err = parseTemplate(raw, pos, depth+1)
				if err != nil {
					return nil, "", err
				}
			}
			if stop != "endif" {
				return nil, "", ErrConfig
			}
			nodes = append(nodes, templateNode{condition: condition, yes: yes, no: no})
			continue
		}
		nodes = append(nodes, templateNode{literal: "{"})
		*pos = *pos + 1
	}
	return nodes, "", nil
}

func CompileFieldTemplate(raw string) (FieldTemplate, error) {
	if len(raw) > 16<<10 || !utf8.ValidString(raw) {
		return FieldTemplate{}, ErrConfig
	}
	pos := 0
	nodes, stop, err := parseTemplate(raw, &pos, 0)
	if err != nil {
		return FieldTemplate{}, err
	}
	if stop != "" {
		return FieldTemplate{}, ErrConfig
	}
	return FieldTemplate{nodes: nodes}, nil
}

func (template FieldTemplate) Render(ctx context.Context, fields map[string]string) (string, error) {
	var output strings.Builder
	var render func([]templateNode) error
	render = func(nodes []templateNode) error {
		for _, node := range nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			value := node.literal
			if node.expr != nil {
				var err error
				value, err = node.expr(fields)
				if err != nil {
					return err
				}
			}
			if node.condition != nil {
				condition, err := node.condition(fields)
				if err != nil {
					return err
				}
				branch := node.no
				if condition {
					branch = node.yes
				}
				if err := render(branch); err != nil {
					return err
				}
				continue
			}
			if len(value)+output.Len() > 64<<10 {
				return ErrResponse
			}
			output.WriteString(value)
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := render(template.nodes); err != nil {
		return "", err
	}
	return output.String(), nil
}
