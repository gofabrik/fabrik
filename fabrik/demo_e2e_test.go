package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	// Local replacements let the committed demo module build before its dependencies are published.
	src := copyDemoWithLocalReplaces(t, demoDir, repoRoot)
	bin := filepath.Join(tmp, "demo-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = src
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	port := freePort(t)
	server := exec.Command(bin)
	server.Dir = src
	server.Env = append(os.Environ(),
		"DEMO_HTTP_ADDR=:"+port,
		"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
	)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Process.Kill()

	visitRE := regexp.MustCompile(`visit #(\d+)`)
	// visit returns the visit counter the page rendered (-1 if the server
	// is not answering yet) and the body.
	visit := func(name string) (int, string) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/?name=%s", port, name))
		if err != nil {
			return -1, ""
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		m := visitRE.FindStringSubmatch(string(b))
		if m == nil {
			return -1, string(b)
		}
		n, _ := strconv.Atoi(m[1])
		return n, string(b)
	}

	deadline := time.Now().Add(10 * time.Second)

	// Verify synchronous greeting writes before polling asynchronous visits.
	for {
		if _, body := visit("one"); body != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("demo server did not answer")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, body := visit("two"); !strings.Contains(body, `<li class="greeting">one</li>`) || !strings.Contains(body, `<li class="greeting">two</li>`) {
		t.Fatalf("greetings are synchronous; the second response should list both:\n%s", body)
	}

	// Visit counts are eventually consistent because workers update them asynchronously.
	for {
		if n, _ := visit("poll"); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("visit counter never advanced; the background job is not draining")
		}
		time.Sleep(50 * time.Millisecond)
	}
	sessionFlow(t, port)
}

func copyDemoWithLocalReplaces(t *testing.T, demoDir, repoRoot string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "demo-src")
	if err := os.CopyFS(dst, os.DirFS(demoDir)); err != nil {
		t.Fatalf("copy demo: %v", err)
	}
	// os.CopyFS may preserve a read-only mode.
	if err := os.Chmod(filepath.Join(dst, "go.mod"), 0o644); err != nil {
		t.Fatalf("chmod go.mod: %v", err)
	}
	edit := exec.Command("go", "mod", "edit", "-json")
	edit.Dir = dst
	out, err := edit.Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}
	var mod struct{ Require []struct{ Path string } }
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatalf("parse go.mod: %v", err)
	}
	const prefix = "github.com/gofabrik/fabrik/"
	for _, r := range mod.Require {
		if !strings.HasPrefix(r.Path, prefix) {
			continue
		}
		local := filepath.Join(repoRoot, strings.TrimPrefix(r.Path, prefix))
		e := exec.Command("go", "mod", "edit", "-replace="+r.Path+"="+local)
		e.Dir = dst
		if out, err := e.CombinedOutput(); err != nil {
			t.Fatalf("inject replace %s: %v\n%s", r.Path, err, out)
		}
	}
	return dst
}

func sessionFlow(t *testing.T, port string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	get := func(c *http.Client, path string) string {
		resp, err := c.Get("http://localhost:" + port + path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d:\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}

	body := get(client, "/?name=alice")
	if !strings.Contains(body, "Goodbye, alice!") {
		t.Fatalf("rename request should greet alice (config greeter: goodbye):\n%s", body)
	}
	if strings.Contains(body, "Greeting name updated.") {
		t.Fatalf("flash showed on the request that added it - it must ride the commit to the next one:\n%s", body)
	}
	body = get(client, "/")
	if !strings.Contains(body, "Goodbye, alice!") {
		t.Fatalf("session did not persist the greeting name:\n%s", body)
	}
	if !strings.Contains(body, "Greeting name updated.") {
		t.Fatalf("flash message did not round-trip through the store:\n%s", body)
	}
	if body = get(client, "/"); strings.Contains(body, "Greeting name updated.") {
		t.Fatalf("flash survived being taken:\n%s", body)
	}
	if body := get(http.DefaultClient, "/"); !strings.Contains(body, "Goodbye, world!") {
		t.Fatalf("fresh visitor should get the default greeting:\n%s", body)
	}
}
