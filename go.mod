module github.com/cplieger/docker-rsync-scheduler

go 1.26.5

require (
	github.com/cplieger/health v1.4.0
	go.yaml.in/yaml/v3 v3.0.4
)

require github.com/cplieger/envx/yamlenv v1.2.0

require github.com/cplieger/slogx v1.4.0

require (
	github.com/cplieger/envx v1.2.2
	github.com/cplieger/scheduler/v2 v2.1.0
	pgregory.net/rapid v1.3.0 // test-only
)
