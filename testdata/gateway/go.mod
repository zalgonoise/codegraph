module github.com/zalgonoise/codegraph/testdata/gateway

go 1.27

require (
	github.com/go-chi/chi/v5 v5.0.0
	github.com/kataras/iris/v12 v12.2.0
)

replace (
	github.com/go-chi/chi/v5 => ../stubs/chi
	github.com/kataras/iris/v12 => ../stubs/iris
)
