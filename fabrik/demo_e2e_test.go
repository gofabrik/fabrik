package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
	// Local replacements make the copied demo use this checkout's modules.
	src := copyDemoWithLocalReplaces(t, demoDir, repoRoot)
	bin := filepath.Join(tmp, "demo-bin")
	build := exec.Command("go", "build", "-o", bin, ".") // #nosec G204 -- launches the go toolchain with controlled args
	build.Dir = src
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	port := freePort(t)
	env := append(os.Environ(),
		"DEMO_HTTP_ADDR=:"+port,
		"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
		"DEMO_CROSSORIGIN_TRUSTED_ORIGINS=https://trusted.example",
	)
	// An invalid config verifies that help and command listing skip construction.
	cfgPath := filepath.Join(src, "config.yaml")
	goodCfg, err := os.ReadFile(cfgPath) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("http: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	help := exec.Command(bin, "--help") // #nosec G204 -- launches a controlled binary built by this test
	help.Dir = src
	help.Env = env
	out, err := help.CombinedOutput()
	if err != nil {
		t.Fatalf("demo --help: %v\n%s", err, out)
	}
	for _, cmd := range []string{"config", "database", "run", "serve"} {
		if !commandListed(out, cmd) {
			t.Fatalf("demo --help missing %q:\n%s", cmd, out)
		}
	}
	bare := exec.Command(bin) // #nosec G204 -- launches a controlled binary built by this test
	bare.Dir = src
	bare.Env = env
	out, err = bare.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("bare demo: err=%v (want exit 2)\n%s", err, out)
	}
	for _, cmd := range []string{"config", "database", "run", "serve"} {
		if !commandListed(out, cmd) {
			t.Fatalf("bare demo listing missing %q:\n%s", cmd, out)
		}
	}
	// Nested help exposes declared inputs without constructing the application.
	nestedHelp := exec.Command(bin, "database", "migrate", "--help") // #nosec G204 -- launches a controlled binary built by this test
	nestedHelp.Dir = src
	nestedHelp.Env = env
	out, err = nestedHelp.CombinedOutput()
	if err != nil {
		t.Fatalf("database migrate --help: %v\n%s", err, out)
	}
	for _, want := range []string{"demo database migrate", "--dry-run", "Print what would run"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("nested help missing %q:\n%s", want, out)
		}
	}
	// Parse errors must occur before application construction.
	badArgs := exec.Command(bin, "database", "migrate", "sideways") // #nosec G204 -- launches a controlled binary built by this test
	badArgs.Dir = src
	badArgs.Env = env
	if out, err := badArgs.CombinedOutput(); err == nil || !strings.Contains(string(out), "unexpected argument") {
		t.Fatalf("unexpected positional accepted: err=%v\n%s", err, out)
	}
	if err := os.WriteFile(cfgPath, goodCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	// Dry-run binds the flag without creating the database.
	dry := exec.Command(bin, "database", "migrate", "-n") // #nosec G204 -- launches a controlled binary built by this test
	dry.Dir = src
	dry.Env = env
	if out, err := dry.CombinedOutput(); err != nil || !strings.Contains(string(out), "would apply pending migrations") {
		t.Fatalf("dry run: err=%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "demo.db")); !os.IsNotExist(err) {
		t.Fatalf("dry run touched the database: %v", err)
	}
	migrate := exec.Command(bin, "database", "migrate") // #nosec G204 -- launches a controlled binary built by this test
	migrate.Dir = src
	migrate.Env = env
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("demo migrate: %v\n%s", err, out)
	}
	server := exec.Command(bin, "run") // #nosec G204 -- launches a controlled binary built by this test
	server.Dir = src
	server.Env = env
	// A shared buffer is safe because exec serializes writes to identical non-file writers.
	var serverOut bytes.Buffer
	server.Stdout = &serverOut
	server.Stderr = &serverOut
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // best-effort test process cleanup
	defer server.Process.Kill() // #nosec G104 -- best-effort test process cleanup

	visitRE := regexp.MustCompile(`visit #(\d+)`)
	base := "http://localhost:" + port
	// A -1 count means no response or no parseable counter.
	visit := func() (int, string) {
		resp, err := http.Get(base + "/")
		if err != nil {
			return -1, ""
		}
		b, _ := io.ReadAll(resp.Body)
		//nolint:errcheck // response body close after reading is cleanup only
		resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
		m := visitRE.FindStringSubmatch(string(b))
		if m == nil {
			return -1, string(b)
		}
		n, _ := strconv.Atoi(m[1])
		return n, string(b)
	}

	deadline := time.Now().Add(10 * time.Second)

	for {
		if _, body := visit(); body != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("demo server did not answer")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Greeting inserts are synchronous: two renames appear in the list immediately.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	postGreet(t, c, base, "one")
	if body := postGreet(t, c, base, "two"); !strings.Contains(body, `<li class="greeting">one</li>`) || !strings.Contains(body, `<li class="greeting">two</li>`) {
		t.Fatalf("greetings are synchronous; the list should show both:\n%s", body)
	}

	// Visit counts are eventually consistent because workers update them asynchronously.
	for {
		if n, _ := visit(); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("visit counter never advanced; the background job is not draining")
		}
		time.Sleep(50 * time.Millisecond)
	}
	errorPagesFlow(t, base)
	sessionFlow(t, port)
	crossOriginFlow(t, port)
	formsFlow(t, port)
	rateLimitFlow(t, base)
	gracefulShutdown(t, server)

	logged := serverOut.String()
	if !strings.Contains(logged, "mail: would send") || !strings.Contains(logged, "greetings@demo.example") || !strings.Contains(logged, "notifier@demo.example") {
		t.Fatalf("server output missing the greeting notification mail:\n%s", logged)
	}
}

func rateLimitFlow(t *testing.T, base string) {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	post := func() *http.Response {
		resp, err := noRedirect.Post(base+"/greet", "application/x-www-form-urlencoded", strings.NewReader("name=limit"))
		if err != nil {
			t.Fatal(err)
		}
		//nolint:errcheck // response body close after reading is cleanup only
		defer resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
		io.Copy(io.Discard, resp.Body)
		return resp
	}
	var last *http.Response
	successes := 0
	got429 := false
	for i := 0; i < 20; i++ {
		last = post()
		if last.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if last.StatusCode != http.StatusSeeOther {
			t.Fatalf("post %d: status %d, want 303 or the terminating 429", i+1, last.StatusCode)
		}
		successes++
	}
	if !got429 {
		t.Fatalf("greeting form never rate-limited after 20 rapid posts (last status %d)", last.StatusCode)
	}
	if successes == 0 {
		t.Fatal("no within-limit post succeeded before the denial")
	}
	for _, header := range []string{"Retry-After", "RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"} {
		if last.Header.Get(header) == "" {
			t.Fatalf("429 missing %s: %v", header, last.Header)
		}
	}
	if last.Header.Get("RateLimit-Remaining") != "0" {
		t.Fatalf("RateLimit-Remaining = %q on denial", last.Header.Get("RateLimit-Remaining"))
	}
	retry, err := strconv.Atoi(last.Header.Get("Retry-After"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Duration(retry)*time.Second + 200*time.Millisecond)
	if resp := post(); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("post after Retry-After = %d, want success", resp.StatusCode)
	}
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // response body close after reading is cleanup only
	defer resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
	if resp.StatusCode != http.StatusOK || resp.Header.Get("RateLimit-Limit") != "" {
		t.Fatalf("GET / status=%d headers=%v; must be unaffected", resp.StatusCode, resp.Header)
	}
}

func errorPagesFlow(t *testing.T, base string) {
	t.Helper()
	resp, err := http.Get(base + "/definitely-missing")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	//nolint:errcheck // response body close after reading is cleanup only
	resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing route status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("404 Content-Type = %q, want the handler-set HTML type", ct)
	}
	if !strings.Contains(string(b), "/definitely-missing") {
		t.Fatalf("404 body should render the template with the path:\n%s", b)
	}

	req, err := http.NewRequest(http.MethodDelete, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	//nolint:errcheck // response body close after reading is cleanup only
	resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE / status = %d, want 405", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("405 Content-Type = %q, want the handler-set HTML type", ct)
	}
	if !strings.Contains(string(b), "DELETE") {
		t.Fatalf("405 body should render the template with the method:\n%s", b)
	}
}

// gracefulShutdown requires SIGTERM to produce a clean exit within the generated grace period.
func gracefulShutdown(t *testing.T, server *exec.Cmd) {
	t.Helper()
	if err := server.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("demo did not exit cleanly on SIGTERM: %v", err)
		}
	case <-time.After(35 * time.Second): // generated grace period is 30 seconds
		t.Fatal("demo did not exit within the grace window after SIGTERM")
	}
}

func copyDemoWithLocalReplaces(t *testing.T, demoDir, repoRoot string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "demo-src")
	if err := os.CopyFS(dst, os.DirFS(demoDir)); err != nil {
		t.Fatalf("copy demo: %v", err)
	}
	// os.CopyFS may preserve a read-only mode.
	if err := os.Chmod(filepath.Join(dst, "go.mod"), 0o600); err != nil {
		t.Fatalf("chmod go.mod: %v", err)
	}
	edit := exec.Command("go", "mod", "edit", "-json") // #nosec G204 -- launches the go toolchain with controlled args
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
		e := exec.Command("go", "mod", "edit", "-replace="+r.Path+"="+local) // #nosec G204 -- launches the go toolchain with controlled args
		e.Dir = dst
		if out, err := e.CombinedOutput(); err != nil {
			t.Fatalf("inject replace %s: %v\n%s", r.Path, err, out)
		}
	}
	return dst
}

func crossOriginFlow(t *testing.T, port string) {
	t.Helper()
	base := "http://localhost:" + port
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	post := func(origin, name string) int {
		req, err := http.NewRequest(http.MethodPost, base+"/greet", strings.NewReader("name="+name))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		//nolint:errcheck // response body close after reading is cleanup only
		resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
		return resp.StatusCode
	}

	if code := post("https://evil.example", "mallory"); code != http.StatusForbidden {
		t.Fatalf("cross-origin POST /greet must be 403, got %d", code)
	}
	if body := crossOriginGet(t, client, base+"/", ""); strings.Contains(body, "Goodbye, mallory!") {
		t.Fatalf("rejected cross-origin POST must not rename the session:\n%s", body)
	}
	if code := post("", "sameorigin"); code != http.StatusOK {
		t.Fatalf("same-origin POST /greet must be 200, got %d", code)
	}
	if code := post("https://trusted.example", "zoe"); code != http.StatusOK {
		t.Fatalf("trusted-origin POST /greet must be 200, got %d", code)
	}
	if body := crossOriginGet(t, client, base+"/", ""); !strings.Contains(body, "Goodbye, zoe!") {
		t.Fatalf("GET / should reflect the trusted POST's saved name:\n%s", body)
	}
	if body := crossOriginGet(t, client, base+"/?name=hacker", "https://evil.example"); !strings.Contains(body, "Goodbye, zoe!") {
		t.Fatalf("cross-origin GET must be allowed but must not rename the session:\n%s", body)
	}
}

func formsFlow(t *testing.T, port string) {
	t.Helper()
	base := "http://localhost:" + port
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	if body := crossOriginGet(t, client, base+"/greet", ""); !strings.Contains(body, `name="name"`) {
		t.Fatalf("GET /greet should render the name form:\n%s", body)
	}

	post := func(name string) (int, string) {
		resp, err := client.PostForm(base+"/greet", url.Values{"name": {name}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		//nolint:errcheck // response body close after reading is cleanup only
		resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
		return resp.StatusCode, string(b)
	}

	if code, b := post(""); code != http.StatusOK || !strings.Contains(b, "is required") {
		t.Fatalf("blank name should re-render with 'is required', got %d:\n%s", code, b)
	}

	long := strings.Repeat("x", 21)
	if code, b := post(long); code != http.StatusOK || !strings.Contains(b, "at most 20 characters") || !strings.Contains(b, `value="`+long+`"`) {
		t.Fatalf("21-char name should re-render with the length error and repopulate, got %d:\n%s", code, b)
	}

	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.PostForm(base+"/greet", url.Values{"name": {"alice"}})
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // response body close after reading is cleanup only
	resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("valid name should 303 to /, got %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if body := crossOriginGet(t, client, base+"/", ""); !strings.Contains(body, "Goodbye, alice!") {
		t.Fatalf("after the valid post, / should greet alice:\n%s", body)
	}
}

func crossOriginGet(t *testing.T, client *http.Client, url, origin string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	//nolint:errcheck // response body close after reading is cleanup only
	resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", url, resp.StatusCode, b)
	}
	return string(b)
}

func postGreet(t *testing.T, client *http.Client, base, name string) string {
	t.Helper()
	resp, err := client.PostForm(base+"/greet", url.Values{"name": {name}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	//nolint:errcheck // response body close after reading is cleanup only
	resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /greet = %d:\n%s", resp.StatusCode, b)
	}
	return string(b)
}

func sessionFlow(t *testing.T, port string) {
	t.Helper()
	base := "http://localhost:" + port
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	get := func(c *http.Client, path string) string {
		resp, err := c.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		//nolint:errcheck // response body close after reading is cleanup only
		resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d:\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}

	// Renaming is POST-only; the PRG redirect shows the new name and a one-shot flash.
	body := postGreet(t, client, base, "alice")
	if !strings.Contains(body, "Goodbye, alice!") {
		t.Fatalf("POST rename should greet alice (config greeter: goodbye):\n%s", body)
	}
	if !strings.Contains(body, "Greeting name updated.") {
		t.Fatalf("flash should show on the post-redirect page:\n%s", body)
	}
	if body := get(client, "/"); strings.Contains(body, "Greeting name updated.") {
		t.Fatalf("flash survived being taken:\n%s", body)
	}
	if body := get(client, "/"); !strings.Contains(body, "Goodbye, alice!") {
		t.Fatalf("session did not persist the greeting name:\n%s", body)
	}
	// GET is read-only: a query param cannot rename the session.
	if body := get(client, "/?name=evil"); strings.Contains(body, "Goodbye, evil!") {
		t.Fatalf("GET must not rename the session:\n%s", body)
	}
	if body := get(client, "/"); !strings.Contains(body, "Goodbye, alice!") {
		t.Fatalf("GET must not have renamed the session:\n%s", body)
	}
	if body := get(http.DefaultClient, "/"); !strings.Contains(body, "Goodbye, world!") {
		t.Fatalf("fresh visitor should get the default greeting:\n%s", body)
	}
}

// commandListed matches command entries rather than arbitrary help text.
func commandListed(out []byte, cmd string) bool {
	return regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(cmd) + `\s`).Match(out)
}
