// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// A Jinja subset evaluator for the templates this bridge puts into its
// discovery documents, so a test can render them against the payloads the
// publish path really writes — see template_contract_test.go.
//
// It is deliberately NOT lenient. Home Assistant renders these templates with
// Jinja2 and its own LoggingUndefined (helpers/template/__init__.py,
// make_logging_undefined): touching an attribute of an undefined value raises
// (logged at ERROR, and the MQTT entity then keeps its previous state), and
// rendering or testing the truth of one logs a WARNING. Both are reported here
// as errors, because a template that logs on a payload this bridge publishes is
// a defect even when Home Assistant survives it. And a construct the evaluator
// does not know fails the render with errJinjaUnsupported instead of being
// skipped: a contract test that silently passes a template it could not read
// proves nothing.
//
// Supported: text, `{{ expr }}`, `{% set name = expr %}`, `{% if %}` with
// `{% elif %}`/`{% else %}`; expressions with `a if c else b`, `or`, `and`,
// `not`, the comparisons, string/number/none/boolean literals, dict and list
// literals, attribute and item access, `dict.get(k[, d])`, the filters `lower`,
// `int`, `float` and `tojson` and the tests `defined` and `none`. Python's
// value semantics are followed where they matter here: JSON integers stay
// integers, `str()` of a float is its repr, True == 1, and `None` renders as
// "None".

// errJinjaUnsupported marks a template shape the evaluator cannot evaluate.
var errJinjaUnsupported = errors.New("jinja subset: unsupported construct")

// jUndefined is Jinja's Undefined. Its name is only for messages.
type jUndefined struct{ name string }

// jMethod is a bound dict method (only `get` is supported).
type jMethod struct {
	recv map[string]any
	name string
}

// renderJinja renders tmpl with `value` = payload and `value_json` = the
// payload decoded as JSON, or undefined when it is not JSON — what Home
// Assistant's async_render_with_possible_json_value does. The result is
// stripped, as Home Assistant strips it.
func renderJinja(tmpl, payload string) (string, error) {
	nodes, err := parseJinja(tmpl)
	if err != nil {
		return "", err
	}
	vars := map[string]any{"value": payload}
	if v, ok := pyJSONLoads(payload); ok {
		vars["value_json"] = v
	}
	var out strings.Builder
	if err := execNodes(nodes, vars, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// pyJSONLoads decodes like Python's json.loads: integers stay integers.
func pyJSONLoads(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false // trailing data
	}
	return pyNumbers(v), true
}

func pyNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return i
		}
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = pyNumbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = pyNumbers(e)
		}
	}
	return v
}

// --- template structure ------------------------------------------------------

type jNode interface{}

type (
	jText   struct{ s string }
	jOutput struct{ expr jExpr }
	jSet    struct {
		name string
		expr jExpr
	}
	jIf struct {
		conds  []jExpr
		bodies [][]jNode
		orElse []jNode
	}
)

// parseJinja splits tmpl into text, output and statement segments and nests
// the if blocks.
func parseJinja(tmpl string) ([]jNode, error) {
	type segment struct {
		kind byte // 't', 'o', 's'
		body string
	}
	var segs []segment
	for rest := tmpl; rest != ""; {
		i := strings.Index(rest, "{")
		for i >= 0 && i+1 < len(rest) && rest[i+1] != '{' && rest[i+1] != '%' && rest[i+1] != '#' {
			j := strings.Index(rest[i+1:], "{")
			if j < 0 {
				i = -1
				break
			}
			i += 1 + j
		}
		if i < 0 || i+1 >= len(rest) {
			segs = append(segs, segment{'t', rest})
			break
		}
		if i > 0 {
			segs = append(segs, segment{'t', rest[:i]})
		}
		open := rest[i : i+2]
		if open == "{#" {
			return nil, fmt.Errorf("%w: comment in %q", errJinjaUnsupported, tmpl)
		}
		closer := "}}"
		kind := byte('o')
		if open == "{%" {
			closer, kind = "%}", 's'
		}
		end := strings.Index(rest[i+2:], closer)
		if end < 0 {
			return nil, fmt.Errorf("unterminated %s in %q", open, tmpl)
		}
		body := rest[i+2 : i+2+end]
		if strings.HasPrefix(body, "-") || strings.HasSuffix(body, "-") {
			return nil, fmt.Errorf("%w: whitespace control in %q", errJinjaUnsupported, tmpl)
		}
		segs = append(segs, segment{kind, strings.TrimSpace(body)})
		rest = rest[i+2+end+2:]
	}

	pos := 0
	var block func(stop ...string) ([]jNode, string, error)
	block = func(stop ...string) ([]jNode, string, error) {
		var out []jNode
		for pos < len(segs) {
			sg := segs[pos]
			pos++
			switch sg.kind {
			case 't':
				out = append(out, jText{sg.body})
			case 'o':
				e, err := parseExpr(sg.body)
				if err != nil {
					return nil, "", err
				}
				out = append(out, jOutput{e})
			case 's':
				word, arg, _ := strings.Cut(sg.body, " ")
				for _, s := range stop {
					if word == s {
						return out, sg.body, nil
					}
				}
				switch word {
				case "set":
					name, rhs, ok := strings.Cut(arg, "=")
					name = strings.TrimSpace(name)
					if !ok || !isIdent(name) {
						return nil, "", fmt.Errorf("%w: set %q", errJinjaUnsupported, arg)
					}
					e, err := parseExpr(rhs)
					if err != nil {
						return nil, "", err
					}
					out = append(out, jSet{name, e})
				case "if":
					n := jIf{}
					cond := arg
					for {
						c, err := parseExpr(cond)
						if err != nil {
							return nil, "", err
						}
						body, term, err := block("elif", "else", "endif")
						if err != nil {
							return nil, "", err
						}
						n.conds, n.bodies = append(n.conds, c), append(n.bodies, body)
						tw, targ, _ := strings.Cut(term, " ")
						if tw == "elif" {
							cond = targ
							continue
						}
						if tw == "else" {
							n.orElse, term, err = block("endif")
							if err != nil {
								return nil, "", err
							}
							if term != "endif" {
								return nil, "", fmt.Errorf("if without endif in %q", tmpl)
							}
						} else if tw != "endif" {
							return nil, "", fmt.Errorf("if without endif in %q", tmpl)
						}
						break
					}
					out = append(out, n)
				default:
					return nil, "", fmt.Errorf("%w: statement %q", errJinjaUnsupported, sg.body)
				}
			}
		}
		if len(stop) > 0 {
			return nil, "", fmt.Errorf("missing {%% %s %%} in %q", stop[len(stop)-1], tmpl)
		}
		return out, "", nil
	}
	nodes, _, err := block()
	return nodes, err
}

func execNodes(nodes []jNode, vars map[string]any, out *strings.Builder) error {
	for _, n := range nodes {
		switch n := n.(type) {
		case jText:
			out.WriteString(n.s)
		case jOutput:
			v, err := n.expr(vars)
			if err != nil {
				return err
			}
			s, err := pyStr(v)
			if err != nil {
				return err
			}
			out.WriteString(s)
		case jSet:
			v, err := n.expr(vars)
			if err != nil {
				return err
			}
			vars[n.name] = v
		case jIf:
			done := false
			for i, c := range n.conds {
				v, err := c(vars)
				if err != nil {
					return err
				}
				t, err := pyTruth(v)
				if err != nil {
					return err
				}
				if t {
					if err := execNodes(n.bodies[i], vars, out); err != nil {
						return err
					}
					done = true
					break
				}
			}
			if !done {
				if err := execNodes(n.orElse, vars, out); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// --- expressions -------------------------------------------------------------

// jExpr is a compiled expression.
type jExpr func(vars map[string]any) (any, error)

type jTok struct {
	kind byte // 'n' name, 's' string, 'd' number, 'o' operator, 'e' end
	text string
	val  any
}

func lexExpr(src string) ([]jTok, error) {
	var toks []jTok
	for i := 0; i < len(src); {
		ch := src[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n':
			i++
		case ch == '\'' || ch == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(src) && src[j] != ch; j++ {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case '\\', '\'', '"':
						b.WriteByte(src[j])
					default:
						return nil, fmt.Errorf("%w: escape \\%c in %q", errJinjaUnsupported, src[j], src)
					}
					continue
				}
				b.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string in %q", src)
			}
			toks = append(toks, jTok{kind: 's', val: b.String()})
			i = j + 1
		case ch >= '0' && ch <= '9':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			lit := src[i:j]
			if strings.Contains(lit, ".") {
				f, err := strconv.ParseFloat(lit, 64)
				if err != nil {
					return nil, err
				}
				toks = append(toks, jTok{kind: 'd', val: f})
			} else {
				n, err := strconv.ParseInt(lit, 10, 64)
				if err != nil {
					return nil, err
				}
				toks = append(toks, jTok{kind: 'd', val: n})
			}
			i = j
		case ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			toks = append(toks, jTok{kind: 'n', text: src[i:j]})
			i = j
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "==", "!=", ">=", "<=":
				toks = append(toks, jTok{kind: 'o', text: two})
				i += 2
				continue
			}
			if !strings.ContainsRune("(){}[],:.|<>", rune(ch)) {
				return nil, fmt.Errorf("%w: operator %q in %q", errJinjaUnsupported, string(ch), src)
			}
			toks = append(toks, jTok{kind: 'o', text: string(ch)})
			i++
		}
	}
	return append(toks, jTok{kind: 'e'}), nil
}

type jParser struct {
	toks []jTok
	pos  int
	src  string
}

func parseExpr(src string) (jExpr, error) {
	toks, err := lexExpr(src)
	if err != nil {
		return nil, err
	}
	p := &jParser{toks: toks, src: src}
	e, err := p.ternary()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != 'e' {
		return nil, fmt.Errorf("%w: trailing %q in %q", errJinjaUnsupported, p.peek().text, src)
	}
	return e, nil
}

func (p *jParser) peek() jTok { return p.toks[p.pos] }

func (p *jParser) isOp(s string) bool { t := p.peek(); return t.kind == 'o' && t.text == s }

func (p *jParser) isWord(s string) bool { t := p.peek(); return t.kind == 'n' && t.text == s }

func (p *jParser) expect(s string) error {
	if !p.isOp(s) {
		return fmt.Errorf("expected %q in %q", s, p.src)
	}
	p.pos++
	return nil
}

func (p *jParser) ternary() (jExpr, error) {
	body, err := p.or()
	if err != nil {
		return nil, err
	}
	if !p.isWord("if") {
		return body, nil
	}
	p.pos++
	cond, err := p.or()
	if err != nil {
		return nil, err
	}
	if !p.isWord("else") {
		return nil, fmt.Errorf("%w: conditional without else in %q", errJinjaUnsupported, p.src)
	}
	p.pos++
	alt, err := p.ternary()
	if err != nil {
		return nil, err
	}
	return func(vars map[string]any) (any, error) {
		c, err := cond(vars)
		if err != nil {
			return nil, err
		}
		t, err := pyTruth(c)
		if err != nil {
			return nil, err
		}
		if t {
			return body(vars)
		}
		return alt(vars)
	}, nil
}

func (p *jParser) or() (jExpr, error)  { return p.logical("or", p.and) }
func (p *jParser) and() (jExpr, error) { return p.logical("and", p.not) }

// logical parses a left-associative and/or chain with Python's short-circuit
// semantics: the result is the deciding operand, not a boolean.
func (p *jParser) logical(word string, next func() (jExpr, error)) (jExpr, error) {
	left, err := next()
	if err != nil {
		return nil, err
	}
	for p.isWord(word) {
		p.pos++
		right, err := next()
		if err != nil {
			return nil, err
		}
		l := left
		left = func(vars map[string]any) (any, error) {
			a, err := l(vars)
			if err != nil {
				return nil, err
			}
			t, err := pyTruth(a)
			if err != nil {
				return nil, err
			}
			if t == (word == "or") {
				return a, nil
			}
			return right(vars)
		}
	}
	return left, nil
}

func (p *jParser) not() (jExpr, error) {
	if !p.isWord("not") {
		return p.compare()
	}
	p.pos++
	inner, err := p.not()
	if err != nil {
		return nil, err
	}
	return func(vars map[string]any) (any, error) {
		v, err := inner(vars)
		if err != nil {
			return nil, err
		}
		t, err := pyTruth(v)
		return !t, err
	}, nil
}

func (p *jParser) compare() (jExpr, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != 'o' || !slicesContains([]string{"==", "!=", "<", ">", "<=", ">="}, t.text) {
			return left, nil
		}
		p.pos++
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		op, l := t.text, left
		left = func(vars map[string]any) (any, error) {
			a, err := l(vars)
			if err != nil {
				return nil, err
			}
			b, err := right(vars)
			if err != nil {
				return nil, err
			}
			return pyCompare(op, a, b)
		}
	}
}

func slicesContains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// unary is a primary with its postfix accessors, then its filters and tests —
// Jinja's parse_unary/parse_filter_expr, which is why `x | int(0) >= 2` is
// `(x | int(0)) >= 2`.
func (p *jParser) unary() (jExpr, error) {
	e, err := p.postfix()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.isOp("|"):
			p.pos++
			if p.peek().kind != 'n' {
				return nil, fmt.Errorf("filter name expected in %q", p.src)
			}
			name := p.peek().text
			p.pos++
			var args []jExpr
			if p.isOp("(") {
				p.pos++
				if args, err = p.args(")"); err != nil {
					return nil, err
				}
			}
			if e, err = p.filter(e, name, args); err != nil {
				return nil, err
			}
		case p.isWord("is"):
			p.pos++
			negate := false
			if p.isWord("not") {
				p.pos++
				negate = true
			}
			if p.peek().kind != 'n' {
				return nil, fmt.Errorf("test name expected in %q", p.src)
			}
			name := p.peek().text
			p.pos++
			subject := e
			var test func(any) bool
			switch name {
			case "defined":
				test = func(v any) bool { _, u := v.(jUndefined); return !u }
			case "none":
				test = func(v any) bool { return v == nil }
			default:
				return nil, fmt.Errorf("%w: test %q in %q", errJinjaUnsupported, name, p.src)
			}
			e = func(vars map[string]any) (any, error) {
				v, err := subject(vars)
				if err != nil {
					return nil, err
				}
				return test(v) != negate, nil
			}
		default:
			return e, nil
		}
	}
}

func (p *jParser) filter(subject jExpr, name string, args []jExpr) (jExpr, error) {
	evalArgs := func(vars map[string]any) ([]any, error) {
		out := make([]any, len(args))
		for i, a := range args {
			v, err := a(vars)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	var f func(v any, args []any) (any, error)
	switch name {
	case "lower":
		if len(args) != 0 {
			return nil, fmt.Errorf("%w: lower with arguments in %q", errJinjaUnsupported, p.src)
		}
		f = func(v any, _ []any) (any, error) {
			s, err := pyStr(v)
			return strings.ToLower(s), err
		}
	case "int", "float":
		if len(args) > 1 {
			return nil, fmt.Errorf("%w: %s with %d arguments in %q", errJinjaUnsupported, name, len(args), p.src)
		}
		f = func(v any, args []any) (any, error) {
			n, ok, err := pyNumber(v, name == "int")
			if err != nil {
				return nil, err
			}
			if ok {
				return n, nil
			}
			if len(args) == 0 {
				return nil, fmt.Errorf("template error: %s got invalid input %v and no default", name, v)
			}
			return args[0], nil
		}
	case "tojson":
		if len(args) != 0 {
			return nil, fmt.Errorf("%w: tojson with arguments in %q", errJinjaUnsupported, p.src)
		}
		f = func(v any, _ []any) (any, error) {
			if u, ok := v.(jUndefined); ok {
				return nil, fmt.Errorf("template error: tojson of undefined %s", u.name)
			}
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(v); err != nil {
				return nil, err
			}
			return strings.TrimSpace(b.String()), nil
		}
	default:
		return nil, fmt.Errorf("%w: filter %q in %q", errJinjaUnsupported, name, p.src)
	}
	return func(vars map[string]any) (any, error) {
		v, err := subject(vars)
		if err != nil {
			return nil, err
		}
		a, err := evalArgs(vars)
		if err != nil {
			return nil, err
		}
		return f(v, a)
	}, nil
}

func (p *jParser) args(closer string) ([]jExpr, error) {
	var out []jExpr
	for !p.isOp(closer) {
		e, err := p.ternary()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if p.isOp(",") {
			p.pos++
			continue
		}
		if !p.isOp(closer) {
			return nil, fmt.Errorf("expected %q in %q", closer, p.src)
		}
	}
	p.pos++
	return out, nil
}

func (p *jParser) postfix() (jExpr, error) {
	e, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.isOp("."):
			p.pos++
			if p.peek().kind != 'n' {
				return nil, fmt.Errorf("attribute name expected in %q", p.src)
			}
			name := p.peek().text
			p.pos++
			obj := e
			e = func(vars map[string]any) (any, error) {
				v, err := obj(vars)
				if err != nil {
					return nil, err
				}
				return jGetattr(v, name)
			}
		case p.isOp("["):
			p.pos++
			key, err := p.ternary()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			obj := e
			e = func(vars map[string]any) (any, error) {
				v, err := obj(vars)
				if err != nil {
					return nil, err
				}
				k, err := key(vars)
				if err != nil {
					return nil, err
				}
				s, ok := k.(string)
				if !ok {
					return nil, fmt.Errorf("%w: non-string subscript in %q", errJinjaUnsupported, p.src)
				}
				return jGetattr(v, s)
			}
		case p.isOp("("):
			p.pos++
			args, err := p.args(")")
			if err != nil {
				return nil, err
			}
			fn := e
			e = func(vars map[string]any) (any, error) {
				v, err := fn(vars)
				if err != nil {
					return nil, err
				}
				m, ok := v.(jMethod)
				if !ok {
					return nil, fmt.Errorf("%w: call of %T in %q", errJinjaUnsupported, v, p.src)
				}
				if m.name != "get" || len(args) < 1 || len(args) > 2 {
					return nil, fmt.Errorf("%w: dict.%s with %d arguments in %q", errJinjaUnsupported, m.name, len(args), p.src)
				}
				k, err := args[0](vars)
				if err != nil {
					return nil, err
				}
				if s, ok := k.(string); ok {
					if found, ok := m.recv[s]; ok {
						return found, nil
					}
				}
				if len(args) == 2 {
					return args[1](vars)
				}
				return nil, nil
			}
		default:
			return e, nil
		}
	}
}

func (p *jParser) primary() (jExpr, error) {
	t := p.peek()
	p.pos++
	switch t.kind {
	case 's', 'd':
		v := t.val
		return func(map[string]any) (any, error) { return v, nil }, nil
	case 'n':
		switch t.text {
		case "none", "None":
			return func(map[string]any) (any, error) { return nil, nil }, nil
		case "true", "True":
			return func(map[string]any) (any, error) { return true, nil }, nil
		case "false", "False":
			return func(map[string]any) (any, error) { return false, nil }, nil
		case "if", "else", "and", "or", "not", "is", "in":
			return nil, fmt.Errorf("%w: keyword %q in %q", errJinjaUnsupported, t.text, p.src)
		}
		name := t.text
		return func(vars map[string]any) (any, error) {
			if v, ok := vars[name]; ok {
				return v, nil
			}
			return jUndefined{name}, nil
		}, nil
	case 'o':
		switch t.text {
		case "(":
			e, err := p.ternary()
			if err != nil {
				return nil, err
			}
			return e, p.expect(")")
		case "{":
			var keys, vals []jExpr
			for !p.isOp("}") {
				k, err := p.ternary()
				if err != nil {
					return nil, err
				}
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				v, err := p.ternary()
				if err != nil {
					return nil, err
				}
				keys, vals = append(keys, k), append(vals, v)
				if p.isOp(",") {
					p.pos++
				} else if !p.isOp("}") {
					return nil, fmt.Errorf("expected } in %q", p.src)
				}
			}
			p.pos++
			return func(vars map[string]any) (any, error) {
				m := make(map[string]any, len(keys))
				for i := range keys {
					k, err := keys[i](vars)
					if err != nil {
						return nil, err
					}
					s, ok := k.(string)
					if !ok {
						return nil, fmt.Errorf("%w: non-string dict key in %q", errJinjaUnsupported, p.src)
					}
					v, err := vals[i](vars)
					if err != nil {
						return nil, err
					}
					m[s] = v
				}
				return m, nil
			}, nil
		case "[":
			items, err := p.args("]")
			if err != nil {
				return nil, err
			}
			return func(vars map[string]any) (any, error) {
				out := make([]any, len(items))
				for i, it := range items {
					v, err := it(vars)
					if err != nil {
						return nil, err
					}
					out[i] = v
				}
				return out, nil
			}, nil
		}
	}
	return nil, fmt.Errorf("%w: token %q in %q", errJinjaUnsupported, t.text, p.src)
}

// --- Python value semantics --------------------------------------------------

// jGetattr is Jinja's environment.getattr: an attribute, else an item, else
// undefined — and an error on an undefined receiver.
func jGetattr(v any, name string) (any, error) {
	switch x := v.(type) {
	case jUndefined:
		return nil, fmt.Errorf("template error: %q is undefined (accessing .%s)", x.name, name)
	case map[string]any:
		switch name {
		case "get":
			return jMethod{recv: x, name: name}, nil
		case "items", "keys", "values", "pop", "update", "setdefault", "copy", "clear":
			return nil, fmt.Errorf("%w: dict.%s", errJinjaUnsupported, name)
		}
		if found, ok := x[name]; ok {
			return found, nil
		}
		return jUndefined{name}, nil
	case string:
		if slicesContains([]string{"lower", "upper", "strip", "split", "replace", "startswith", "endswith", "format", "join"}, name) {
			return nil, fmt.Errorf("%w: str.%s", errJinjaUnsupported, name)
		}
	}
	return jUndefined{name}, nil
}

// pyTruth is Python's bool(); an undefined logs a warning in Home Assistant.
func pyTruth(v any) (bool, error) {
	switch x := v.(type) {
	case nil:
		return false, nil
	case jUndefined:
		return false, fmt.Errorf("template warning: truth of undefined %q", x.name)
	case bool:
		return x, nil
	case int64:
		return x != 0, nil
	case float64:
		return x != 0, nil
	case string:
		return x != "", nil
	case map[string]any:
		return len(x) > 0, nil
	case []any:
		return len(x) > 0, nil
	}
	return false, fmt.Errorf("%w: truth of %T", errJinjaUnsupported, v)
}

// pyStr is Python's str(); rendering an undefined logs a warning in Home
// Assistant.
func pyStr(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "None", nil
	case jUndefined:
		return "", fmt.Errorf("template warning: rendering undefined %q", x.name)
	case bool:
		if x {
			return "True", nil
		}
		return "False", nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return pyFloatRepr(x), nil
	case string:
		return x, nil
	}
	return "", fmt.Errorf("%w: str() of %T", errJinjaUnsupported, v)
}

// pyFloatRepr is Python's repr(float): the shortest round-tripping digits,
// positional between 1e-4 and 1e16 with at least one decimal, else exponent.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if a := math.Abs(f); a != 0 && (a < 1e-4 || a >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyNumber is the int/float conversion of the filters: ok false when the
// value does not convert (the filter then takes its default), an error on an
// undefined (Undefined.__int__ raises in Jinja).
func pyNumber(v any, asInt bool) (n any, ok bool, err error) {
	var f float64
	switch x := v.(type) {
	case jUndefined:
		return nil, false, fmt.Errorf("template error: number of undefined %q", x.name)
	case bool:
		if x {
			f = 1
		}
	case int64:
		f = float64(x)
	case float64:
		f = x
	case string:
		s := strings.TrimSpace(x)
		if asInt {
			if i, perr := strconv.ParseInt(s, 10, 64); perr == nil {
				return i, true, nil
			}
		}
		if f, ok = parseFloatOK(s); !ok {
			return nil, false, nil
		}
	default:
		return nil, false, nil
	}
	if asInt {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false, nil
		}
		return int64(f), true, nil
	}
	return f, true, nil
}

// parseFloatOK is strconv.ParseFloat with the failure as a flag: a string
// that is no number makes the filter take its default, it is no error.
func parseFloatOK(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// pyNumeric reports v as a number when Python would compare it as one.
func pyNumeric(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func pyCompare(op string, a, b any) (any, error) {
	_, ua := a.(jUndefined)
	_, ub := b.(jUndefined)
	if op == "==" || op == "!=" {
		var eq bool
		switch {
		case ua || ub:
			eq = ua && ub // Undefined.__eq__: same type
		default:
			eq = pyEqual(a, b)
		}
		return eq == (op == "=="), nil
	}
	if ua || ub {
		return nil, errors.New("template error: ordering an undefined")
	}
	if x, ok := pyNumeric(a); ok {
		if y, ok := pyNumeric(b); ok {
			return orderResult(op, cmpFloat(x, y)), nil
		}
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return orderResult(op, strings.Compare(x, y)), nil
		}
	}
	return nil, fmt.Errorf("template error: %T %s %T is not supported", a, op, b)
}

func cmpFloat(x, y float64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

func orderResult(op string, c int) bool {
	switch op {
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	default:
		return c >= 0
	}
}

func pyEqual(a, b any) bool {
	if x, ok := pyNumeric(a); ok {
		y, ok := pyNumeric(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case map[string]any, []any:
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		return bytes.Equal(ja, jb)
	}
	return false
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// TestJinjaSubsetAgreesWithJinja pins the evaluator against what Jinja2 (as
// Home Assistant configures it) renders for the shapes that matter, including
// the ones that must fail — so the evaluator cannot drift lenient.
func TestJinjaSubsetAgreesWithJinja(t *testing.T) {
	t.Parallel()
	const guarded = `{% if value_json is defined and value_json.val is defined and value_json.val is not none and value_json.val != '' %}{{ value_json.val }}{% else %}None{% endif %}`
	for _, tc := range []struct {
		tmpl, payload, want string
		wantErr             bool
		unsupported         bool
	}{
		{tmpl: `{{ value_json.val }}`, payload: `{"val":21.5}`, want: "21.5"},
		{tmpl: `{{ value_json.val }}`, payload: `{"val":20.0}`, want: "20.0"},
		{tmpl: `{{ value_json.val }}`, payload: `{"val":20}`, want: "20"},
		{tmpl: `{{ value_json.val }}`, payload: `{"val":null}`, want: "None"},
		{tmpl: `{{ value_json.val }}`, payload: `{"val":true}`, want: "True"},
		{tmpl: `{{ value_json.val }}`, payload: ``, wantErr: true},        // UndefinedError: the 0.14.0 clear
		{tmpl: `{{ value_json.val }}`, payload: `{"x":1}`, wantErr: true}, // renders undefined: a WARNING
		{tmpl: `{{ value_json.val | lower }}`, payload: `{"val":false}`, want: "false"},
		{tmpl: `{{ value_json.val | lower }}`, payload: `{"val":"ON"}`, want: "on"},
		{tmpl: `{{ value_json.val | tojson }}`, payload: `{"val":{"data_source":"cloud"}}`, want: `{"data_source":"cloud"}`},
		{tmpl: `{{ 'online' if value | int(0) >= 2 else 'offline' }}`, payload: `2`, want: "online"},
		{tmpl: `{{ 'online' if value | int(0) >= 2 else 'offline' }}`, payload: `1`, want: "offline"},
		{tmpl: `{{ 'online' if value | int(0) >= 2 else 'offline' }}`, payload: `x`, want: "offline"},
		{tmpl: `{% set m = {'a': 'A'} %}{% if value_json is defined and value_json.val is not none %}{{ m.get(value_json.val, value_json.val) }}{% endif %}`, payload: `{"val":"a"}`, want: "A"},
		{tmpl: `{% set m = {'a': 'A'} %}{% if value_json is defined and value_json.val is not none %}{{ m.get(value_json.val, value_json.val) }}{% endif %}`, payload: `{"val":"b"}`, want: "b"},
		{tmpl: `{% set m = {'a': 'A'} %}{% if value_json is defined and value_json.val is not none %}{{ m.get(value_json.val, value_json.val) }}{% endif %}`, payload: ``, want: ""},
		{tmpl: `{% set m = {'A': 'a'} %}{{ m.get(value, value) }}`, payload: `A`, want: "a"},
		{tmpl: guarded, payload: ``, want: "None"},
		{tmpl: guarded, payload: `{"val":""}`, want: "None"},
		{tmpl: guarded, payload: `{"val":null}`, want: "None"},
		{tmpl: guarded, payload: `{"x":1}`, want: "None"},
		{tmpl: guarded, payload: `5`, want: "None"},
		{tmpl: guarded, payload: `{"val":0}`, want: "0"},
		{tmpl: guarded, payload: `{"val":"E3"}`, want: "E3"},
		{tmpl: `{% if value %}a{% elif value_json %}b{% else %}c{% endif %}`, payload: ``, wantErr: true}, // bool(undefined) warns
		{tmpl: `{{ value_json.val | float * 2 }}`, payload: `{"val":1}`, unsupported: true},
		{tmpl: `{{ value_json.val | round(1) }}`, payload: `{"val":1}`, unsupported: true},
		{tmpl: `{{ value_json.val is string }}`, payload: `{"val":1}`, unsupported: true},
		{tmpl: `{%- if value_json is defined -%}x{% endif %}`, payload: `{}`, unsupported: true},
		{tmpl: `{% for x in value_json %}{{ x }}{% endfor %}`, payload: `[]`, unsupported: true},
	} {
		got, err := renderJinja(tc.tmpl, tc.payload)
		switch {
		case tc.unsupported:
			if !errors.Is(err, errJinjaUnsupported) {
				t.Errorf("%s on %q: got (%q, %v), want an unsupported-construct error", tc.tmpl, tc.payload, got, err)
			}
		case tc.wantErr:
			if err == nil || errors.Is(err, errJinjaUnsupported) {
				t.Errorf("%s on %q: got (%q, %v), want a template error", tc.tmpl, tc.payload, got, err)
			}
		case err != nil || got != tc.want:
			t.Errorf("%s on %q: got (%q, %v), want %q", tc.tmpl, tc.payload, got, err, tc.want)
		}
	}
}
