package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func serveGuarded(t *testing.T, mw func(http.Handler) http.Handler, claims *ClaimSet) *httptest.ResponseRecorder {
	t.Helper()
	var reached bool
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if claims != nil {
		req = req.WithContext(WithClaims(req.Context(), claims))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK && !reached {
		t.Fatal("200 without reaching the handler")
	}
	if rr.Code != http.StatusOK && reached {
		t.Fatalf("handler reached despite %d", rr.Code)
	}
	return rr
}

func TestGuardMatrix(t *testing.T) {
	anonymous := (*ClaimSet)(nil)
	subjectless := &ClaimSet{Scope: "read", Roles: []string{"admin"}}
	viewer := &ClaimSet{Subject: "bob", Roles: []string{"viewer"}, Scope: "read"}
	admin := &ClaimSet{Subject: "alice", Roles: []string{"admin", "viewer"}, Scope: "read write"}

	var g Guard
	tests := []struct {
		name   string
		mw     func(http.Handler) http.Handler
		claims *ClaimSet
		want   int
	}{
		{"authenticated/anonymous", g.Authenticated(), anonymous, http.StatusUnauthorized},
		{"authenticated/subjectless", g.Authenticated(), subjectless, http.StatusUnauthorized},
		{"authenticated/viewer", g.Authenticated(), viewer, http.StatusOK},
		{"role/anonymous", g.Role("admin"), anonymous, http.StatusUnauthorized},
		{"role/subjectless", g.Role("admin"), subjectless, http.StatusUnauthorized},
		{"role/missing", g.Role("admin"), viewer, http.StatusForbidden},
		{"role/granted", g.Role("admin"), admin, http.StatusOK},
		{"scope/anonymous", g.Scope("write"), anonymous, http.StatusUnauthorized},
		{"scope/subjectless", g.Scope("read"), subjectless, http.StatusUnauthorized},
		{"scope/missing", g.Scope("write"), viewer, http.StatusForbidden},
		{"scope/granted", g.Scope("write"), admin, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rr := serveGuarded(t, tt.mw, tt.claims); rr.Code != tt.want {
				t.Errorf("status = %d, want %d", rr.Code, tt.want)
			}
		})
	}
}

func TestGuardOverrides(t *testing.T) {
	var gotUnauthenticated, gotForbidden *http.Request
	g := Guard{
		OnUnauthenticated: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUnauthenticated = r
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}),
		OnForbidden: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotForbidden = r
			w.WriteHeader(http.StatusTeapot)
		}),
	}

	req := httptest.NewRequest(http.MethodGet, "/original", nil)
	rr := httptest.NewRecorder()
	g.Authenticated()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler reached anonymously")
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("OnUnauthenticated not used: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if gotUnauthenticated != req {
		t.Fatal("OnUnauthenticated received a different request")
	}

	viewer := &ClaimSet{Subject: "bob", Roles: []string{"viewer"}}
	req = httptest.NewRequest(http.MethodGet, "/original", nil)
	req = req.WithContext(WithClaims(req.Context(), viewer))
	rr = httptest.NewRecorder()
	g.Role("admin")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler reached without the role")
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusTeapot {
		t.Fatalf("OnForbidden not used: %d", rr.Code)
	}
	if gotForbidden != req {
		t.Fatal("OnForbidden received a different request")
	}
}

func TestGuardZeroValueUsable(t *testing.T) {
	rr := serveGuarded(t, Guard{}.Authenticated(), nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("zero-value guard = %d, want 401", rr.Code)
	}
	if body := rr.Body.String(); body == "" {
		t.Error("bare 401 has no body text")
	}
}
