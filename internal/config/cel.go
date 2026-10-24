package config

import (
	"errors"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// ErrNotBool marks an expression whose result is not a bool. That is a
// configuration error, never "not ready".
var ErrNotBool = errors.New("the expression did not yield a bool")

// ErrNotString marks an expression whose result is not a string.
var ErrNotString = errors.New("the expression did not yield a string")

func compile(expr string) (cel.Program, error) { return compileTo(expr, cel.BoolType) }

func compileTo(expr string, want *cel.Type) (cel.Program, error) {
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), ext.Strings())
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if out := ast.OutputType(); !out.IsExactType(want) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression yields %s, want %s", out, want)
	}
	return env.Program(ast)
}

func evalBool(program cel.Program, self map[string]any, expr string) (bool, error) {
	out, _, err := program.Eval(map[string]any{"self": self})
	if err != nil {
		return false, fmt.Errorf("evaluating %q: %w", expr, err)
	}
	value, isBool := out.Value().(bool)
	if !isBool {
		return false, fmt.Errorf("evaluating %q: %w: it yielded %T", expr, ErrNotBool, out.Value())
	}
	return value, nil
}

// CompileReady compiles a ready expression as Load compiles an entry's ready.
func CompileReady(expr string) (func(self map[string]any) (bool, error), error) {
	program, err := compile(expr)
	if err != nil {
		return nil, err
	}
	return func(self map[string]any) (bool, error) { return evalBool(program, self, expr) }, nil
}

// CompileString compiles an expression that yields a string, such as an
// owns name or a systems owner.
func CompileString(expr string) (func(self map[string]any) (string, error), error) {
	program, err := compileTo(expr, cel.StringType)
	if err != nil {
		return nil, err
	}
	return func(self map[string]any) (string, error) {
		out, _, err := program.Eval(map[string]any{"self": self})
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
