package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		"\nrequire (\n\tgithub.com/gofabrik/fabrik/config v0.0.0\n\tgithub.com/gofabrik/fabrik/router v0.0.0\n)\n\nreplace (\n\tgithub.com/gofabrik/fabrik/config => %s\n\tgithub.com/gofabrik/fabrik/router => %s\n)\n",
		configDir, routerDir))...)
	if err := os.WriteFile(gomod, mod, 0o644); err != nil {
		t.Fatal(err)
	}
	// Fill go.sum for transitive deps (yaml.v3) from the local module
	// cache; GOPROXY=off keeps this hermetic.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy after pinning: %v\n%s", err, out)
	}

	if _, err := wire(dir); err != nil {
		t.Fatalf("fabrik wire: %v", err)
	}
	// The generated file imports the config module, which no hand-written
	// source references; tidy again so its require lands.
	tidy = exec.Command("go", "mod", "tidy")
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
			if got, want := string(body), "Hello, e2e!"; got != want {
				t.Fatalf("GET %s = %q, want %q", url, got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not answer: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
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
