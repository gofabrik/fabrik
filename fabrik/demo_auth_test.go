package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type demoHarness struct {
	t   *testing.T
	src string
	bin string
	env []string
}

func newDemoHarness(t *testing.T) *demoHarness {
	t.Helper()
	demoDir, err := filepath.Abs("../examples/demo")
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	h := &demoHarness{
		t:   t,
		src: copyDemoWithLocalReplaces(t, demoDir, repoRoot),
		bin: filepath.Join(tmp, "demo-bin"),
	}
	h.env = append(os.Environ(),
		"FABRIK_ENV=production",
		"DEMO_SESSION_COOKIE_SECURE=false",
		"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
		"DEMO_STORAGE_PATH="+filepath.Join(tmp, "storage"),
	)
	h.build()
	return h
}

func (h *demoHarness) build() {
	h.t.Helper()
	cmd := exec.Command("go", "build", "-o", h.bin, ".") // #nosec G204 -- launches the go toolchain with controlled args
	cmd.Dir = h.src
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("go build: %v\n%s", err, out)
	}
}

func (h *demoHarness) migrate() {
	h.t.Helper()
	cmd := exec.Command(h.bin, "database", "migrate") // #nosec G204 -- launches a controlled binary built by this test
	cmd.Dir = h.src
	cmd.Env = append(h.env, "DEMO_HTTP_ADDR=:0")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("demo migrate: %v\n%s", err, out)
	}
}

// addMigration rebuilds the demo because migrations are embedded.
func (h *demoHarness) addMigration(name, sql string) {
	h.t.Helper()
	path := filepath.Join(h.src, "shared", "migrations", name)
	if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.build()
	h.migrate()
}

func (h *demoHarness) withServer(fn func(port string)) {
	h.t.Helper()
	port := freePort(h.t)
	server := exec.Command(h.bin, "run") // #nosec G204 -- launches a controlled binary built by this test
	server.Dir = h.src
	server.Env = append(h.env, "DEMO_HTTP_ADDR=:"+port)
	var out bytes.Buffer
	server.Stdout = &out
	server.Stderr = &out
	if err := server.Start(); err != nil {
		h.t.Fatal(err)
	}
	defer func() {
		server.Process.Kill() //nolint:errcheck // best-effort test process cleanup
		server.Wait()         //nolint:errcheck // best-effort test process cleanup
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://localhost:" + port + "/")
		if err == nil {
			resp.Body.Close() //nolint:errcheck // drain is unnecessary for readiness
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("demo server did not answer:\n%s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	fn(port)
}

func noFollowClient(jar http.CookieJar) *http.Client {
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (h *demoHarness) postForm(client *http.Client, port, path string, form url.Values) int {
	h.t.Helper()
	resp, err := client.PostForm("http://localhost:"+port+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // only the status code matters
	return resp.StatusCode
}

func (h *demoHarness) login(port, email, password string) int {
	h.t.Helper()
	return h.postForm(noFollowClient(nil), port, "/login", url.Values{"email": {email}, "password": {password}})
}

func (h *demoHarness) loginClient(port, email, password string) (*http.Client, int) {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	client := noFollowClient(jar)
	return client, h.postForm(client, port, "/login", url.Values{"email": {email}, "password": {password}})
}

func (h *demoHarness) get(client *http.Client, port, path string) (int, string) {
	h.t.Helper()
	resp, err := client.Get("http://localhost:" + port + path)
	if err != nil {
		h.t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // body already read
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// Demo account seeding preserves changed credentials and restores deleted identities or credentials across restarts.
func TestDemoAuthPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	h := newDemoHarness(t)
	h.migrate()
	h.withServer(func(port string) {
		if code := h.login(port, "admin@example.com", "admin"); code != http.StatusSeeOther {
			t.Fatalf("baseline admin login: want 303, got %d", code)
		}
	})

	// Seeding must not replace a changed password after restart.
	changedHash := `$argon2id$v=19$m=19456,t=2,p=1$ox/yJu0jSSXKkJh0ZnmkNQ$zBDV/BUiEql/v2AjJ79pKV6QIVdBcxZn1NHDDsOIUEM`
	h.addMigration("0008_test_change_password.sql",
		`UPDATE password_credentials SET hash = '`+changedHash+`' WHERE identity_id = 'admin';`)
	h.withServer(func(port string) {
		if code := h.login(port, "admin@example.com", "changed"); code != http.StatusSeeOther {
			t.Fatalf("changed password after restart: want 303, got %d", code)
		}
		if code := h.login(port, "admin@example.com", "admin"); code == http.StatusSeeOther {
			t.Fatal("seeding reset a changed credential")
		}
		if code := h.login(port, "viewer@example.com", "viewer"); code != http.StatusSeeOther {
			t.Fatalf("viewer login after admin change: want 303, got %d", code)
		}
	})

	// Deleting an identity must cascade before seeding can restore it.
	h.addMigration("0009_test_delete_identity.sql",
		`DELETE FROM identities WHERE id = 'admin';`)
	h.withServer(func(port string) {
		if code := h.login(port, "admin@example.com", "admin"); code != http.StatusSeeOther {
			t.Fatalf("admin login after cascade and re-seed: want 303, got %d (cascade left the changed credential in place?)", code)
		}
		if code := h.login(port, "admin@example.com", "changed"); code == http.StatusSeeOther {
			t.Fatal("changed credential survived the identity delete; cascade did not fire")
		}
	})

	// Seeding restores a missing credential after restart.
	h.addMigration("0010_test_delete_credential.sql",
		`DELETE FROM password_credentials WHERE identity_id = 'admin';`)
	h.withServer(func(port string) {
		if code := h.login(port, "admin@example.com", "admin"); code != http.StatusSeeOther {
			t.Fatalf("admin login after credential loss and re-seed: want 303, got %d", code)
		}
	})
}

// Registration creates and signs in viewer accounts without changing existing
// credentials.
func TestDemoRegistration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	h := newDemoHarness(t)
	h.migrate()

	const goodPassword = "a-long-enough-password"
	register := func(port, email, password, confirm string) int {
		return h.postForm(noFollowClient(nil), port, "/register",
			url.Values{"email": {email}, "password": {password}, "confirm": {confirm}})
	}
	registerClient := func(port, email, password string) (*http.Client, int) {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := noFollowClient(jar)
		return client, h.postForm(client, port, "/register",
			url.Values{"email": {email}, "password": {password}, "confirm": {password}})
	}

	var client *http.Client
	var firstSubject string
	h.withServer(func(port string) {
		// Registration of a seed email fails because seeding runs first.
		if code := register(port, "admin@example.com", goodPassword, goodPassword); code != http.StatusUnprocessableEntity {
			t.Fatalf("seed-email registration on fresh db: want 422, got %d", code)
		}
		if code := h.login(port, "admin@example.com", "admin"); code != http.StatusSeeOther {
			t.Fatalf("seeded admin login after collision attempt: want 303, got %d", code)
		}

		var code int
		client, code = registerClient(port, "user@example.com", goodPassword)
		if code != http.StatusSeeOther {
			t.Fatalf("registration: want 303, got %d", code)
		}
		code, body := h.get(client, port, "/private")
		if code != http.StatusOK {
			t.Fatalf("/private as registered user: want 200, got %d", code)
		}
		firstSubject = body
		if code, _ := h.get(client, port, "/admin"); code != http.StatusForbidden {
			t.Fatalf("/admin as registered user: want 403, got %d", code)
		}

		if code := register(port, "user@example.com", "another-long-password", "another-long-password"); code != http.StatusUnprocessableEntity {
			t.Fatalf("duplicate-email registration: want 422, got %d", code)
		}
		if code := h.login(port, "user@example.com", goodPassword); code != http.StatusSeeOther {
			t.Fatalf("original login after duplicate attempt: want 303, got %d", code)
		}

	})

	// Restarting resets the in-memory registration limiter while preserving the
	// first session.
	h.withServer(func(port string) {
		if code := register(port, "v@example.com", "fourteen-chars", "fourteen-chars"); code != http.StatusUnprocessableEntity {
			t.Fatalf("14-character password: want 422, got %d", code)
		}
		if code := register(port, "v@example.com", goodPassword, "different-confirmation"); code != http.StatusUnprocessableEntity {
			t.Fatalf("mismatched confirmation: want 422, got %d", code)
		}
		// The byte cap rejects this password despite its smaller rune count.
		multibyte := strings.Repeat("é", 260)
		if code := register(port, "v@example.com", multibyte, multibyte); code != http.StatusUnprocessableEntity {
			t.Fatalf("multibyte oversized password: want 422, got %d", code)
		}
		if _, code := registerClient(port, "v@example.com", goodPassword); code != http.StatusSeeOther {
			t.Fatalf("registration after rejected attempts: want 303, got %d", code)
		}

		// Registering while signed in switches the session to the new
		// account.
		code, _ := h.get(client, port, "/private")
		if code != http.StatusOK {
			t.Fatalf("/private before signed-in registration: want 200, got %d", code)
		}
		if code := h.postForm(client, port, "/register",
			url.Values{"email": {"second@example.com"}, "password": {goodPassword}, "confirm": {goodPassword}}); code != http.StatusSeeOther {
			t.Fatalf("signed-in registration: want 303, got %d", code)
		}
		code, body := h.get(client, port, "/private")
		if code != http.StatusOK {
			t.Fatalf("/private after signed-in registration: want 200, got %d", code)
		}
		if body == firstSubject {
			t.Fatal("session still shows the first account after registering a second")
		}
	})

	// Restarting provides a fresh registration limiter budget.
	h.withServer(func(port string) {
		fifteen := strings.Repeat("p", 15)
		resp15, err := noFollowClient(nil).PostForm("http://localhost:"+port+"/register",
			url.Values{"email": {"b15@example.com"}, "password": {fifteen}, "confirm": {fifteen}})
		if err != nil {
			t.Fatal(err)
		}
		resp15.Body.Close() //nolint:errcheck // only status and headers matter
		if resp15.StatusCode != http.StatusSeeOther {
			t.Fatalf("15-character password: want 303, got %d", resp15.StatusCode)
		}
		if got := resp15.Header.Get("RateLimit-Remaining"); got != "4" {
			t.Fatalf("RateLimit-Remaining after first attempt = %q, want 4", got)
		}
		if reset, err := strconv.Atoi(resp15.Header.Get("RateLimit-Reset")); err != nil || reset < 1 {
			t.Fatalf("RateLimit-Reset = %q, want a positive integer", resp15.Header.Get("RateLimit-Reset"))
		}
		maxLen := strings.Repeat("p", 512)
		if code := register(port, "b512@example.com", maxLen, maxLen); code != http.StatusSeeOther {
			t.Fatalf("512-byte password: want 303, got %d", code)
		}
		if code := register(port, "Mixed@Example.COM", goodPassword, goodPassword); code != http.StatusSeeOther {
			t.Fatalf("denormalized email registration: want 303, got %d", code)
		}
		if code := h.login(port, "mixed@example.com", goodPassword); code != http.StatusSeeOther {
			t.Fatalf("login with normalized email: want 303, got %d", code)
		}

		// The sixth attempt exhausts the registration limiter's burst of five.
		for i := range 2 {
			if code := register(port, "filler@example.com", "short", "short"); code != http.StatusUnprocessableEntity {
				t.Fatalf("filler attempt %d: want 422, got %d", i, code)
			}
		}
		resp, err := noFollowClient(nil).PostForm("http://localhost:"+port+"/register",
			url.Values{"email": {"over@example.com"}, "password": {goodPassword}, "confirm": {goodPassword}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // only status and headers matter
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("sixth attempt in the burst window: want 429, got %d", resp.StatusCode)
		}
		if got := resp.Header.Get("RateLimit-Limit"); got != "5" {
			t.Fatalf("RateLimit-Limit = %q, want 5", got)
		}
		// A rate of ten per minute refills one token in six seconds, allowing for
		// subsecond rounding.
		if after, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || after < 5 || after > 6 {
			t.Fatalf("Retry-After = %q, want 5..6 seconds", resp.Header.Get("Retry-After"))
		}
	})

	// Rejected registrations leave no identities without credentials.
	h.addMigration("0011_test_orphan_check.sql",
		`CREATE TABLE test_orphan_check (orphans INTEGER NOT NULL CHECK (orphans = 0));
INSERT INTO test_orphan_check
SELECT COUNT(*) FROM identities i
WHERE NOT EXISTS (SELECT 1 FROM password_credentials c WHERE c.identity_id = i.id);`)

	// Registered rows use normalized emails, opaque 43-character IDs, active
	// status, and viewer-only claims.
	h.addMigration("0012_test_registered_rows.sql",
		`CREATE TABLE test_registered_rows (ok INTEGER NOT NULL CHECK (ok = 2));
INSERT INTO test_registered_rows
SELECT COUNT(*) FROM identities i JOIN password_credentials c ON c.identity_id = i.id
WHERE c.email IN ('user@example.com', 'mixed@example.com')
  AND i.claims = '{"roles":["viewer"]}' AND i.status = 'active' AND length(i.id) = 43;`)

	// Registered accounts survive a restart.
	h.withServer(func(port string) {
		if code := h.login(port, "user@example.com", goodPassword); code != http.StatusSeeOther {
			t.Fatalf("registered login after restart: want 303, got %d", code)
		}
	})
}
