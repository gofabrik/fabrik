package assetmapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultJSPMBaseURL is the official jspm.io generator endpoint.
// Override [JSPMResolver.BaseURL] to point at a mirror.
const DefaultJSPMBaseURL = "https://api.jspm.io"

// JSPMResolver implements [PackageResolver] against jspm.io's /generate endpoint.
//
// Construct with [NewJSPMResolver]; the zero value is unusable.
type JSPMResolver struct {
	// Client is used for both /generate and package-file requests.
	Client *http.Client
	// BaseURL is the jspm.io API root. Empty means
	// [DefaultJSPMBaseURL].
	BaseURL string
}

// NewJSPMResolver returns a resolver wired to jspm.io. Pass nil for
// client to get an [http.Client] with a 30s timeout per call.
func NewJSPMResolver(client *http.Client) *JSPMResolver {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &JSPMResolver{
		Client:  client,
		BaseURL: DefaultJSPMBaseURL,
	}
}

// Resolve expands package requests into a flat [Resolution].
//
// Conflicting scoped versions are rejected because a flat importmap cannot express them.
func (j *JSPMResolver) Resolve(ctx context.Context, reqs []PackageRequest) (*Resolution, error) {
	if len(reqs) == 0 {
		return &Resolution{}, nil
	}
	install := make([]string, len(reqs))
	for i, r := range reqs {
		if r.Version != "" {
			install[i] = r.Name + "@" + r.Version
		} else {
			install[i] = r.Name
		}
	}

	payload, err := json.Marshal(map[string]any{
		"install":  install,
		"env":      []string{"browser", "production"},
		"provider": "jspm.io",
	})
	if err != nil {
		return nil, err
	}

	base := j.BaseURL
	if base == "" {
		base = DefaultJSPMBaseURL
	}
	endpoint := strings.TrimSuffix(base, "/") + "/generate"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := j.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: POST /generate: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close after reading is cleanup only
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jspm.io: POST /generate: status %d: %s", resp.StatusCode, body)
	}

	var gen jspmGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&gen); err != nil {
		return nil, fmt.Errorf("jspm.io: decode response: %w", err)
	}
	return jspmFlatten(&gen)
}

// Fetch downloads a single package file by URL.
func (j *JSPMResolver) Fetch(ctx context.Context, raw string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := j.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: fetch %s: %w", raw, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close after reading is cleanup only
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jspm.io: fetch %s: status %d", raw, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// jspmGenerateResponse mirrors the /generate fields used by vendoring.
type jspmGenerateResponse struct {
	Map struct {
		Imports map[string]string            `json:"imports"`
		Scopes  map[string]map[string]string `json:"scopes"`
	} `json:"map"`
}

// jspmFlatten turns imports and scopes into a deterministic flat resolution.
func jspmFlatten(g *jspmGenerateResponse) (*Resolution, error) {
	byspec := map[string]string{}
	var pkgs []ResolvedPackage
	add := func(spec, u string) error {
		if spec == "" || u == "" {
			return nil
		}
		if prev, dup := byspec[spec]; dup {
			if prev != u {
				return fmt.Errorf("jspm.io: specifier %q resolves to both %s and %s (conflicting dependency versions); vendor the conflicting packages separately", spec, prev, u)
			}
			return nil
		}
		byspec[spec] = u
		pkgs = append(pkgs, ResolvedPackage{
			Specifier: spec,
			Version:   versionFromJSPMURL(u),
			Type:      typeFromURL(u),
			URL:       u,
		})
		return nil
	}
	for _, spec := range sortedKeys(g.Map.Imports) {
		if err := add(spec, g.Map.Imports[spec]); err != nil {
			return nil, err
		}
	}
	for _, scopeKey := range sortedKeys(g.Map.Scopes) {
		scope := g.Map.Scopes[scopeKey]
		for _, spec := range sortedKeys(scope) {
			if err := add(spec, scope[spec]); err != nil {
				return nil, err
			}
		}
	}
	return &Resolution{Packages: pkgs}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// versionFromJSPMURL extracts the package version from JSPM npm URLs.
func versionFromJSPMURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.TrimPrefix(parsed.Path, "/")
	if i := strings.Index(p, ":"); i >= 0 {
		p = p[i+1:]
	}
	// Scoped packages can contain slashes, so the last "@" marks the version.
	at := strings.LastIndex(p, "@")
	if at <= 0 {
		return ""
	}
	rest := p[at+1:]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	return rest
}

// typeFromURL classifies vendored files by extension.
func typeFromURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if strings.HasSuffix(u, ".css") {
		return "css"
	}
	return "js"
}
