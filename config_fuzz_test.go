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

// FuzzHasShellMeta asserts that hasShellMeta agrees with an independent
// oracle over coverage-guided input: a string is unsafe iff it contains a
// control character (< 0x20 or 0x7f) or any shell metacharacter. This
// strengthens the previous crash-only target so the security gate's
// detection invariant accumulates a persistent corpus, complementing the
// every-PR rapid property test.
func FuzzHasShellMeta(f *testing.F) {
	f.Add("/sources/caddy")
	f.Add("root@host")
	f.Add("a;b")
	f.Add("**/*.lock")
	f.Add("")
	f.Add("a\x1fb")
	f.Add("a\x7fb")
	f.Fuzz(func(t *testing.T, s string) {
		got := hasShellMeta(s)
		want := false
		for _, r := range s {
			if r < 0x20 || r == 0x7f || strings.ContainsRune(shellMetaChars, r) {
				want = true
				break
			}
		}
		if got != want {
			t.Fatalf("hasShellMeta(%q) = %v, want %v", s, got, want)
		}
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

// TestProperty_HasShellMetaTotal confirms hasShellMeta is total over
// arbitrary strings and that any string containing a known injection
// character is rejected.
func TestProperty_HasShellMetaTotal(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.String().Draw(rt, "s")
		got := hasShellMeta(s)

		wantMeta := false
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				wantMeta = true
				break
			}
			for _, m := range shellMetaChars {
				if r == m {
					wantMeta = true
					break
				}
			}
			if wantMeta {
				break
			}
		}
		if got != wantMeta {
			rt.Fatalf("hasShellMeta(%q) = %v, want %v", s, got, wantMeta)
		}
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
