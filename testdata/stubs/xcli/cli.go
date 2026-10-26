// Package cli is a minimal offline stand-in for github.com/zalgonoise/x/cli/v2:
// a runner whose executors are a map of command names to functions, run by
// name like the real library does.
package cli

import (
	"context"
	"log/slog"
	"os"
)

type Executor interface {
	Exec(ctx context.Context, logger *slog.Logger, args []string) (int, error)
}

type Executable func(ctx context.Context, logger *slog.Logger, args []string) (int, error)

func (e Executable) Exec(ctx context.Context, logger *slog.Logger, args []string) (int, error) {
	return e(ctx, logger, args)
}

type Option func(*Runnable)

type Runnable struct {
	name      string
	executors map[string]Executor
}

func NewRunner(name string, opts ...Option) *Runnable {
	r := &Runnable{name: name}
	for _, o := range opts {
		o(r)
	}
	return r
}

func WithExecutors(executors map[string]Executor) Option {
	return func(r *Runnable) { r.executors = executors }
}

type Runner interface {
	Run(logger *slog.Logger) (int, error)
}

func (r *Runnable) Run(logger *slog.Logger) (int, error) {
	if len(os.Args) < 2 {
		return 1, nil
	}
	if e, ok := r.executors[os.Args[1]]; ok {
		return e.Exec(context.Background(), logger, os.Args[2:])
	}
	return 1, nil
}

func Run(runner Runner, logger *slog.Logger) {
	code, _ := runner.Run(logger)
	os.Exit(code)
}
