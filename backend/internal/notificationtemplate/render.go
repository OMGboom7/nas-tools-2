// Package notificationtemplate renders operator-saved Jinja notification bodies.
package notificationtemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/config"
	jinja "github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/nikolalohinski/gonja/v2/parser"
)

var ErrRender = errors.New("notification template could not be rendered")
var slots = make(chan struct{}, 4)

const limit = 8 << 20

type input struct {
	Source    string
	Variables map[string]any
}

// Rendering is isolated from the service and its configuration. Expensive
// loops and recursive macros can be stopped when their caller goes away.
func Render(ctx context.Context, source string, variables map[string]any) ([]byte, error) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ErrRender
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := json.Marshal(input{source, variables})
	if err != nil || len(data) >= limit {
		return nil, ErrRender
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, ErrRender
	}
	cmd := exec.CommandContext(ctx, exe, "render-notification")
	cmd.Env = []string{"GOMEMLIMIT=64MiB", "GOMAXPROCS=1"}
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 || len(out) > limit {
		return nil, ErrRender
	}
	return out, nil
}

type limitedOutput struct {
	w         io.Writer
	remaining int
}

func (w *limitedOutput) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, ErrRender
	}
	n, err := w.w.Write(b)
	w.remaining -= n
	return n, err
}

// Command runs only in the disposable local renderer process. It sees the
// template and its data, never the channel URL, HTTP headers or credentials.
func Command(in io.Reader, out io.Writer) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrRender
		}
	}()
	// The child must also exit if its parent is forcibly terminated.
	deadline := time.AfterFunc(5*time.Second, func() { os.Exit(1) })
	defer deadline.Stop()
	defer debug.SetMaxStack(debug.SetMaxStack(8 << 20))
	if err := limitRendererMemory(); err != nil {
		return ErrRender
	}
	return render(in, out)
}

func render(in io.Reader, out io.Writer) error {
	var q input
	r := &io.LimitedReader{R: in, N: limit}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if dec.Decode(&q) != nil || dec.Decode(new(any)) != io.EOF || r.N == 0 || q.Source == "" || q.Variables == nil {
		return ErrRender
	}
	if _, err := templateNumbers(q.Variables); err != nil {
		return ErrRender
	}
	loader := loaders.MustNewMemoryLoader(map[string]string{"/notification": q.Source})
	// The original environment had no external template loader.
	controls := jinja.NewControlStructureSet(map[string]parser.ControlStructureParser{})
	for _, name := range []string{"autoescape", "block", "filter", "for", "if", "macro", "raw", "set", "with", "call"} {
		fn, _ := builtins.ControlStructures.Get(name)
		if err := controls.Register(name, fn); err != nil {
			return ErrRender
		}
	}
	filters := jinja.NewFilterSet(map[string]jinja.FilterFunction{}).Update(builtins.Filters)
	if err := filters.Replace("tojson", toJSON); err != nil {
		return ErrRender
	}
	globals := jinja.EmptyContext().Update(builtins.GlobalFunctions)
	globals.Set("range", boundedRange)
	env := &jinja.Environment{Context: globals, Filters: filters, Tests: builtins.Tests, Methods: builtins.Methods, ControlStructures: controls}
	t, err := jinja.NewTemplate("/notification", config.New(), loader, env)
	if err != nil {
		return ErrRender
	}
	writer := &limitedOutput{w: out, remaining: limit}
	if err = t.Execute(writer, jinja.NewContext(q.Variables)); err != nil || writer.remaining == limit {
		return ErrRender
	}
	return nil
}

// JSON numbers arrive from both legacy callers and saved media lists. Keep
// integer identifiers exact when they exceed the precision of a float64.
func templateNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		if !strings.ContainsAny(v.String(), ".Ee") {
			return strconv.ParseUint(v.String(), 10, 64)
		}
		return v.Float64()
	case []any:
		for i, x := range v {
			n, err := templateNumbers(x)
			if err != nil {
				return nil, err
			}
			v[i] = n
		}
	case map[string]any:
		for k, x := range v {
			n, err := templateNumbers(x)
			if err != nil {
				return nil, err
			}
			v[k] = n
		}
	}
	return value, nil
}

// Jinja's sandbox permits at most 100000 elements in range().
func boundedRange(args *jinja.VarArgs) ([]int, error) {
	if len(args.Args) < 1 || len(args.Args) > 3 || len(args.KwArgs) != 0 {
		return nil, ErrRender
	}
	for _, a := range args.Args {
		if !a.IsInteger() {
			return nil, ErrRender
		}
	}
	start, stop, step := 0, args.Args[0].Integer(), 1
	if len(args.Args) > 1 {
		start, stop = stop, args.Args[1].Integer()
	}
	if len(args.Args) == 3 {
		step = args.Args[2].Integer()
	}
	if step == 0 {
		return nil, ErrRender
	}
	var result []int
	for n := start; (step > 0 && n < stop) || (step < 0 && n > stop); {
		if len(result) >= 100000 {
			return nil, ErrRender
		}
		result = append(result, n)
		next := n + step
		if step > 0 && next < n || step < 0 && next > n {
			break
		}
		n = next
	}
	return result, nil
}

// Jinja's default JSON policy uses ASCII escapes and a space after element
// separators. Gonja supplies HTML-safe quoting and converts template dicts.
func toJSON(e *jinja.Evaluator, in *jinja.Value, args *jinja.VarArgs) *jinja.Value {
	if len(args.Args)+len(args.KwArgs) > 1 {
		return jinja.AsValue(ErrRender)
	}
	var indent *jinja.Value
	if len(args.Args) == 1 {
		indent = args.Args[0]
	}
	if len(args.KwArgs) > 0 {
		indent = args.KwArgs["indent"]
		if indent == nil {
			return jinja.AsValue(ErrRender)
		}
	}
	var padding string
	pretty := indent != nil && !indent.IsNil()
	if pretty {
		switch {
		case indent.IsString():
			padding = indent.String()
		case indent.IsInteger() && indent.Integer() <= limit:
			padding = strings.Repeat(" ", max(0, indent.Integer()))
		default:
			return jinja.AsValue(ErrRender)
		}
	}
	base, _ := builtins.Filters.Get("tojson")
	val := base(e, in, &jinja.VarArgs{})
	if val.IsError() {
		return val
	}
	raw := val.String()
	if pretty {
		var b bytes.Buffer
		if err := json.Indent(&b, []byte(raw), "", ""); err != nil {
			return jinja.AsValue(ErrRender)
		}
		raw = b.String()
	}
	var b strings.Builder
	quoted, escaped := false, false
	depth := 0
	for i, r := range raw {
		if r >= 127 {
			if r > 0xffff {
				v := r - 0x10000
				writeEscape(&b, 0xd800+v/1024)
				writeEscape(&b, 0xdc00+v%1024)
			} else {
				writeEscape(&b, r)
			}
		} else {
			b.WriteRune(r)
			switch {
			case escaped:
				escaped = false
			case quoted && r == '\\':
				escaped = true
			case r == '"':
				quoted = !quoted
			case pretty && !quoted && (r == '{' || r == '['):
				depth++
			case pretty && !quoted && (r == '}' || r == ']'):
				depth--
			case pretty && r == '\n':
				level := depth
				if i+1 < len(raw) && (raw[i+1] == '}' || raw[i+1] == ']') {
					level--
				}
				if level > 0 && len(padding) > (limit-b.Len())/level {
					return jinja.AsValue(ErrRender)
				}
				b.WriteString(strings.Repeat(padding, level))
			case !pretty && !quoted && (r == ':' || r == ','):
				b.WriteByte(' ')
			}
		}
		if b.Len() > limit {
			return jinja.AsValue(ErrRender)
		}
	}
	return jinja.AsSafeValue(b.String())
}

func writeEscape(b *strings.Builder, r rune) {
	const hex = "0123456789abcdef"
	b.WriteString(`\u`)
	for _, shift := range []uint{12, 8, 4, 0} {
		b.WriteByte(hex[(r>>shift)&15])
	}
}
