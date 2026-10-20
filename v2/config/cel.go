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

func compile(expr string) (cel.Program, error) {
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), ext.Strings())
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression yields %s, want bool", out)
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

// CompileReady compiles a ready expression as Load compiles primary.ready.
func CompileReady(expr string) (func(self map[string]any) (bool, error), error) {
	program, err := compile(expr)
	if err != nil {
		return nil, err
	}
	return func(self map[string]any) (bool, error) { return evalBool(program, self, expr) }, nil
}
