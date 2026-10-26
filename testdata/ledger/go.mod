module github.com/zalgonoise/codegraph/testdata/ledger

go 1.27

require (
	example.com/inhouse/kvcache v1.2.0
	example.com/inhouse/kafkabus/v2 v2.0.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.27.0
	google.golang.org/grpc v0.0.0
)

replace (
	example.com/inhouse/kvcache => ../stubs/kvcache
	example.com/inhouse/kafkabus/v2 => ../stubs/kafkabus
	github.com/grpc-ecosystem/grpc-gateway/v2 => ../stubs/gateway
	google.golang.org/grpc => ../stubs/grpc
)
