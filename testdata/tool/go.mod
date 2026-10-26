module github.com/zalgonoise/codegraph/testdata/tool

go 1.27

require (
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/spf13/cobra v1.8.0
	github.com/zalgonoise/codegraph/testdata/mailer v0.0.0
	github.com/zalgonoise/x/cli/v2 v2.0.0
)

replace (
	github.com/kelseyhightower/envconfig => ../stubs/envconfig
	github.com/spf13/cobra => ../stubs/cobra
	github.com/zalgonoise/codegraph/testdata/mailer => ../mailer
	github.com/zalgonoise/x/cli/v2 => ../stubs/xcli
)
