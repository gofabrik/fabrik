// Command example runs a small router server.
//
// Run it:
//
//	go run ./example
//
// Try:
//
//	curl -i localhost:8080/
//	curl -i localhost:8080/users/42
//	curl -i localhost:8080/api/me            # 401 without the header
//	curl -i -H 'Authorization: t' localhost:8080/api/me
//	curl -i localhost:8080/admin/reports/q3  # mounted subrouter, param in prefix
//	curl -i -X DELETE localhost:8080/users/42 # 405 with an Allow header
//	curl -i localhost:8080/nope               # 404
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/router"
)

func main() {
	r := router.New()

	r.Use(logger, recoverer)

	// A plain "/" is a subtree match and would hide the custom 404.
	r.Get("/{$}", text("hello from fabrik/router\n"))
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "user %s\n", req.PathValue("id"))
	})
	r.Post("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "updated user %s\n", req.PathValue("id"))
	})

	r.Route("/api", func(r *router.Scope) {
		r.Use(requireAuth)
		r.Get("/me", text("your profile\n"))
		r.Get("/settings", text("your settings\n"))
	})

	// Mount prefix parameters are visible inside flattened subrouter routes.
	r.Mount("/admin/{org}", reportsRouter())

	// Custom miss handlers default to 404/405 and preserve ServeMux's Allow header.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "nothing at %s\n", req.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "%s not allowed (try: %s)\n", req.Method, w.Header().Get("Allow"))
	})

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}

func reportsRouter() *router.Router {
	r := router.New()
	r.Get("/reports/{quarter}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "org %s report for %s\n", req.PathValue("org"), req.PathValue("quarter"))
	})
	return r
}

func logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, req)
		log.Printf("%s %s (%s)", req.Method, req.URL.Path, time.Since(start))
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic: %v", v)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, req)
	})
}

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func text(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, s) }
}
