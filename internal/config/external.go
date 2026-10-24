package config

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/functions"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"

	"sigs.k8s.io/yaml"
)

// When says when a property is judged.
type When string

const (
	// StepEnd judges a property at each settled step's end.
	StepEnd When = "step"
	// Always judges a property after every recorded version.
	Always When = "always"
)

// Property is one properties entry: CEL over every system's resources
// that must hold. DESIGN.md §12 names its variables and functions.
type Property struct {
	Name, Expr string
	When       When
}

func properties(d declaration) ([]Property, error) {
	var properties []Property
	for i, declared := range d.Properties {
		key := fmt.Sprintf("properties[%d]", i)
		p := Property{Name: declared.Name, Expr: declared.Expr, When: When(declared.When)}
		switch {
		case p.Name == "":
			return nil, fmt.Errorf("%s.name is required", key)
		case slices.ContainsFunc(properties, func(other Property) bool { return other.Name == p.Name }):
			return nil, fmt.Errorf("%s.name %s: another property has this name", key, p.Name)
		case p.Expr == "":
			return nil, fmt.Errorf("%s.expr is required", key)
		case p.When == "":
			p.When = StepEnd
		case p.When != StepEnd && p.When != Always:
			return nil, fmt.Errorf("%s.when %q: want %s or %s", key, declared.When, StepEnd, Always)
		}
		if _, err := CompileProperty(p.Expr); err != nil {
			return nil, fmt.Errorf("%s.expr: %w", key, err)
		}
		properties = append(properties, p)
	}
	return properties, nil
}

// FirstWrittenUnder implements a property's firstWrittenUnder(object, path).
type FirstWrittenUnder func(object map[string]any, path []string) (string, error)

// PropertyProgram is a compiled property.
type PropertyProgram struct {
	env  *cel.Env
	ast  *cel.Ast
	expr string
}

const firstWrittenUnderOverload = "firstWrittenUnder_dyn_list_string"

// CompileProperty compiles a property's CEL over crs and world.
func CompileProperty(expr string) (*PropertyProgram, error) {
	env, err := cel.NewEnv(
		cel.Variable("crs", cel.ListType(cel.DynType)),
		cel.Variable("world", cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.ListType(cel.DynType)))),
		cel.Function("firstWrittenUnder", cel.Overload(firstWrittenUnderOverload,
			[]*cel.Type{cel.DynType, cel.ListType(cel.StringType)}, cel.StringType)),
		ext.Strings(), ext.Encoders())
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression yields %s, want %s", out, cel.BoolType)
	}
	return &PropertyProgram{env: env, ast: ast, expr: expr}, nil
}

// Bind returns the property's evaluator, with firstWrittenUnder bound to f.
func (p *PropertyProgram) Bind(f FirstWrittenUnder) (func(crs []any, world map[string]map[string][]any) (bool, error), error) {
	program, err := p.env.Program(p.ast, cel.Functions(&functions.Overload{
		Operator: firstWrittenUnderOverload,
		Binary: func(object, path ref.Val) ref.Val {
			native, err := object.ConvertToNative(reflect.TypeFor[map[string]any]())
			if err != nil {
				return types.WrapErr(fmt.Errorf("firstWrittenUnder: the object is not a map: %w", err))
			}
			keys, err := path.ConvertToNative(reflect.TypeFor[[]string]())
			if err != nil {
				return types.WrapErr(fmt.Errorf("firstWrittenUnder: the path is not a list of strings: %w", err))
			}
			uid, err := f(native.(map[string]any), keys.([]string))
			if err != nil {
				return types.WrapErr(fmt.Errorf("firstWrittenUnder: %w", err))
			}
			return types.String(uid)
		},
	}))
	if err != nil {
		return nil, err
	}
	return func(crs []any, world map[string]map[string][]any) (bool, error) {
		out, _, err := program.Eval(map[string]any{"crs": crs, "world": world})
		if err != nil {
			return false, fmt.Errorf("evaluating %q: %w", p.expr, err)
		}
		value, isBool := out.Value().(bool)
		if !isBool {
			return false, fmt.Errorf("evaluating %q: %w: it yielded %T", p.expr, ErrNotBool, out.Value())
		}
		return value, nil
	}, nil
}

func periodic(primary string, declared *declaredPeriodic) (*Periodic, error) {
	if declared == nil {
		return nil, nil
	}
	every, err := time.ParseDuration(declared.Every)
	if err != nil || every <= 0 {
		return nil, fmt.Errorf("%s.periodic.every %q: want a positive duration", primary, declared.Every)
	}
	if len(declared.Paths) == 0 {
		return nil, fmt.Errorf("%s.periodic.paths is required", primary)
	}
	return &Periodic{Every: every, Paths: declared.Paths}, nil
}

// SystemFixture is one entry of a system's fixtures file: a resource the
// harness creates through the system's API map.
type SystemFixture struct {
	Resource string         `json:"resource"`
	Name     string         `json:"name"`
	Body     map[string]any `json:"body"`
}

// ReadSystemFixtures reads a YAML list of SystemFixture strictly.
func ReadSystemFixtures(path string) ([]SystemFixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fixtures []SystemFixture
	if err := yaml.UnmarshalStrict(data, &fixtures); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, f := range fixtures {
		switch {
		case f.Resource == "":
			return nil, fmt.Errorf("%s: [%d].resource is required", path, i)
		case f.Name == "":
			return nil, fmt.Errorf("%s: [%d].name is required", path, i)
		}
	}
	return fixtures, nil
}
