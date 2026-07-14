package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd verifies scaffold, wire, build, and serve.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	routerDir, err := filepath.Abs("../router")
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := filepath.Abs("../config")
	if err != nil {
		t.Fatal(err)
	}
	templateDir, err := filepath.Abs("../templates")
	if err != nil {
		t.Fatal(err)
	}
	webDir, err := filepath.Abs("../web")
	if err != nil {
		t.Fatal(err)
	}
	assetsDir, err := filepath.Abs("../assetmapper")
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	t.Chdir(tmp)
	// Keep dependency resolution local.
	t.Setenv("GOPROXY", "off")
	err = newCmd([]string{"hello"})
	if err == nil || !strings.Contains(err.Error(), "dependencies could not be resolved") {
		t.Fatalf("fabrik new offline: err = %v, want unresolved-dependencies failure", err)
	}
	dir := filepath.Join(tmp, "hello")

	// Resolve router imports through the checkout under test.
	gomod := filepath.Join(dir, "go.mod")
	mod, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	mod = append(mod, []byte(fmt.Sprintf(
		"\nrequire (\n\tgithub.com/gofabrik/fabrik/assetmapper v0.0.0\n\tgithub.com/gofabrik/fabrik/config v0.0.0\n\tgithub.com/gofabrik/fabrik/router v0.0.0\n\tgithub.com/gofabrik/fabrik/templates v0.0.0\n\tgithub.com/gofabrik/fabrik/web v0.0.0\n)\n\nreplace (\n\tgithub.com/gofabrik/fabrik/assetmapper => %s\n\tgithub.com/gofabrik/fabrik/config => %s\n\tgithub.com/gofabrik/fabrik/router => %s\n\tgithub.com/gofabrik/fabrik/templates => %s\n\tgithub.com/gofabrik/fabrik/web => %s\n)\n",
		assetsDir, configDir, routerDir, templateDir, webDir))...)
	if err := os.WriteFile(gomod, mod, 0o644); err != nil {
		t.Fatal(err)
	}
	// Populate transitive sums from the local cache.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy after pinning: %v\n%s", err, out)
	}

	if _, err := wire(dir); err != nil {
		t.Fatalf("fabrik wire: %v", err)
	}
	// Best-effort tidy to add the requires the generated imports need from
	// what is available offline. -e lets it exit zero even when the fuller
	// import set pulls test-only transitive deps of the replaced local
	// modules (e.g. yaml.v3's kr/text) that the offline cache lacks - those
	// are not build deps. This does not assert the tidy is clean; the build
	// below is the gate on missing build deps.
	tidy = exec.Command("go", "mod", "tidy", "-e")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy after wire: %v\n%s", err, out)
	}
	if err := wireCheck(dir); err != nil {
		t.Fatalf("fabrik wire -check right after wire: %v", err)
	}

	bin := filepath.Join(dir, "hello-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	port := freePort(t)
	server := exec.Command(bin)
	server.Dir = dir
	server.Env = append(os.Environ(), "HELLO_HTTP_ADDR=:"+port)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Process.Kill()

	url := fmt.Sprintf("http://localhost:%s/?name=e2e", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			got := string(body)
			// The scaffold renders through the template set: the page
			// carries the greeting and the shout helper's title.
			if !strings.Contains(got, "<h1>Hello, e2e!</h1>") || !strings.Contains(got, "<title>HELLO, E2E!</title>") {
				t.Fatalf("GET %s = %q, want rendered greeting page", url, got)
			}
			checkAsset(t, port, got)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not answer: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// checkAsset verifies the asset pipeline end to end: the page links a
// content-hashed stylesheet URL, and fetching it serves the CSS with
// the immutable cache header.
func checkAsset(t *testing.T, port, page string) {
	t.Helper()
	m := regexp.MustCompile(`/assets/style-[0-9a-f]{8}\.css`).FindString(page)
	if m == "" {
		t.Fatalf("page carries no hashed asset URL:\n%s", page)
	}
	resp, err := http.Get(fmt.Sprintf("http://localhost:%s%s", port, m))
	if err != nil {
		t.Fatalf("GET %s: %v", m, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", m, resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("GET %s: Cache-Control = %q", m, got)
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}
