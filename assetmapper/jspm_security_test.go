package assetmapper

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func response(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestJSPMResolver_RejectsUntrustedURLs(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("request reached transport: %s", request.URL)
		return nil, nil
	})}
	resolver := NewJSPMResolver(client)

	for _, raw := range []string{
		"http://example.com/pkg.js",
		"https://127.0.0.1/pkg.js",
		"https://localhost/pkg.js",
		"https://user:password@example.com/pkg.js",
	} {
		if _, err := resolver.Fetch(context.Background(), raw); err == nil {
			t.Errorf("Fetch(%q) succeeded", raw)
		}
	}
}

func TestJSPMResolver_RejectsCrossHostRedirect(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		result := response(request, http.StatusFound, "")
		result.Header.Set("Location", "https://other.example/pkg.js")
		return result, nil
	})}
	resolver := NewJSPMResolver(client)
	resolver.AllowPrivateNetwork = true

	_, err := resolver.Fetch(context.Background(), "https://packages.example/pkg.js")
	if err == nil || !strings.Contains(err.Error(), "different host") {
		t.Fatalf("Fetch redirect error = %v", err)
	}
}

func TestJSPMResolver_ValidatesRedirectAfterClientCallback(t *testing.T) {
	var calls int
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls > 1 {
				t.Fatalf("redirect reached transport after callback changed it to %s", request.URL)
			}
			result := response(request, http.StatusFound, "")
			result.Header.Set("Location", "https://packages.example/next.js")
			return result, nil
		}),
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			request.URL.Scheme = "http"
			request.URL.Host = "127.0.0.1"
			return nil
		},
	}
	resolver := NewJSPMResolver(client)
	resolver.AllowPrivateNetwork = true

	_, err := resolver.Fetch(context.Background(), "https://packages.example/pkg.js")
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("Fetch redirect error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("transport calls = %d, want 1", calls)
	}
}

func TestJSPMResolver_RedirectCallbackCannotRewriteOriginHost(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			result := response(request, http.StatusFound, "")
			result.Header.Set("Location", "https://other.example/pkg.js")
			return result, nil
		}),
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			via[0].URL.Host = "other.example"
			return nil
		},
	}
	resolver := NewJSPMResolver(client)
	resolver.AllowPrivateNetwork = true

	_, err := resolver.Fetch(context.Background(), "https://packages.example/pkg.js")
	if err == nil || !strings.Contains(err.Error(), "redirect from packages.example to different host other.example") {
		t.Fatalf("Fetch redirect error = %v", err)
	}
}

func TestJSPMResolver_ReusesSecuredClient(t *testing.T) {
	resolver := NewJSPMResolver(&http.Client{})
	resolver.AllowPrivateNetwork = true

	first, err := resolver.secureClient()
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.secureClient()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("secureClient rebuilt the client instead of reusing its transport pool")
	}
}

func TestJSPMResolver_FreezesTrustSettingsOnFirstUse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, "package"), nil
	})}
	resolver := NewJSPMResolver(client)
	resolver.AllowPrivateNetwork = true

	if _, err := resolver.Fetch(context.Background(), "https://packages.example/pkg.js"); err != nil {
		t.Fatal(err)
	}
	resolver.AllowHTTP = true
	resolver.AllowPrivateNetwork = false

	if _, err := resolver.Fetch(context.Background(), "http://127.0.0.1/pkg.js"); err == nil ||
		!strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("Fetch after trust-setting mutation = %v, want frozen HTTPS requirement", err)
	}
}

func TestJSPMResolver_BoundsPackageResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, "12345"), nil
	})}
	resolver := NewJSPMResolver(client)
	resolver.AllowPrivateNetwork = true
	resolver.Limits.MaxPackageBytes = 4

	_, err := resolver.Fetch(context.Background(), "https://packages.example/pkg.js")
	if err == nil || !strings.Contains(err.Error(), "4-byte limit") {
		t.Fatalf("Fetch size error = %v", err)
	}
}

func TestJSPMResolver_BoundsGenerateResponseAndPackageCount(t *testing.T) {
	t.Run("response bytes", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return response(request, http.StatusOK, `{"map":{"imports":{"pkg":"https://packages.example/pkg.js"}}}`), nil
		})}
		resolver := NewJSPMResolver(client)
		resolver.AllowPrivateNetwork = true
		resolver.Limits.MaxGenerateBytes = 8

		_, err := resolver.Resolve(context.Background(), []PackageRequest{{Name: "pkg"}})
		if err == nil || !strings.Contains(err.Error(), "8-byte limit") {
			t.Fatalf("Resolve size error = %v", err)
		}
	})

	t.Run("package count", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return response(request, http.StatusOK,
				`{"map":{"imports":{"a":"https://packages.example/a@1.0.0.js","b":"https://packages.example/b@1.0.0.js"}}}`), nil
		})}
		resolver := NewJSPMResolver(client)
		resolver.AllowPrivateNetwork = true
		resolver.Limits.MaxPackages = 1

		_, err := resolver.Resolve(context.Background(), []PackageRequest{{Name: "pkg"}})
		if err == nil || !strings.Contains(err.Error(), "limit is 1") {
			t.Fatalf("Resolve package count error = %v", err)
		}
	})
}
