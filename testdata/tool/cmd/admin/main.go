// Command admin is the cobra side of the CLI sample: a root command that only
// groups others, a subcommand built by a constructor (its RunE a closure), and
// a package-level one. Both send mail with the mailer library sample.
package main

import (
	"context"
	"database/sql"
	"os"

	"github.com/spf13/cobra"
	"github.com/zalgonoise/codegraph/testdata/mailer"
)

var purge = cobra.Command{Use: "purge [days]", RunE: runPurge}

func main() {
	root := &cobra.Command{Use: "admin"}
	root.AddCommand(newUsersCmd(), &purge)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func newUsersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "users",
		Short: "list the users",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := sql.Open("postgres", "postgres://admin")
			if err != nil {
				return err
			}
			rows, err := db.Query("SELECT id FROM users")
			if err != nil {
				return err
			}
			if err := mailer.New().Send(context.Background(), "ops", "users listed"); err != nil {
				return err
			}
			return rows.Close()
		},
	}
}

func runPurge(cmd *cobra.Command, args []string) error {
	db, err := sql.Open("postgres", "postgres://admin")
	if err != nil {
		return err
	}
	if _, err = db.Exec("DELETE FROM sessions WHERE expired"); err != nil {
		return err
	}
	return mailer.NewQueued(db).Send(context.Background(), "ops", "sessions purged")
}
