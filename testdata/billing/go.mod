module github.com/zalgonoise/codegraph/testdata/billing

go 1.27

require (
	example.com/inhouse/kvcache v1.2.0
	example.com/inhouse/kafkabus/v2 v2.0.0
	example.com/inhouse/ledger-sdk-go v0.1.0
	example.com/inhouse/rabbitbus v0.0.0
	google.golang.org/grpc v0.0.0
	gorm.io/driver/mysql v1.5.0
	gorm.io/gorm v1.25.0
)

replace (
	example.com/inhouse/kvcache => ../stubs/kvcache
	example.com/inhouse/kafkabus/v2 => ../stubs/kafkabus
	example.com/inhouse/ledger-sdk-go => ../stubs/ledger-sdk
	example.com/inhouse/rabbitbus => ../stubs/rabbitbus
	google.golang.org/grpc => ../stubs/grpc
	gorm.io/driver/mysql => ../stubs/gorm-mysql
	gorm.io/gorm => ../stubs/gorm
)
