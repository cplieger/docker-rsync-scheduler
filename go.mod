module github.com/cplieger/docker-rsync-scheduler

go 1.27.0

require (
	github.com/cplieger/envx v1.6.4
	github.com/cplieger/envx/yamlenv v1.2.3
	github.com/cplieger/health v1.6.0
	github.com/cplieger/scheduler/v3 v3.0.2
	github.com/cplieger/slogx v1.6.3
	go.yaml.in/yaml/v3 v3.0.5
	pgregory.net/rapid v1.3.0 // test-only
)

require github.com/cplieger/pathinside v1.0.2 // indirect
