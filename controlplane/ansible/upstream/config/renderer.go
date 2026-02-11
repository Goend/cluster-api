package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// GroupResource identifies a Config-like CRD.
type GroupResource struct {
	Group    string
	Version  string
	Resource string
}

// Renderer loads Config CRs and renders templates with ${expr} placeholders.
type Renderer struct {
	dyn       dynamic.Interface
	mapper    meta.RESTMapper
	namespace string
	sources   []GroupResource
	values    map[string]interface{}
	execCtx   context.Context
}

// NewRenderer creates a renderer. When sources is empty, defaults to Config CRD.
func NewRenderer(dyn dynamic.Interface, mapper meta.RESTMapper, namespace string, sources []GroupResource) *Renderer {
	if len(sources) == 0 {
		sources = []GroupResource{{
			Group:    "controlplane.cluster.x-k8s.io",
			Version:  "v1alpha1",
			Resource: "configs",
		}}
	}
	return &Renderer{
		dyn:       dyn,
		mapper:    mapper,
		namespace: namespace,
		sources:   sources,
		values:    map[string]interface{}{},
	}
}

// Load fetches source CRs and merges their .data into the renderer context.

func (r *Renderer) Load(ctx context.Context) error {
	for _, src := range r.sources {
		gvr := schema.GroupVersionResource{Group: src.Group, Version: src.Version, Resource: src.Resource}
		list, err := r.listObjects(ctx, gvr, r.namespace)
		if err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return err
		}
		for _, item := range list.Items {
			data, _, _ := unstructured.NestedFieldNoCopy(item.Object, "data")
			if data == nil {
				continue
			}
			switch typed := data.(type) {
			case string:
				decoded := map[string]interface{}{}
				if err := yaml.Unmarshal([]byte(typed), &decoded); err == nil {
					r.values[item.GetName()] = decoded
				}
			case map[string]interface{}:
				r.values[item.GetName()] = typed
			}
		}
	}
	return nil
}

// SetValue injects an additional value accessible within templates.
func (r *Renderer) SetValue(name string, value interface{}) {
	if name == "" {
		return
	}
	r.values[name] = value
}

// Render parses the template as Go text/template and executes it.
func (r *Renderer) Render(ctx context.Context, tpl string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.execCtx = ctx
	defer func() {
		r.execCtx = nil
	}()
	goTpl, err := template.New("config").Option("missingkey=zero").Funcs(template.FuncMap{
		"eval": func(expr string) (interface{}, error) {
			return r.evaluate(expr)
		},
		"toYAML": func(value interface{}) (string, error) {
			return toYAML(value)
		},
		"indent": func(spaces int, text string) string {
			return indent(spaces, text)
		},
	}).Parse(tpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := goTpl.Execute(&buf, r.values); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (r *Renderer) evaluate(expr string) (interface{}, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", nil
	}
	if fn, args, rest, ok := parseFunction(expr); ok {
		val, err := r.callFunction(fn, args)
		if err != nil {
			return nil, err
		}
		if rest != "" {
			return traversePath(val, rest), nil
		}
		return val, nil
	}
	return r.evalPath(expr)
}

func (r *Renderer) evalPath(expr string) (interface{}, error) {
	switch {
	case isStringLiteral(expr):
		return trimQuotes(expr), nil
	default:
		if n, err := strconv.Atoi(expr); err == nil {
			return n, nil
		}
	}
	root, rest := splitRoot(expr)
	value := r.values[root]
	if rest == "" {
		return value, nil
	}
	return traversePath(value, rest), nil
}

func (r *Renderer) callFunction(name, args string) (interface{}, error) {
	parsedArgs, err := parseArgs(args)
	if err != nil {
		return nil, err
	}
	switch name {
	case "random_password":
		return randomPassword(parsedArgs)
	case "safe_render":
		return safeRender(r.values, parsedArgs)
	case "safe_render_multi":
		return safeRenderMulti(r.values, parsedArgs)
	case "resource":
		return r.fetchResource(parsedArgs)
	default:
		return nil, fmt.Errorf("unknown function %q", name)
	}
}

func randomPassword(args []interface{}) (interface{}, error) {
	length := 12
	if len(args) > 0 {
		if v, ok := args[0].(int); ok && v > 0 {
			length = v
		}
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func safeRender(values map[string]interface{}, args []interface{}) (interface{}, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("safe_render requires name and path")
	}
	name := fmt.Sprint(args[0])
	path := fmt.Sprint(args[1])
	var def interface{}
	if len(args) > 2 {
		def = args[2]
	}
	result := traversePath(values[name], path)
	if result == nil {
		return def, nil
	}
	return result, nil
}

func safeRenderMulti(values map[string]interface{}, args []interface{}) (interface{}, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("safe_render_multi requires names and path")
	}
	path := fmt.Sprint(args[1])
	var def interface{}
	if len(args) > 2 {
		def = args[2]
	}
	switch typed := args[0].(type) {
	case []interface{}:
		for _, v := range typed {
			if val := traversePath(values[fmt.Sprint(v)], path); val != nil {
				return val, nil
			}
		}
	default:
		if val := traversePath(values[fmt.Sprint(typed)], path); val != nil {
			return val, nil
		}
	}
	return def, nil
}

func traversePath(value interface{}, path string) interface{} {
	if value == nil {
		return nil
	}
	remaining := strings.TrimSpace(path)
	for remaining != "" {
		if remaining[0] == '/' {
			remaining = strings.TrimSpace(remaining[1:])
			continue
		}
		key, rest, ok := nextBracketSegment(remaining)
		if !ok {
			return nil
		}
		m, ok := value.(map[string]interface{})
		if !ok {
			return nil
		}
		value = m[key]
		remaining = rest
	}
	return value
}

func nextBracketSegment(path string) (string, string, bool) {
	if !strings.HasPrefix(path, "[") || len(path) < 4 {
		return "", path, false
	}
	quote := path[1]
	if quote != '\'' && quote != '"' {
		return "", path, false
	}
	remaining := path[2:]
	end := strings.IndexByte(remaining, quote)
	if end == -1 {
		return "", path, false
	}
	key := remaining[:end]
	remaining = remaining[end+1:]
	if len(remaining) == 0 || remaining[0] != ']' {
		return "", path, false
	}
	remaining = strings.TrimSpace(remaining[1:])
	return key, remaining, true
}

func parseFunction(expr string) (string, string, string, bool) {
	idx := strings.Index(expr, "(")
	if idx <= 0 {
		return "", "", "", false
	}
	name := strings.TrimSpace(expr[:idx])
	if name == "" {
		return "", "", "", false
	}
	body := expr[idx+1:]
	depth := 1
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				args := strings.TrimSpace(body[:i])
				rest := strings.TrimSpace(body[i+1:])
				return name, args, rest, true
			}
		}
	}
	return "", "", "", false
}

func parseArgs(raw string) ([]interface{}, error) {
	args := []interface{}{}
	current := strings.Builder{}
	depth := 0
	inString := rune(0)
	for _, r := range raw {
		switch {
		case inString != 0:
			current.WriteRune(r)
			if r == inString {
				inString = 0
			}
		case r == '\'' || r == '"':
			inString = r
			current.WriteRune(r)
		case r == '[':
			depth++
			current.WriteRune(r)
		case r == ']':
			depth--
			current.WriteRune(r)
		case r == ',' && depth == 0:
			if token := strings.TrimSpace(current.String()); token != "" {
				val, err := parseLiteral(token)
				if err != nil {
					return nil, err
				}
				args = append(args, val)
			}
			current.Reset()
		default:
			if !(r == ' ' && current.Len() == 0) {
				current.WriteRune(r)
			}
		}
	}
	if token := strings.TrimSpace(current.String()); token != "" {
		val, err := parseLiteral(token)
		if err != nil {
			return nil, err
		}
		args = append(args, val)
	}
	return args, nil
}

func parseLiteral(token string) (interface{}, error) {
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return "", nil
	case isStringLiteral(token):
		return trimQuotes(token), nil
	case strings.HasPrefix(token, "[") && strings.HasSuffix(token, "]"):
		return parseListLiteral(token)
	default:
		if n, err := strconv.Atoi(token); err == nil {
			return n, nil
		}
		return token, nil
	}
}

func parseListLiteral(token string) (interface{}, error) {
	body := strings.TrimSpace(token[1 : len(token)-1])
	if body == "" {
		return []interface{}{}, nil
	}
	items, err := parseArgs(body)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func isStringLiteral(token string) bool {
	return len(token) >= 2 && ((token[0] == '\'' && token[len(token)-1] == '\'') ||
		(token[0] == '"' && token[len(token)-1] == '"'))
}

func trimQuotes(token string) string {
	return token[1 : len(token)-1]
}

func splitRoot(expr string) (string, string) {
	idx := strings.Index(expr, "[")
	if idx == -1 {
		return strings.TrimSpace(expr), ""
	}
	return strings.TrimSpace(expr[:idx]), strings.TrimSpace(expr[idx:])
}

func truthy(val interface{}) bool {
	switch v := val.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int:
		return v != 0
	case float64:
		return v != 0
	default:
		return true
	}
}

func toYAML(value interface{}) (string, error) {
	if value == nil {
		return "", nil
	}
	data, err := yaml.Marshal(value)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}

func indent(spaces int, text string) string {
	if text == "" {
		return ""
	}
	prefix := ""
	if spaces > 0 {
		prefix = strings.Repeat(" ", spaces)
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func (r *Renderer) fetchResource(args []interface{}) (interface{}, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("resource requires at least group, version, resource")
	}
	group := fmt.Sprint(args[0])
	version := fmt.Sprint(args[1])
	resource := fmt.Sprint(args[2])
	name := ""
	hasName := false
	if len(args) > 3 {
		name = fmt.Sprint(args[3])
		hasName = name != ""
	}
	namespace := r.namespace
	if len(args) > 4 {
		namespace = fmt.Sprint(args[4])
	}
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	ctx := r.currentCtx()
	if hasName {
		obj, err := r.getObject(ctx, gvr, name, namespace)
		if err != nil {
			return nil, err
		}
		return obj.Object, nil
	}
	list, err := r.listObjects(ctx, gvr, namespace)
	if err != nil {
		return nil, err
	}
	return list.UnstructuredContent(), nil
}

func (r *Renderer) currentCtx() context.Context {
	if r.execCtx != nil {
		return r.execCtx
	}
	return context.Background()
}

func (r *Renderer) listObjects(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (*unstructured.UnstructuredList, error) {
	mapping := r.restMapping(gvr)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := namespace
		if ns == "" {
			return nil, fmt.Errorf("namespace required for resource %s", gvr.Resource)
		}
		return r.dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	}
	return r.dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
}

func (r *Renderer) getObject(ctx context.Context, gvr schema.GroupVersionResource, name, namespace string) (*unstructured.Unstructured, error) {
	mapping := r.restMapping(gvr)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := namespace
		if ns == "" {
			return nil, fmt.Errorf("namespace required for resource %s", gvr.Resource)
		}
		return r.dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}
	return r.dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
}

func (r *Renderer) restMapping(gvr schema.GroupVersionResource) *meta.RESTMapping {
	if r.mapper == nil {
		return &meta.RESTMapping{Scope: meta.RESTScopeRoot}
	}
	gvk, err := r.mapper.KindFor(gvr)
	if err != nil {
		gvk = schema.GroupVersion{Group: gvr.Group, Version: gvr.Version}.WithKind(strings.Title(strings.TrimSuffix(gvr.Resource, "s")))
	}
	mapping, err := r.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return &meta.RESTMapping{Scope: meta.RESTScopeRoot}
	}
	return mapping
}
