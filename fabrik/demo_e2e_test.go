package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	poison := filepath.Join(src, "shared", "migrations", "0006_cache_poison.sql")
	// RAISE(FAIL) preserves the disarming update, so only the first
	// sentinel deletion fails.
	trigger := `CREATE TABLE IF NOT EXISTS cache_poison_armed (armed INTEGER NOT NULL);
INSERT INTO cache_poison_armed (armed) VALUES (1);
CREATE TRIGGER IF NOT EXISTS cache_poison BEFORE DELETE ON cache_entries
WHEN CAST(OLD.value AS TEXT) LIKE '%poison-sentinel%' AND (SELECT armed FROM cache_poison_armed) = 1
BEGIN
    UPDATE cache_poison_armed SET armed = 0;
    SELECT RAISE(FAIL, 'poisoned cache row');
END;`
	if err := os.WriteFile(poison, []byte(trigger), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "demo-bin")
	build := exec.Command("go", "build", "-o", bin, ".") // #nosec G204 -- launches the go toolchain with controlled args
	build.Dir = src
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	port := freePort(t)
	env := append(os.Environ(),
		"FABRIK_ENV=production",
		// Disable Secure so the HTTP-only client can exercise sessions.
		"DEMO_SESSION_COOKIE_SECURE=false",
		"DEMO_HTTP_ADDR=:"+port,
		"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
		"DEMO_STORAGE_PATH="+filepath.Join(tmp, "storage"),
		"DEMO_CROSSORIGIN_TRUSTED_ORIGINS=https://trusted.example",
	)
	// Help and parse paths skip construction and must not resolve FABRIK_ENV.
	envNoFabrik := append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "FABRIK_ENV=")
	}), "DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"))
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
	help.Env = envNoFabrik
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
	bare.Env = envNoFabrik
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
	nestedHelp.Env = envNoFabrik
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
	badArgs.Env = envNoFabrik
	if out, err := badArgs.CombinedOutput(); err == nil || !strings.Contains(string(out), "unexpected argument") {
		t.Fatalf("unexpected positional accepted: err=%v\n%s", err, out)
	}
	if err := os.WriteFile(cfgPath, goodCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	noIdentity := exec.Command(bin, "database", "migrate", "-n") // #nosec G204 -- launches a controlled binary built by this test
	noIdentity.Dir = src
	noIdentity.Env = envNoFabrik
	if out, err := noIdentity.CombinedOutput(); err == nil || !strings.Contains(string(out), "FABRIK_ENV is required") {
		t.Fatalf("construction without FABRIK_ENV: err=%v\n%s", err, out)
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
	filesFlow(t, base)
	// Invalidation failure is logged without failing the committed write.
	postGreet(t, c, base, "poison-sentinel")
	if _, body := visit(); !strings.Contains(body, "poison-sentinel") {
		t.Fatalf("sentinel greeting not cached:\n%s", body)
	}
	postGreet(t, c, base, "casualty")

	secureHeadersFlow(t, base)
	rateLimitFlow(t, base)
	gracefulShutdown(t, server)

	logged := serverOut.String()
	if !strings.Contains(logged, "mail: would send") || !strings.Contains(logged, "greetings@demo.example") || !strings.Contains(logged, "notifier@demo.example") {
		t.Fatalf("server output missing the greeting notification mail:\n%s", logged)
	}
	// The poisoned deletion is the only expected cache failure.
	if strings.Contains(logged, "cache fault ignored") {
		t.Fatalf("cache operations degraded to fail-open:\n%s", logged)
	}
	if got := strings.Count(logged, "greeting cache delete failed"); got != 1 {
		t.Fatalf("invalidation failures logged %d times, want exactly 1 (the poisoned delete):\n%s", got, logged)
	}
	if !strings.Contains(logged, `"msg":`) {
		t.Fatalf("production logs are not JSON:\n%s", logged)
	}

	developmentFlow(t, src, bin, tmp)
	envIdentityFlow(t, src, bin, tmp)
	productionIgnoresLocalFlow(t, src, bin, tmp)
	fabrikRunFlow(t, src, tmp)
}

func withFabrikEnv(env []string, value string) []string {
	return append(slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "FABRIK_ENV=")
	}), "FABRIK_ENV="+value)
}

func envIdentityFlow(t *testing.T, src, bin, tmp string) {
	t.Helper()
	invalid := exec.Command(bin, "database", "migrate", "-n") // #nosec G204 -- launches a controlled binary built by this test
	invalid.Dir = src
	invalid.Env = withFabrikEnv(developmentEnv(t, tmp, freePort(t)), "staging")
	if out, err := invalid.CombinedOutput(); err == nil || !strings.Contains(string(out), `invalid FABRIK_ENV "staging"`) {
		t.Fatalf("invalid environment accepted: err=%v\n%s", err, out)
	}

	profile := filepath.Join(src, "config.production.yaml")
	if err := os.Rename(profile, profile+".off"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(profile+".off", profile); err != nil {
			t.Fatal(err)
		}
	}()
	missing := exec.Command(bin, "database", "migrate", "-n") // #nosec G204 -- launches a controlled binary built by this test
	missing.Dir = src
	missing.Env = withFabrikEnv(developmentEnv(t, tmp, freePort(t)), "production")
	if out, err := missing.CombinedOutput(); err == nil || !strings.Contains(string(out), "config.production.yaml") {
		t.Fatalf("missing profile did not fail naming the file: err=%v\n%s", err, out)
	}
}

func productionIgnoresLocalFlow(t *testing.T, src, bin, tmp string) {
	t.Helper()
	local := filepath.Join(src, "config.local.yaml")
	if err := os.WriteFile(local, []byte("log:\n  format: text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(local) //nolint:errcheck // test cleanup

	port := freePort(t)
	server := exec.Command(bin, "run") // #nosec G204 -- launches a controlled binary built by this test
	server.Dir = src
	server.Env = append(withFabrikEnv(developmentEnv(t, tmp, port), "production"), "DEMO_SESSION_COOKIE_SECURE=false")
	var out bytes.Buffer
	server.Stdout = &out
	server.Stderr = &out
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // best-effort test process cleanup
	defer server.Process.Kill() // #nosec G104 -- best-effort test process cleanup

	waitServe(t, "http://localhost:"+port+"/")
	gracefulShutdown(t, server)
	logged := out.String()
	if !strings.Contains(logged, `"msg":`) || strings.Contains(logged, "level=") {
		t.Fatalf("config.local.yaml leaked into production logging:\n%s", logged)
	}
}

func waitServe(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(url) // #nosec G107 -- test requests a loopback URL constructed above
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			//nolint:errcheck // response body close after reading is cleanup only
			resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
			return string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not answer at %s: %v", url, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url) // #nosec G107 -- test requests a loopback URL constructed above
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	//nolint:errcheck // response body close after reading is cleanup only
	resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
	return string(body)
}

func getHeader(t *testing.T, url, name string) string {
	t.Helper()
	resp, err := http.Get(url) // #nosec G107 -- test requests a loopback URL constructed above
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // response body close after reading is cleanup only
	defer resp.Body.Close()        // #nosec G104 -- response body close after reading is cleanup only
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain before close
	return resp.Header.Get(name)
}

func postGreetSetCookie(t *testing.T, base string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(base+"/greet", url.Values{"name": {"cookie-probe"}})
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // response body close after reading is cleanup only
	defer resp.Body.Close()        // #nosec G104 -- response body close after reading is cleanup only
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain before close
	return resp.Header.Get("Set-Cookie")
}

func developmentEnv(t *testing.T, tmp, port string) []string {
	t.Helper()
	return append(os.Environ(),
		"FABRIK_ENV=development",
		"DEMO_HTTP_ADDR=:"+port,
		"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
		"DEMO_STORAGE_PATH="+filepath.Join(tmp, "storage"),
		"DEMO_CROSSORIGIN_TRUSTED_ORIGINS=https://trusted.example",
	)
}

func developmentFlow(t *testing.T, src, bin, tmp string) {
	t.Helper()
	port := freePort(t)
	server := exec.Command(bin, "run") // #nosec G204 -- launches a controlled binary built by this test
	server.Dir = src
	server.Env = developmentEnv(t, tmp, port)
	var out bytes.Buffer
	server.Stdout = &out
	server.Stderr = &out
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // best-effort test process cleanup
	defer server.Process.Kill() // #nosec G104 -- best-effort test process cleanup

	base := "http://localhost:" + port
	page := waitServe(t, base+"/")
	csp := getHeader(t, base+"/", "Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("development CSP is missing the explicit relaxation: %q", csp)
	}
	if strings.Contains(csp, "'sha256-") {
		t.Fatalf("development CSP carries a hash despite source assets: %q", csp)
	}

	if sc := postGreetSetCookie(t, base); sc == "" || strings.Contains(sc, "Secure") {
		t.Fatalf("development session cookie should be set and not Secure: %q", sc)
	}

	appJS := regexp.MustCompile(`/assets/app-[0-9a-f]{8}\.js`).FindString(page)
	if appJS == "" {
		t.Fatalf("page carries no hashed app.js URL:\n%s", page)
	}
	jsPath := filepath.Join(src, "web", "assets", "app.js")
	orig, err := os.ReadFile(jsPath) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	edited := append(append([]byte{}, orig...), []byte("\nconsole.log(\"dev-edit\");\n")...)
	if err := os.WriteFile(jsPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	pageAfter := getBody(t, base+"/")
	appJSAfter := regexp.MustCompile(`/assets/app-[0-9a-f]{8}\.js`).FindString(pageAfter)
	if appJSAfter == "" || appJSAfter == appJS {
		t.Fatalf("asset URL did not follow the disk edit: %q -> %q", appJS, appJSAfter)
	}
	if body := getBody(t, base+appJSAfter); !strings.Contains(body, "dev-edit") {
		t.Fatalf("served asset does not carry the disk edit:\n%s", body)
	}
	if err := os.WriteFile(jsPath, orig, 0o600); err != nil {
		t.Fatal(err)
	}

	gracefulShutdown(t, server)
	if logged := out.String(); !strings.Contains(logged, "level=") {
		t.Fatalf("development logs are not text:\n%s", logged)
	}
}

func fabrikRunFlow(t *testing.T, src, tmp string) {
	t.Helper()
	if v, ok := os.LookupEnv("FABRIK_ENV"); ok {
		defer os.Setenv("FABRIK_ENV", v) // #nosec G104 -- test env restore
		os.Unsetenv("FABRIK_ENV")        // #nosec G104 -- test env setup
	}
	port := freePort(t)
	cmd, err := runCommand(filepath.Join(src, "web"), []string{"run"})
	if err != nil {
		t.Fatalf("fabrik run from subtree: %v", err)
	}
	if cmd.Dir != src {
		t.Fatalf("child cwd = %q, want the module root %q", cmd.Dir, src)
	}
	cmd.Env = append(cmd.Env,
		"DEMO_HTTP_ADDR=:"+port,
		"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
		"DEMO_STORAGE_PATH="+filepath.Join(tmp, "storage"),
		"DEMO_CROSSORIGIN_TRUSTED_ORIGINS=https://trusted.example",
		"DEMO_SESSION_COOKIE_SECURE=true",
		"GOWORK=off",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// go run re-execs the built binary; kill the whole process group.
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // best-effort test process cleanup

	base := "http://localhost:" + port
	deadline := time.Now().Add(120 * time.Second)
	for {
		resp, err := http.Get(base + "/") // #nosec G107 -- test requests a loopback URL constructed above
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			//nolint:errcheck // response body close after reading is cleanup only
			resp.Body.Close() // #nosec G104 -- response body close after reading is cleanup only
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "'unsafe-inline'") {
				t.Fatalf("fabrik run did not select the development environment: CSP %q", csp)
			}
			if !strings.Contains(string(body), "/assets/") {
				t.Fatalf("fabrik run page carries no assets:\n%s", body)
			}
			if sc := postGreetSetCookie(t, base); sc == "" || !strings.Contains(sc, "Secure") {
				t.Fatalf("env override did not secure the session cookie: %q", sc)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fabrik run server did not answer: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func filesFlow(t *testing.T, base string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("hello storage"))
	mw.Close()
	resp, err := http.Post(base+"/files", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	resp, err = http.Get(base + "/files/uploads/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello storage" {
		t.Fatalf("serve = %q", body)
	}

	req, _ := http.NewRequest("GET", base+"/files/uploads/hello.txt", nil)
	req.Header.Set("Range", "bytes=0-4")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "hello" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Disposition") != "attachment" {
		t.Fatalf("uploaded content must not serve same-origin inline: %v", resp.Header)
	}

	resp, err = http.Get(base + "/files")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "uploads/hello.txt") {
		t.Fatalf("listing missing upload:\n%s", body)
	}

	// Encoded dot segments bypass mux canonicalization and must be rejected by Serve.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = noRedirect.Get(base + "/files/%2e%2e/%2e%2e/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("encoded traversal: %d, want the handler's 400 (CheckKey rejection)", resp.StatusCode)
	}
}

func secureHeadersFlow(t *testing.T, base string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	fetch := func(method, url string, mod func(*http.Request)) *http.Response {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if mod != nil {
			mod(req)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	assertBaseline := func(kind string, resp *http.Response, wantStatus int) {
		t.Helper()
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s status = %d, want %d", kind, resp.StatusCode, wantStatus)
		}
		assertBaselineHeaders(t, kind, resp.Header)
	}

	home := fetch(http.MethodGet, base+"/", nil)
	homeCSP := home.Header.Get("Content-Security-Policy")
	body, _ := io.ReadAll(home.Body)
	home.Body.Close() //nolint:errcheck
	if home.StatusCode != http.StatusOK {
		t.Fatalf("home status = %d", home.StatusCode)
	}
	assertBaselineHeaders(t, "home", home.Header)
	// Exactly one inline script (the importmap) is served; the CSP's
	// hash set must equal its hash - nothing missing, nothing extra.
	var inline []string
	for _, m := range regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`).FindAllStringSubmatch(string(body), -1) {
		if strings.Contains(m[1], "src=") {
			continue
		}
		inline = append(inline, m[2])
	}
	if len(inline) != 1 {
		t.Fatalf("inline scripts = %d, want exactly 1 (the importmap):\n%s", len(inline), body)
	}
	sum := sha256.Sum256([]byte(inline[0]))
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	cspHashes := regexp.MustCompile(`'sha256-[^']+'`).FindAllString(homeCSP, -1)
	if len(cspHashes) != 1 || cspHashes[0] != wantHash {
		t.Fatalf("CSP hash set %v, want exactly [%s]", cspHashes, wantHash)
	}
	if !strings.Contains(homeCSP, "script-src 'self' ") {
		t.Fatalf("script-src missing 'self': %q", homeCSP)
	}
	if !regexp.MustCompile(`<script type="module" src="/assets/[^"]+"[^>]*></script>`).MatchString(string(body)) {
		t.Fatalf("no external module entrypoint on home:\n%s", body)
	}
	// The CSP requires htmx indicator styles to come from the stylesheet.
	src := regexp.MustCompile(`src="(/assets/[^"]+app-[^"]+\.js)"`).FindStringSubmatch(string(body))
	imports := regexp.MustCompile(`"app":"(/assets/[^"]+)"`).FindStringSubmatch(string(body))
	appURL := ""
	if src != nil {
		appURL = src[1]
	} else if imports != nil {
		appURL = imports[1]
	}
	if appURL == "" {
		t.Fatalf("cannot find the app module URL in the page:\n%s", body)
	}
	appResp := fetch(http.MethodGet, base+appURL, nil)
	appJS, _ := io.ReadAll(appResp.Body)
	appResp.Body.Close() //nolint:errcheck
	if !strings.Contains(string(appJS), "includeIndicatorStyles = false") {
		t.Fatal("served app.js does not disable htmx indicator styles")
	}
	cssURL := regexp.MustCompile(`href="(/assets/[^"]+\.css)"`).FindStringSubmatch(string(body))
	if cssURL == nil {
		t.Fatalf("no stylesheet link on home:\n%s", body)
	}
	cssResp := fetch(http.MethodGet, base+cssURL[1], nil)
	css, _ := io.ReadAll(cssResp.Body)
	cssResp.Body.Close() //nolint:errcheck
	// The stylesheet preserves htmx's indicator visibility transitions.
	for _, rule := range []string{
		".htmx-indicator",
		"visibility: hidden",
		".htmx-request .htmx-indicator",
		".htmx-request.htmx-indicator",
		"visibility: visible",
		"transition: opacity 200ms ease-in",
	} {
		if !strings.Contains(string(css), rule) {
			t.Fatalf("stylesheet missing htmx indicator rule %q", rule)
		}
	}

	assertBaseline("asset", fetch(http.MethodGet, base+appURL, nil), http.StatusOK)
	assertBaseline("404", fetch(http.MethodGet, base+"/definitely-not-here", nil), http.StatusNotFound)
	redirect := fetch(http.MethodPost, base+"/greet", func(r *http.Request) {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Body = io.NopCloser(strings.NewReader("name=headers"))
	})
	assertBaseline("redirect", redirect, http.StatusSeeOther)
	// The uploaded file exercises storage serving rather than listing.
	assertBaseline("storage serve", fetch(http.MethodGet, base+"/files/uploads/hello.txt", nil), http.StatusOK)
	assertBaseline("cross-origin 403", fetch(http.MethodPost, base+"/greet", func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}), http.StatusForbidden)
}

func assertBaselineHeaders(t *testing.T, kind string, h http.Header) {
	t.Helper()
	want := map[string]string{
		"X-Content-Type-Options":            "nosniff",
		"X-Frame-Options":                   "DENY",
		"Referrer-Policy":                   "no-referrer",
		"Permissions-Policy":                "geolocation=(), camera=(), microphone=(), payment=(), usb=()",
		"Cross-Origin-Opener-Policy":        "same-origin",
		"Cross-Origin-Resource-Policy":      "same-origin",
		"X-Permitted-Cross-Domain-Policies": "none",
		"X-DNS-Prefetch-Control":            "off",
	}
	for name, v := range want {
		if got := h.Get(name); got != v {
			t.Fatalf("%s response %s = %q, want %q", kind, name, got, v)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'", "form-action 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("%s response CSP missing %q: %s", kind, directive, csp)
		}
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Fatalf("%s response carries HSTS on plain HTTP", kind)
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
			assertBaselineHeaders(t, "rate-limit 429", last.Header)
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
