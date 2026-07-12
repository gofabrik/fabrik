package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	get := func(name string) (visit, body string) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/?name=%s", port, name))
		if err != nil {
			return "", ""
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return visitRE.FindString(string(b)), string(b)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if first, body := get("one"); first != "" {
			if !strings.HasSuffix(first, "#1") {
				t.Fatalf("first visit = %q, want visit #1 on a fresh database", first)
			}
			if !strings.Contains(body, `<li class="greeting">one</li>`) {
				t.Fatalf("first response should list its own greeting:\n%s", body)
			}
			second, body := get("two")
			if second != "visit #2" {
				t.Fatalf("second visit = %q, want visit #2", second)
			}
			// The greeting written by the first request round-trips
			// through the database into the second response.
			if !strings.Contains(body, `<li class="greeting">two</li>`) || !strings.Contains(body, `<li class="greeting">one</li>`) {
				t.Fatalf("second response should list both greetings:\n%s", body)
			}
			sessionFlow(t, port)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("demo server did not answer")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sessionFlow asserts the session-backed greeting: ?name= renames,
// and the name persists per visitor through the cookie jar.
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
	// The second request carries no name; the session remembers the
	// greeting and the flash library's cell arrives alongside it.
	body = get(client, "/")
	if !strings.Contains(body, "Goodbye, alice!") {
		t.Fatalf("session did not persist the greeting name:\n%s", body)
	}
	if !strings.Contains(body, "Greeting name updated.") {
		t.Fatalf("flash message did not round-trip through the store:\n%s", body)
	}
	// The flash is one-shot: taken, it is gone.
	if body = get(client, "/"); strings.Contains(body, "Greeting name updated.") {
		t.Fatalf("flash survived being taken:\n%s", body)
	}
	// A visitor without the cookie is unaffected: distinct session.
	if body := get(http.DefaultClient, "/"); !strings.Contains(body, "Goodbye, world!") {
		t.Fatalf("fresh visitor should get the default greeting:\n%s", body)
	}
}
