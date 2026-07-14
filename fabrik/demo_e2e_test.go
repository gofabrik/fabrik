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
	"strconv"
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

	// Wait for the server, then assert the synchronous greeting round-trip
	// with two controlled requests: greetings are written in-request, so
	// the greeting from the first shows up alongside the second's. (Done
	// before the counter poll below, which floods greetings.)
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

	// Recording a visit is deferred to a background job, so the counter is
	// eventually consistent: each request enqueues a Visit the worker
	// records later. Poll the page until the counter has advanced past the
	// enqueued visits, which proves the async side effect actually ran.
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
