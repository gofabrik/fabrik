package shared

import (
	"net/http"
	"sync"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/web"
)

// WebData supplies shared layout data without changing the page template's dot.
type WebData struct {
	Page    any
	Request RequestView
	Viewer  *ViewerView

	flashes func() ([]flash.Message, error)
}

type RequestView struct {
	Path  string
	Route string
}

type ViewerView struct {
	Name string
}

// Flashes consumes pending messages at most once, when first evaluated.
func (d WebData) Flashes() ([]flash.Message, error) {
	if d.flashes == nil {
		return nil, nil
	}
	return d.flashes()
}

func compose(r *http.Request, page any, sessions *session.Manager[Session]) (*WebData, error) {
	d := &WebData{
		Page:    page,
		Request: RequestView{Path: r.URL.Path, Route: r.Pattern},
	}
	ok, err := sessions.Has(r.Context())
	if err != nil {
		return nil, err
	}
	if ok {
		s, err := sessions.Get(r.Context())
		if err != nil {
			return nil, err
		}
		if s.Name != "" {
			d.Viewer = &ViewerView{Name: s.Name}
		}
	}
	return d, nil
}

// NewWebData returns a provider that loads flashes only when templates access them.
//
//fabrik:provider
func NewWebData(sessions *session.Manager[Session], flashes *flash.Flash) web.DataProvider {
	return func(r *http.Request, page any) (any, error) {
		d, err := compose(r, page, sessions)
		if err != nil {
			return nil, err
		}
		ctx := r.Context()
		d.flashes = sync.OnceValues(func() ([]flash.Message, error) {
			return flashes.Take(ctx)
		})
		return d, nil
	}
}
