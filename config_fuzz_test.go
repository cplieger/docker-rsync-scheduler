package main

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// FuzzParseConfig asserts that parsing arbitrary bytes as YAML config
// never panics. A malformed document must return an error, not crash.
func FuzzParseConfig(f *testing.F) {
	f.Add([]byte("jobs: []"))
	f.Add([]byte("jobs:\n  - name: a\n    local: /a\n"))
	f.Add([]byte("not yaml at all: ["))
	f.Add([]byte(""))
	f.Add([]byte("- - -"))
	f.Add([]byte("jobs:\n  - {}"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parseConfig(data)
	})
}

// TestProperty_ParseConfigNeverPanics feeds random byte slices to the
// parser and confirms it always returns (no panic) regardless of input.
func TestProperty_ParseConfigNeverPanics(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		data := rapid.SliceOf(rapid.Byte()).Draw(rt, "data")
		_, _ = parseConfig(data)
	})
}

func TestProperty_AcceptedRemoteHostNeverLeaksUnbracketedColon(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		host := rapid.SampledFrom([]string{
			"192.0.2.10", "10.0.0.1", "example.com", "host-1.example.com",
			"2001:db8::1", "::1", "fe80::1", "[2001:db8::1]", "[192.0.2.10]",
			"host:", "2001:db8", "fe80::1%eth0", "[name]", "-bad",
		}).Draw(rt, "host")
		raw := host
		if rapid.Bool().Draw(rt, "withUser") {
			raw = "u@" + host
		}
		j := &job{Name: "p", RemoteHost: raw, RemotePath: "/srv/x"}
		if validateRemoteHost(j) != nil {
			return // only an accepted host has a meaningful destination
		}
		dest := remoteDest(j)
		hostSeg := strings.TrimSuffix(dest, ":"+j.RemotePath+"/")
		stripped := hostSeg
		if i := strings.IndexByte(stripped, '['); i >= 0 {
			if k := strings.IndexByte(stripped, ']'); k > i {
				stripped = stripped[:i] + stripped[k+1:]
			}
		}
		if strings.Contains(stripped, ":") {
			rt.Fatalf("accepted remote_host %q -> remoteDest %q has an unbracketed colon", raw, dest)
		}
	})
}
