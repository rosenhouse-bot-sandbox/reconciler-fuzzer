package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

// ErrNotBool marks an expression whose result is not a bool. That is a
// configuration error, never "not ready".
var ErrNotBool = errors.New("the expression did not yield a bool")

// ErrNotString marks an expression whose result is not a string.
var ErrNotString = errors.New("the expression did not yield a string")

// library is what each expression may use beyond self. As in the API
// server, numbers of different types compare.
var library = []cel.EnvOption{ext.Strings(), cel.CrossTypeNumericComparisons(true)}

func compileTo(expr string, want *cel.Type) (cel.Program, error) {
	env, err := cel.NewEnv(append([]cel.EnvOption{cel.Variable("self", cel.DynType)}, library...)...)
	if err != nil {
		return nil, err
	}
	ast, err := checked(env, expr, want)
	if err != nil {
		return nil, err
	}
	return env.Program(ast)
}

// checked compiles expr in env and refuses it unless it yields want or dyn.
func checked(env *cel.Env, expr string, want *cel.Type) (*cel.Ast, error) {
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if out := ast.OutputType(); !out.IsExactType(want) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression yields %s, want %s", out, want)
	}
	return ast, nil
}

// typeCheck refuses expr unless it yields want with self typed by root, an
// openAPIV3Schema, as the API server checks x-kubernetes-validations rules.
// It types self as the harness decodes it from JSON, with any metadata and
// a string in any format. A nil root types self as dyn.
func typeCheck(expr string, root map[string]any, want *cel.Type) error {
	self := declType(root, true).MaybeAssignTypeName("selfType")
	provider := apiservercel.NewDeclTypeProvider(self)
	base, err := cel.NewEnv(library...)
	if err != nil {
		return err
	}
	options, err := provider.EnvOptions(base.CELTypeProvider())
	if err != nil {
		return err
	}
	env, err := base.Extend(append(options, cel.Variable("self", self.CelType()))...)
	if err != nil {
		return err
	}
	_, err = checked(env, expr, want)
	return err
}

// declType types a value that s, an OpenAPI v3 schema, describes. A
// resource has apiVersion, kind and any metadata. Any value fits where s
// keeps unknown fields, embeds a resource or names no type. An object with
// a property that CEL cannot select is a map, which CEL can index.
func declType(s map[string]any, resource bool) *apiservercel.DeclType {
	if s["x-kubernetes-preserve-unknown-fields"] == true || s["x-kubernetes-embedded-resource"] == true {
		return apiservercel.DynType
	}
	switch s["type"] {
	case "object":
		if values, isSchema := s["additionalProperties"].(map[string]any); isSchema {
			return apiservercel.NewMapType(apiservercel.StringType, declType(values, false), -1)
		}
		properties, _ := s["properties"].(map[string]any)
		fields := map[string]*apiservercel.DeclField{}
		for name, property := range properties {
			if !selectable(name) {
				return apiservercel.NewMapType(apiservercel.StringType, apiservercel.DynType, -1)
			}
			property, _ := property.(map[string]any)
			fields[name] = apiservercel.NewDeclField(name, declType(property, false), false, nil, nil)
		}
		if resource {
			for name, t := range map[string]*apiservercel.DeclType{
				"apiVersion": apiservercel.StringType, "kind": apiservercel.StringType, "metadata": apiservercel.DynType,
			} {
				fields[name] = apiservercel.NewDeclField(name, t, false, nil, nil)
			}
		}
		return apiservercel.NewObjectType("object", fields)
	case "array":
		items, _ := s["items"].(map[string]any)
		return apiservercel.NewListType(declType(items, false), -1)
	case "string":
		return apiservercel.StringType
	case "integer":
		return apiservercel.IntType
	case "number":
		return apiservercel.DoubleType
	case "boolean":
		return apiservercel.BoolType
	}
	return apiservercel.DynType
}

var identifier = regexp.MustCompile(`^[_a-zA-Z][_a-zA-Z0-9]*$`)

// selectable reports whether CEL can select a field by name, as in
// self.spec.name. CEL reads another field only by index, as in
// self.spec['app.kubernetes.io/name'].
func selectable(name string) bool {
	return identifier.MatchString(name) && !slices.Contains([]string{"true", "false", "null", "in"}, name)
}

// typed converts v, decoded by encoding/json, to the CEL types t declares,
// as the API server converts a CR: a whole number is an int unless t types
// it double.
func typed(v any, t *apiservercel.DeclType) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for name, value := range v {
			field := apiservercel.DynType
			if f, found := t.FindField(name); found {
				field = f.Type
			} else if t.IsMap() {
				field = t.ElemType
			}
			out[name] = typed(value, field)
		}
		return out
	case []any:
		item := apiservercel.DynType
		if t.IsList() {
			item = t.ElemType
		}
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = typed(value, item)
		}
		return out
	case float64:
		if t != apiservercel.DoubleType && v == math.Trunc(v) && v >= math.MinInt64 && v < math.MaxInt64 {
			return int64(v)
		}
	}
	return v
}

// compileOver compiles expr, CEL over self that yields want, with self
// typed by root as typeCheck types it. It returns that type.
func compileOver(expr string, root map[string]any, want *cel.Type) (cel.Program, *apiservercel.DeclType, error) {
	if err := typeCheck(expr, root, want); err != nil {
		return nil, nil, err
	}
	program, err := compileTo(expr, want)
	return program, declType(root, true), err
}

// CompileReady compiles expr, CEL over self that yields a bool, with self
// typed by root, as typeCheck types it. The result reads self through typed.
func CompileReady(expr string, root map[string]any) (func(self map[string]any) (bool, error), error) {
	program, self, err := compileOver(expr, root, cel.BoolType)
	if err != nil {
		return nil, err
	}
	return func(obj map[string]any) (bool, error) {
		out, _, err := program.Eval(map[string]any{"self": typed(obj, self)})
		if err != nil {
			return false, fmt.Errorf("evaluating %q: %w", expr, err)
		}
		value, isBool := out.Value().(bool)
		if !isBool {
			return false, fmt.Errorf("evaluating %q: %w: it yielded %T", expr, ErrNotBool, out.Value())
		}
		return value, nil
	}, nil
}

// CompileString compiles expr, CEL over self that yields a string, such as
// an owns name or a systems owner, as CompileReady compiles a bool.
func CompileString(expr string, root map[string]any) (func(self map[string]any) (string, error), error) {
	program, self, err := compileOver(expr, root, cel.StringType)
	if err != nil {
		return nil, err
	}
	return func(obj map[string]any) (string, error) {
		out, _, err := program.Eval(map[string]any{"self": typed(obj, self)})
		if err != nil {
			return "", fmt.Errorf("evaluating %q: %w", expr, err)
		}
		value, isString := out.Value().(string)
		if !isString {
			return "", fmt.Errorf("evaluating %q: %w: it yielded %T", expr, ErrNotString, out.Value())
		}
		return value, nil
	}, nil
}
