package main

import (
	"os/exec"
	"reflect"
	"strings"

	"github.com/hashicorp/hil"
	"github.com/hashicorp/hil/ast"
	"github.com/hashicorp/terraform/config"
	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/pkg/workspace"
	"github.com/pulumi/pulumi/pkg/util/contract"
	"github.com/pulumi/pulumi-terraform/pkg/tfbridge"
	"github.com/ugorji/go/codec"
)

// todo: explicit dependencies

type node interface {
	dependencies() []node
}

type graph struct {
	providers []*providerNode
	resources []*resourceNode
	outputs []*outputNode
	locals []*localNode
	variables []*variableNode
}

type providerNode struct {
	config *config.ProviderConfig
	deps []node
	properties map[string]interface{}
	info       *tfbridge.ProviderInfo
}

type resourceNode struct {
	config *config.Resource
	provider *providerNode
	deps []node
	explicitDeps []node
	properties map[string]interface{}
}

type outputNode struct {
	config *config.Output
	deps []node
	explicitDeps []node
	value interface{}
}

type localNode struct {
	config *config.Local
	deps []node
	properties map[string]interface{}
}

type variableNode struct {
	config *config.Variable
	defaultValue interface{}
}

func (p *providerNode) dependencies() []node {
	return p.deps
}

func (r *resourceNode) dependencies() []node {
	return r.deps
}

func (o *outputNode) dependencies() []node {
	return o.deps
}

func (l *localNode) dependencies() []node {
	return l.deps
}

func (v *variableNode) dependencies() []node {
	return nil
}

type builder struct {
	providers map[string]*providerNode
	resources map[string]*resourceNode
	outputs   map[string]*outputNode
	locals    map[string]*localNode
	variables map[string]*variableNode
}

func newBuilder() *builder {
	return &builder{
		providers: make(map[string]*providerNode),
		resources: make(map[string]*resourceNode),
		outputs:   make(map[string]*outputNode),
		locals:    make(map[string]*localNode),
		variables: make(map[string]*variableNode),
	}
}

func (b *builder) getNode(name string) (node, bool) {
	if p, ok := b.providers[name]; ok {
		return p, true
	}
	if r, ok := b.resources[name]; ok {
		return r, true
	}
	if o, ok := b.outputs[name]; ok {
		return o, true
	}
	if l, ok := b.locals[name]; ok {
		return l, true
	}
	if v, ok := b.variables[name]; ok {
		return v, true
	}
	return nil, false
}

type propertyWalker struct {
	deps map[string]struct{}
}

func (w *propertyWalker) walkPrimitive(p reflect.Value) (interface{}, error) {
	switch p.Kind() {
	case reflect.Bool, reflect.Int, reflect.Float64:
		// return these as-is
		return p.Interface(), nil

	case reflect.String:
		// attempt to parse the string as HIL. If the result is a simple literal, return that. Otherwise, keep the HIL
		// itself.
		rootNode, err := hil.Parse(p.String())
		if err != nil {
			return nil, err
		}
		contract.Assert(rootNode != nil)

		if lit, ok := rootNode.(*ast.LiteralNode); ok && lit.Typex == ast.TypeString {
			return lit.Value, nil
		}

		rootNode.Accept(func(n ast.Node) ast.Node {
			if v, ok := n.(*ast.VariableAccess); ok {
				w.deps[v.Name] = struct{}{}
			}
			return n
		})
		if err != nil {
			return nil, err
		}
		return rootNode, nil

	default:
		// walk should have ensured we never reach this point
		contract.Failf("unexpeted property type %v", p.Type())
		return nil, errors.New("unexpected property type")
	}
}

func (w *propertyWalker) walkSlice(s reflect.Value) ([]interface{}, error) {
	contract.Require(s.Kind() == reflect.Slice, "s")

	// iterate over slice elements
	result := make([]interface{}, s.Len())
	for i := 0; i < s.Len(); i++ {
		v, err := w.walk(s.Index(i))
		if err != nil {
			return nil, err
		}
		result[i] = v
	}
	return result, nil
}

func (w *propertyWalker) walkMap(m reflect.Value) (map[string]interface{}, error) {
	contract.Require(m.Kind() == reflect.Map, "m")

	// grab the key type and ensure it is of type string
	if m.Type().Key().Kind() != reflect.String {
		return nil, errors.Errorf("unexpected key type %v", m.Type().Key())
	}

	// iterate over the map elements
	result := make(map[string]interface{})
	for _, k := range m.MapKeys() {
		v, err := w.walk(m.MapIndex(k))
		if err != nil {
			return nil, err
		}
		result[k.String()] = v
	}
	return result, nil
}

func (w *propertyWalker) walk(v reflect.Value) (interface{}, error) {
	if v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Bool, reflect.Int, reflect.Float64, reflect.String:
		return w.walkPrimitive(v)
	case reflect.Slice:
		return w.walkSlice(v)
	case reflect.Map:
		return w.walkMap(v)
	default:
		return nil, errors.Errorf("unexpected property type %v", v.Type())
	}
}

func (b *builder) buildValue(v interface{}) (interface{}, map[node]struct{}, error) {
	if v == nil {
		return nil, nil, nil
	}

	// Walk the value, perform any necessary conversions (HIL parsing in particular), and collect dependency
	// information.
	walker := &propertyWalker{
		deps: make(map[string]struct{}),
	}
	prop, err := walker.walk(reflect.ValueOf(v))
	if err != nil {
		return nil, nil, err
	}

	// Walk the collected dependencies and convert them to `node`s
	deps := make(map[node]struct{})
	for k, _ := range walker.deps {
		tfVar, err := config.NewInterpolatedVariable(k)
		if err != nil {
			return nil, nil, err
		}

		switch v := tfVar.(type) {
		case *config.CountVariable:
		case *config.PathVariable:
		case *config.SelfVariable:
		case *config.SimpleVariable:
		case *config.TerraformVariable:
			// nothing to do

		case *config.ModuleVariable:
			// unsupported
			return nil, nil, errors.Errorf("module variable references are not yet supported (%v)", v.Name)

		case *config.LocalVariable:
			l, ok := b.locals[v.Name]
			if !ok {
				return nil, nil, errors.Errorf("unknown local variable %v", v.Name)
			}
			deps[l] = struct{}{}
		case *config.ResourceVariable:
			r, ok := b.resources[v.ResourceId()]
			if !ok {
				return nil, nil, errors.Errorf("unknown resource %v", v.Name)
			}
			deps[r] = struct{}{}
		case *config.UserVariable:
			u, ok := b.variables[v.Name]
			if !ok {
				return nil, nil, errors.Errorf("unknown variable %v", v.Name)
			}
			deps[u] = struct{}{}
		}
	}

	return prop, deps, nil
}

func (b *builder) buildProperties(raw *config.RawConfig) (map[string]interface{}, map[node]struct{}, error) {
	v, deps, err := b.buildValue(raw.Raw)
	if err != nil {
		return nil, nil, err
	}
	return v.(map[string]interface{}), deps, nil
}

func (b *builder) buildDeps(deps map[node]struct{}, dependsOn []string) ([]node, []node, error) {
	explicitDeps := make([]node, len(dependsOn))
	for i, name := range dependsOn {
		if strings.HasPrefix(name, "module.") {
			return nil, nil, errors.Errorf("module references are not yet supported (%v)", name)
		}
		r, ok := b.resources[name]
		if !ok {
			return nil, nil, errors.Errorf("unknown resource %v", name)
		}
		deps[r], explicitDeps[i] = struct{}{}, r
	}

	allDeps := make([]node, 0, len(deps))
	for n, _ := range deps {
		allDeps = append(allDeps, n)
	}

	return allDeps, explicitDeps, nil
}

func getProviderInfo(p *providerNode) (*tfbridge.ProviderInfo, error) {
	_, path, err := workspace.GetPluginPath(workspace.ResourcePlugin, p.config.Name, nil)
	if err != nil {
		return nil, err
	} else if path == "" {
		return nil, errors.Errorf("could not find plugin for provider %s", p.config.Name)
	}

	// Run the plugin and decode its provider config.
	cmd := exec.Command(path, "-get-provider-info")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var info tfbridge.ProviderInfo
	err = codec.NewDecoder(out, new(codec.JsonHandle)).Decode(&info)

	if cErr := cmd.Wait(); cErr != nil {
		return nil, cErr
	}
	if err != nil {
		return nil, err
	}

	return &info, nil
}

func (b *builder) buildProvider(p *providerNode) error {
	info, err := getProviderInfo(p)
	if err != nil {
		return err
	}
	p.info = info

	props, deps, err := b.buildProperties(p.config.RawConfig)
	if err != nil {
		return err
	}
	allDeps, _, err := b.buildDeps(deps, nil)
	contract.Assert(err == nil)

	p.properties, p.deps = props, allDeps
	return nil
}

func (b *builder) buildResource(r *resourceNode) error {
	providerName := r.config.ProviderFullName()
	p, ok := b.providers[providerName]
	if !ok {
		return errors.Errorf("could not find provider for resource %s", r.config.Id())
	}
	r.provider = p

	props, deps, err := b.buildProperties(r.config.RawConfig)
	if err != nil {
		return err
	}
	allDeps, explicitDeps, err := b.buildDeps(deps, r.config.DependsOn)
	if err != nil {
		return err
	}
	r.properties, r.deps, r.explicitDeps = props, allDeps, explicitDeps
	return nil
}

func (b *builder) buildOutput(o *outputNode) error {
	props, deps, err := b.buildProperties(o.config.RawConfig)
	if err != nil {
		return err
	}
	allDeps, explicitDeps, err := b.buildDeps(deps, o.config.DependsOn)
	if err != nil {
		return err
	}

	// In general, an output should have a single property named "value". If this is the case, promote it to the
	// output's value.
	value := (interface{})(props)
	if len(props) == 1 {
		if v, ok := props["value"]; ok {
			value = v
		}
	}

	o.value, o.deps, o.explicitDeps = value, allDeps, explicitDeps
	return nil
}

func (b *builder) buildLocal(l *localNode) error {
	props, deps, err := b.buildProperties(l.config.RawConfig)
	if err != nil {
		return err
	}
	allDeps, _, err := b.buildDeps(deps, nil)
	contract.Assert(err == nil)

	l.properties, l.deps = props, allDeps
	return nil
}

func (b *builder) buildVariable(v *variableNode) error {
	defaultValue, deps, err := b.buildValue(v.config.Default)
	if err != nil {
		return err
	}
	if len(deps) != 0 {
		return errors.Errorf("variables may not depend on other nodes (%v)", v.config.Name)
	}
	v.defaultValue = defaultValue
	return nil
}

func buildGraph(conf *config.Config) (*graph, error) {
	b := newBuilder()

	// First create our nodes.
	for _, p := range conf.ProviderConfigs {
		b.providers[p.Name] = &providerNode{config: p}
	}
	for _, r := range conf.Resources {
		b.resources[r.Id()] = &resourceNode{config: r}
	}
	for _, o := range conf.Outputs {
		b.outputs[o.Name] = &outputNode{config: o}
	}
	for _, l := range conf.Locals {
		b.locals[l.Name] = &localNode{config: l}
	}
	for _, v := range conf.Variables {
		b.variables[v.Name] = &variableNode{config: v}
	}

	// Now translate each node's properties and connect any dependency edges.
	for _, p := range b.providers {
		if err := b.buildProvider(p); err != nil {
			return nil, err
		}
	}
	for _, r := range b.resources {
		if err := b.buildResource(r); err != nil {
			return nil, err
		}
	}
	for _, o := range b.outputs {
		if err := b.buildOutput(o); err != nil {
			return nil, err
		}
		// outputs are sinks; we always deal with them last
	}
	for _, l := range b.locals {
		if err := b.buildLocal(l); err != nil {
			return nil, err
		}
	}
	for _, v := range b.variables {
		if err := b.buildVariable(v); err != nil {
			return nil, err
		}
		// variables are sources; we always deal with them before other nodes.
	}

	// put the graph together
	providers := make([]*providerNode, 0, len(b.providers))
	for _, p := range b.providers {
		providers = append(providers, p)
	}
	resources := make([]*resourceNode, 0, len(b.resources))
	for _, r := range b.resources {
		resources = append(resources, r)
	}
	outputs := make([]*outputNode, 0, len(b.outputs))
	for _, o := range b.outputs {
		outputs = append(outputs, o)
	}
	locals := make([]*localNode, 0, len(b.locals))
	for _, l := range b.locals {
		locals = append(locals, l)
	}
	variables := make([]*variableNode, 0, len(b.variables))
	for _, v := range b.variables {
		variables = append(variables, v)
	}
	return &graph{
		providers: providers,
		resources: resources,
		outputs: outputs,
		locals: locals,
		variables: variables,
	}, nil
}