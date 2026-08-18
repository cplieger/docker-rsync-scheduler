module github.com/cplieger/docker-rsync-scheduler

go 1.27.0

require (
	github.com/cplieger/health v1.6.0
	go.yaml.in/yaml/v3 v3.0.5
)

require github.com/cplieger/envx/yamlenv/v2 v2.0.0

require github.com/cplieger/slogx v1.6.2

require github.com/cplieger/pathinside/v2 v2.0.0 // indirect

require (
	github.com/cplieger/envx/v2 v2.0.0
	github.com/cplieger/scheduler/v4 v4.0.0
	pgregory.net/rapid v1.3.0 // test-only
)

require github.com/cplieger/pathinside v1.0.2 // indirect
