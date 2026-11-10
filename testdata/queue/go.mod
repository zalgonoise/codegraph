module github.com/zalgonoise/codegraph/testdata/queue

go 1.27

require (
	example.com/inhouse/tubequeue v0.0.0
	github.com/kr/beanstalk v0.0.0
	github.com/mikegleasonjr/workers v0.0.0
)

replace (
	example.com/inhouse/tubequeue => ../stubs/tubequeue
	github.com/kr/beanstalk => ../stubs/beanstalk
	github.com/mikegleasonjr/workers => ../stubs/beanstalk-workers
)
