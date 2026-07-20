package assetmapper

import (
	"errors"
	"net/http"
	"path"
	"strings"
)

// Handler returns an [http.Handler] that serves dev assets at [Mapper.URLPrefix].
//
// In prod mode, the handler returns 404 for every request.
//
// Stale hashes still serve current content; the hash is a cache buster.
func (m *Mapper) Handler() http.Handler {
	return &devHandler{mapper: m}
}

type devHandler struct {
	mapper *Mapper
}

func (h *devHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.mapper.manifest != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !strings.HasPrefix(r.URL.Path, h.mapper.urlPrefix) {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, h.mapper.urlPrefix)
	logical := stripHashSegment(rel)
	if logical == "" {
		http.NotFound(w, r)
		return
	}

	c, err := h.mapper.loadDev(logical)
	if err != nil {
		if errors.Is(err, ErrAssetNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "asset error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", c.contentType)
	// no-cache keeps browser cache entries but revalidates them on each request.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", `"`+c.hash+`"`)
	if match := r.Header.Get("If-None-Match"); match == `"`+c.hash+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(c.content)
}

// stripHashSegment maps a hashed URL path back to its logical asset path.
func stripHashSegment(rel string) string {
	if rel == "" {
		return ""
	}
	dir, base := path.Split(rel)
	if base == "" {
		return ""
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	// A hash segment is recognized by length and hex shape.
	if len(stem) > HashLength+1 {
		dash := stem[len(stem)-HashLength-1]
		hash := stem[len(stem)-HashLength:]
		if dash == '-' && isHex(hash) {
			return dir + stem[:len(stem)-HashLength-1] + ext
		}
	}
	return rel
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
