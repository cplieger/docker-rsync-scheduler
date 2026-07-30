module github.com/cplieger/docker-rsync-scheduler

go 1.26.5

require (
	github.com/cplieger/health v1.4.0
	go.yaml.in/yaml/v3 v3.0.5
)

require github.com/cplieger/envx/yamlenv v1.2.1

require github.com/cplieger/slogx v1.5.0

require github.com/cplieger/pathinside v1.0.0 // indirect

require (
	github.com/cplieger/envx v1.5.1
	github.com/cplieger/scheduler/v3 v3.0.0
	pgregory.net/rapid v1.3.0 // test-only
)
