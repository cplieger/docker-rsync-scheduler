module github.com/cplieger/docker-rsync-scheduler

go 1.26.7

require (
	github.com/cplieger/health v1.5.2
	go.yaml.in/yaml/v3 v3.0.5
)

require github.com/cplieger/envx/yamlenv v1.2.3

require github.com/cplieger/slogx v1.6.2

require github.com/cplieger/pathinside v1.0.2 // indirect

require (
	github.com/cplieger/envx v1.6.4
	github.com/cplieger/scheduler/v3 v3.0.2
	pgregory.net/rapid v1.3.0 // test-only
)
