module github.com/cplieger/docker-rsync-scheduler

go 1.26.5

require (
	github.com/cplieger/health v1.1.6
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/cplieger/slogx v1.1.0

require github.com/cplieger/scheduler v1.1.0

require pgregory.net/rapid v1.3.0 // test-only
