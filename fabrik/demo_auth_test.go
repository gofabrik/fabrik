package main

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Demo account seeding preserves changed credentials and restores deleted identities or credentials across restarts.
func TestDemoAuthPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in -short mode")
	}
	demoDir, err := filepath.Abs("../examples/demo")
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	src := copyDemoWithLocalReplaces(t, demoDir, repoRoot)
	bin := filepath.Join(tmp, "demo-bin")
	build := func() {
		t.Helper()
		cmd := exec.Command("go", "build", "-o", bin, ".") // #nosec G204 -- launches the go toolchain with controlled args
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
	}
	build()
	env := func(port string) []string {
		return append(os.Environ(),
			"FABRIK_ENV=production",
			"DEMO_SESSION_COOKIE_SECURE=false",
			"DEMO_HTTP_ADDR=:"+port,
			"DEMO_DATABASE_PATH="+filepath.Join(tmp, "demo.db"),
			"DEMO_STORAGE_PATH="+filepath.Join(tmp, "storage"),
		)
	}
	migrate := func() {
		t.Helper()
		cmd := exec.Command(bin, "database", "migrate") // #nosec G204 -- launches a controlled binary built by this test
		cmd.Dir = src
		cmd.Env = env("0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("demo migrate: %v\n%s", err, out)
		}
	}
	withServer := func(fn func(port string)) {
		t.Helper()
		port := freePort(t)
		server := exec.Command(bin, "run") // #nosec G204 -- launches a controlled binary built by this test
		server.Dir = src
		server.Env = env(port)
		var out bytes.Buffer
		server.Stdout = &out
		server.Stderr = &out
		if err := server.Start(); err != nil {
			t.Fatal(err)
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
				t.Fatalf("demo server did not answer:\n%s", out.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
		fn(port)
	}
	login := func(port, email, password string) int {
		t.Helper()
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.PostForm("http://localhost:"+port+"/login",
			url.Values{"email": {email}, "password": {password}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // only the status code matters
		return resp.StatusCode
	}
	// Migrations are embedded, so each added file needs a rebuild.
	addMigration := func(name, sql string) {
		t.Helper()
		path := filepath.Join(src, "shared", "migrations", name)
		if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
		build()
		migrate()
	}

	migrate()
	withServer(func(port string) {
		if code := login(port, "admin@example.com", "admin"); code != http.StatusSeeOther {
			t.Fatalf("baseline admin login: want 303, got %d", code)
		}
	})

	// Seeding must not replace a changed password after restart.
	changedHash := `$argon2id$v=19$m=19456,t=2,p=1$ox/yJu0jSSXKkJh0ZnmkNQ$zBDV/BUiEql/v2AjJ79pKV6QIVdBcxZn1NHDDsOIUEM`
	addMigration("0008_test_change_password.sql",
		`UPDATE password_credentials SET hash = '`+changedHash+`' WHERE identity_id = 'admin';`)
	withServer(func(port string) {
		if code := login(port, "admin@example.com", "changed"); code != http.StatusSeeOther {
			t.Fatalf("changed password after restart: want 303, got %d", code)
		}
		if code := login(port, "admin@example.com", "admin"); code == http.StatusSeeOther {
			t.Fatal("seeding reset a changed credential")
		}
		if code := login(port, "viewer@example.com", "viewer"); code != http.StatusSeeOther {
			t.Fatalf("viewer login after admin change: want 303, got %d", code)
		}
	})

	// Deleting an identity must cascade before seeding can restore it.
	addMigration("0009_test_delete_identity.sql",
		`DELETE FROM identities WHERE id = 'admin';`)
	withServer(func(port string) {
		if code := login(port, "admin@example.com", "admin"); code != http.StatusSeeOther {
			t.Fatalf("admin login after cascade and re-seed: want 303, got %d (cascade left the changed credential in place?)", code)
		}
		if code := login(port, "admin@example.com", "changed"); code == http.StatusSeeOther {
			t.Fatal("changed credential survived the identity delete; cascade did not fire")
		}
	})

	// Seeding restores a missing credential after restart.
	addMigration("0010_test_delete_credential.sql",
		`DELETE FROM password_credentials WHERE identity_id = 'admin';`)
	withServer(func(port string) {
		if code := login(port, "admin@example.com", "admin"); code != http.StatusSeeOther {
			t.Fatalf("admin login after credential loss and re-seed: want 303, got %d", code)
		}
	})
}
