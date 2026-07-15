module github.com/cplieger/docker-rsync-scheduler

go 1.26.5

require (
	github.com/cplieger/health v1.2.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/cplieger/slogx v1.2.0

require github.com/cplieger/scheduler v1.1.1

require (
	github.com/cplieger/envx v1.0.0
	pgregory.net/rapid v1.3.0 // test-only
)
