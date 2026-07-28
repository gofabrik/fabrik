package assetmapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultJSPMBaseURL is the official jspm.io generator endpoint.
// Override [JSPMResolver.BaseURL] to point at a mirror.
const DefaultJSPMBaseURL = "https://api.jspm.io"

const (
	// DefaultMaxPackageBytes bounds one downloaded package artifact.
	DefaultMaxPackageBytes int64 = 20 << 20
	// DefaultMaxResolutionBytes bounds all artifacts in one resolution.
	DefaultMaxResolutionBytes int64 = 100 << 20
	// DefaultMaxPackages bounds the flattened package closure.
	DefaultMaxPackages = 500
	// DefaultMaxGenerateBytes bounds the JSPM generator response.
	DefaultMaxGenerateBytes int64 = 4 << 20
)

// DownloadLimits bounds package resolution and downloads. Zero fields use the
// corresponding DefaultMax value.
type DownloadLimits struct {
	MaxPackageBytes    int64
	MaxResolutionBytes int64
	MaxPackages        int
	MaxGenerateBytes   int64
}

func (l DownloadLimits) normalized() DownloadLimits {
	if l.MaxPackageBytes <= 0 {
		l.MaxPackageBytes = DefaultMaxPackageBytes
	}
	if l.MaxResolutionBytes <= 0 {
		l.MaxResolutionBytes = DefaultMaxResolutionBytes
	}
	if l.MaxPackages <= 0 {
		l.MaxPackages = DefaultMaxPackages
	}
	if l.MaxGenerateBytes <= 0 {
		l.MaxGenerateBytes = DefaultMaxGenerateBytes
	}
	return l
}

// JSPMResolver implements [PackageResolver] against jspm.io's /generate endpoint.
//
// Construct with [NewJSPMResolver]; the zero value is unusable. Configure its
// fields before the first Resolve or Fetch call. Trust settings are snapshotted
// on first use so the HTTP transport and URL validation cannot diverge.
type JSPMResolver struct {
	// Client is used for both /generate and package-file requests.
	Client *http.Client
	// BaseURL is the jspm.io API root. Empty means
	// [DefaultJSPMBaseURL].
	BaseURL string
	// Limits bounds generator responses and package downloads.
	Limits DownloadLimits
	// AllowHTTP permits plaintext resolver and package URLs. It should be used
	// only for explicitly trusted development mirrors.
	AllowHTTP bool
	// AllowPrivateNetwork permits loopback, private, link-local, and other
	// non-public destinations. It should be used only for trusted mirrors.
	AllowPrivateNetwork bool
	// AllowCrossHostRedirects permits redirects to a different hostname.
	AllowCrossHostRedirects bool

	clientOnce    sync.Once
	securedClient *http.Client
	security      jspmSecurity
	securedErr    error
}

type jspmSecurity struct {
	allowHTTP               bool
	allowPrivateNetwork     bool
	allowCrossHostRedirects bool
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
	limits := j.Limits.normalized()
	if len(reqs) > limits.MaxPackages {
		return nil, fmt.Errorf("jspm.io: package requests exceed limit of %d", limits.MaxPackages)
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
	if err := j.validateRemoteURL(endpoint); err != nil {
		return nil, fmt.Errorf("jspm.io: POST /generate: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client, err := j.secureClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: POST /generate: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close after reading is cleanup only
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jspm.io: POST /generate: status %d: %s", resp.StatusCode, body)
	}

	body, err := readLimited(resp.Body, limits.MaxGenerateBytes)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: POST /generate: %w", err)
	}
	var gen jspmGenerateResponse
	if err := json.Unmarshal(body, &gen); err != nil {
		return nil, fmt.Errorf("jspm.io: decode response: %w", err)
	}
	resolution, err := jspmFlatten(&gen)
	if err != nil {
		return nil, err
	}
	if len(resolution.Packages) > limits.MaxPackages {
		return nil, fmt.Errorf("jspm.io: resolved %d packages, limit is %d", len(resolution.Packages), limits.MaxPackages)
	}
	return resolution, nil
}

// Fetch downloads a single package file by URL.
func (j *JSPMResolver) Fetch(ctx context.Context, raw string) ([]byte, error) {
	result, err := j.FetchPackage(ctx, raw)
	if err != nil {
		return nil, err
	}
	return result.Content, nil
}

// FetchPackage downloads a package and returns its final source URL.
func (j *JSPMResolver) FetchPackage(ctx context.Context, raw string) (*FetchedPackage, error) {
	if err := j.validateRemoteURL(raw); err != nil {
		return nil, fmt.Errorf("jspm.io: fetch %s: %w", raw, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	client, err := j.secureClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: fetch %s: %w", raw, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close after reading is cleanup only
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jspm.io: fetch %s: status %d", raw, resp.StatusCode)
	}
	limit := j.Limits.normalized().MaxPackageBytes
	content, err := readLimited(resp.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: fetch %s: %w", raw, err)
	}
	sourceURL := raw
	if resp.Request != nil && resp.Request.URL != nil {
		sourceURL = resp.Request.URL.String()
	}
	return &FetchedPackage{Content: content, SourceURL: sourceURL}, nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", limit)
	}
	return content, nil
}

func (j *JSPMResolver) secureClient() (*http.Client, error) {
	j.clientOnce.Do(func() {
		if j.Client == nil {
			j.securedErr = fmt.Errorf("jspm.io: nil HTTP client")
			return
		}
		j.security = jspmSecurity{
			allowHTTP:               j.AllowHTTP,
			allowPrivateNetwork:     j.AllowPrivateNetwork,
			allowCrossHostRedirects: j.AllowCrossHostRedirects,
		}
		client := *j.Client
		previousRedirect := client.CheckRedirect
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			originalHost := ""
			if len(via) > 0 && via[0] != nil && via[0].URL != nil {
				originalHost = via[0].URL.Hostname()
			}
			if previousRedirect != nil {
				if err := previousRedirect(req, via); err != nil {
					return err
				}
			}
			if len(via) >= 10 {
				return fmt.Errorf("jspm.io: stopped after 10 redirects")
			}
			target := ""
			if req != nil && req.URL != nil {
				target = req.URL.String()
			}
			if err := j.security.validateRemoteURL(target); err != nil {
				return err
			}
			if !j.security.allowCrossHostRedirects && originalHost != "" &&
				!strings.EqualFold(req.URL.Hostname(), originalHost) {
				return fmt.Errorf("jspm.io: redirect from %s to different host %s", originalHost, req.URL.Hostname())
			}
			return nil
		}
		if !j.security.allowPrivateNetwork {
			var transport *http.Transport
			switch configured := client.Transport.(type) {
			case nil:
				transport = http.DefaultTransport.(*http.Transport).Clone()
			case *http.Transport:
				transport = configured.Clone()
			default:
				j.securedErr = fmt.Errorf("jspm.io: custom HTTP transport cannot enforce private-network protection")
				return
			}
			transport.Proxy = nil
			transport.DialContext = publicDialContext
			transport.DialTLSContext = nil
			transport.DialTLS = nil
			client.Transport = transport
		}
		j.securedClient = &client
	})
	return j.securedClient, j.securedErr
}

func (j *JSPMResolver) validateRemoteURL(raw string) error {
	if _, err := j.secureClient(); err != nil {
		return err
	}
	return j.security.validateRemoteURL(raw)
}

func (security jspmSecurity) validateRemoteURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("URL %q has no host", raw)
	}
	if parsed.User != nil {
		return fmt.Errorf("URL %q must not contain credentials", raw)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !security.allowHTTP {
			return fmt.Errorf("URL %q must use HTTPS", raw)
		}
	default:
		return fmt.Errorf("URL %q has unsupported scheme %q", raw, parsed.Scheme)
	}
	if !security.allowPrivateNetwork && isPrivateHost(parsed.Hostname()) {
		return fmt.Errorf("URL %q targets a private network address", raw)
	}
	return nil
}

func publicDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	for _, address := range addresses {
		if isPrivateIP(address) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("jspm.io: %s has no permitted public address", host)
}

func isPrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return isPrivateIP(address)
	}
	return false
}

func isPrivateIP(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsUnspecified() || address.IsMulticast() {
		return true
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
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
