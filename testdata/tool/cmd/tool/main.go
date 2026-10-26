// Command tool is the CLI sample: main hands a map of command names to
// functions to a CLI library (x/cli v2), which runs the one named on the
// command line. Each command is an entry point with a journey of its own. Its
// configuration is read by kelseyhightower/envconfig.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/zalgonoise/x/cli/v2"
)

// config is filled by envconfig.Process("tool", &c): tagged fields by their
// tag, untagged ones by TOOL_ and their name.
type config struct {
	DSN      string    `envconfig:"DSN" default:"postgres://tool"`
	Table    string    `envconfig:"MIGRATIONS_TABLE" default:"schema_migrations"`
	LogLevel string    `split_words:"true"` // TOOL_LOG_LEVEL
	Started  time.Time // decodes itself: one setting, TOOL_STARTED
	Debug    bool      `ignored:"true"`
	HTTP     struct {
		Port int // TOOL_HTTP_PORT
	}
}

func load() (config, error) {
	var c config
	err := envconfig.Process("tool", &c)
	return c, err
}

func main() {
	logger := slog.Default()
	runner := cli.NewRunner("tool", cli.WithExecutors(map[string]cli.Executor{
		"serve":   cli.Executable(serve),
		"migrate": cli.Executable(migrate),
	}))
	cli.Run(runner, logger)
}

// serve opens the database and serves the status route.
func serve(ctx context.Context, logger *slog.Logger, _ []string) (int, error) {
	db, err := sql.Open("postgres", "postgres://tool")
	if err != nil {
		return 1, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	logger.InfoContext(ctx, "serving")
	return 0, http.ListenAndServe(":8095", mux)
}

// migrate records the schema version, in the table its configuration names
// (by default, nothing in the code sets it).
func migrate(ctx context.Context, _ *slog.Logger, _ []string) (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	db, err := sql.Open("postgres", cfg.DSN)
	if err != nil {
		return 1, err
	}
	_, err = db.ExecContext(ctx, "INSERT INTO "+cfg.Table+" (version) VALUES (1)")
	return 0, err
}
