// Package toolchain exists only to hold the build-wiring guard for
// CVE-2026-111943: every outbound TLS connection the Go services make
// (Jetstream, the AppView, plc.directory, the CDN, AGW, the OAuth sign-in
// flow) is verified by the Go toolchain's crypto/x509, which has no
// revocation checking and no compiled-in distrust of the eMudhra
// intermediates before 1.26.10. The mitigation ships in the toolchain, so
// the pins that select the toolchain are the security boundary.
package toolchain

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// minPatchedGo is the oldest Go release containing the crypto/x509
// mitigation for CVE-2026-111943, which distrusted the eight eMudhra
// intermediate CAs revoked with reason cACompromise on 2026-10-18. See
// https://go.dev/issue/82345.
const minPatchedGo = "1.26.10"

// repoRoot is the directory of go.mod, two levels above this package.
var repoRoot = "../.."

// TestGoToolchainIsPatchedForDistrustedCA fails when the running toolchain,
// go.mod's go directive, or deploy/go.Dockerfile's build image would produce
// a binary without the compiled-in distrust list. A binary only inherits the
// mitigation if it is built by a patched Go: the containers are built by
// deploy/go.Dockerfile, and the `go run` Makefile targets (export, feeds
// publish, profile-web, images-*) build on the host toolchain, so all three
// surfaces are checked together.
func TestGoToolchainIsPatchedForDistrustedCA(t *testing.T) {
	running := strings.TrimPrefix(runtime.Version(), "go")
	if v, ok := parseGoVersion(running); !ok {
		t.Skipf("non-release toolchain %q, cannot compare versions", runtime.Version())
	} else if lessThan(v, minPatchedGo) {
		t.Fatalf("toolchain go%s predates the CVE-2026-111943 fix in go%s; upgrade Go on this machine", running, minPatchedGo)
	}

	checkGoMod(t)
	checkDockerfile(t, repoRoot+"/deploy/go.Dockerfile")
}

func checkGoMod(t *testing.T) {
	t.Helper()

	b, err := os.ReadFile(repoRoot + "/go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	m := regexp.MustCompile(`(?m)^go\s+(\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("go.mod has no go directive")
	}

	got := string(m[1])
	v, ok := parseGoVersion(got)
	if !ok {
		t.Fatalf("unparseable go directive %q", got)
	}
	if lessThan(v, minPatchedGo) {
		t.Fatalf("go.mod requires go%s, below the patched go%s (CVE-2026-111943)", got, minPatchedGo)
	}
}

func checkDockerfile(t *testing.T, path string) {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	// The compile stage is the only place the toolchain is pinned; the
	// distroless run stage does not compile anything.
	re := regexp.MustCompile(`(?m)^FROM\s+golang:(\S+)`)
	matches := re.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		t.Fatalf("%s has no golang build stage", path)
	}

	for _, m := range matches {
		tag := m[1]
		// Tags look like 1.26.10 or 1.26.10-bookworm; the variant suffix is
		// not part of the version.
		if i := strings.Index(tag, "-"); i >= 0 {
			tag = tag[:i]
		}
		v, ok := parseGoVersion(tag)
		if !ok {
			t.Fatalf("%s: unparseable golang image tag %q", path, m[1])
		}
		if lessThan(v, minPatchedGo) {
			t.Fatalf("%s builds on golang:%s, below the patched %s (CVE-2026-111943)", path, m[1], minPatchedGo)
		}
	}
}

// parseGoVersion parses a "1.26.10" style version, tolerating a missing patch
// component ("1.26" means 1.26.0). Pre-release suffixes are rejected so a
// tag like 1.27rc1 cannot pass for a patched release.
func parseGoVersion(s string) ([3]int, bool) {
	var out [3]int

	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return out, false
	}

	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}

	return out, true
}

func lessThan(v [3]int, min string) bool {
	want, ok := parseGoVersion(min)
	if !ok {
		panic(fmt.Sprintf("bad minPatchedGo %q", min))
	}

	for i := range v {
		if v[i] != want[i] {
			return v[i] < want[i]
		}
	}

	return false
}
