package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestDemoEndToEnd covers the committed demo wiring and startup path.
func TestDemoEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	demoDir, err := filepath.Abs("../examples/demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := wireCheck(demoDir); err != nil {
		t.Fatalf("demo main.gen.go is stale: %v", err)
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "demo-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = demoDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	port := freePort(t)
	server := exec.Command(bin)
	server.Dir = demoDir
	server.Env = append(os.Environ(),
		"DEMO_HTTP_ADDR=:"+port,
		"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
	)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Process.Kill()

	visitRE := regexp.MustCompile(`visit #(\d+)`)
	get := func() string {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/", port))
		if err != nil {
			return ""
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return visitRE.FindString(string(body))
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if first := get(); first != "" {
			if !strings.HasSuffix(first, "#1") {
				t.Fatalf("first visit = %q, want visit #1 on a fresh database", first)
			}
			if second := get(); second != "visit #2" {
				t.Fatalf("second visit = %q, want visit #2", second)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("demo server did not answer")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
