// Package mailtemplates renders paired text and HTML email bodies from one
// template tree.
package mailtemplates

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"strings"

	htmltpl "github.com/gofabrik/t/html/template"
	texttpl "github.com/gofabrik/t/text/template"
)

// Content holds the rendered bodies of one message.
type Content struct {
	Text string
	HTML string
}

// Renderer renders both bodies of a message from one logical name. It is safe
// for concurrent use.
type Renderer struct {
	text *texttpl.Template
	html *htmltpl.Template
}

// Load parses every .txt and .html file under dir in fsys. A template's name
// is its dir-relative path without the extension, so greeting.txt and
// greeting.html are both "greeting" and one name reaches both bodies. Text
// bodies use text/template; HTML bodies use html/template with contextual
// escaping.
//
// Files with a "_"-prefixed base name are shared partials, addressed by
// explicit {{ template }} calls. Every other .html file must have a .txt
// sibling, because every message needs a text body.
func Load(fsys fs.FS, dir string) (*Renderer, error) {
	r := &Renderer{text: texttpl.New(""), html: htmltpl.New("")}
	var htmlBodies []string
	hasText := map[string]bool{}
	found := false
	err := fs.WalkDir(fsys, dir, func(file string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := path.Ext(file)
		if ext != ".txt" && ext != ".html" {
			return nil
		}
		raw, err := fs.ReadFile(fsys, file)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(file, dir+"/"), ext)
		if ext == ".txt" {
			hasText[name] = true
			_, err = r.text.New(name).Parse(string(raw))
		} else {
			if !strings.HasPrefix(path.Base(file), "_") {
				htmlBodies = append(htmlBodies, name)
			}
			_, err = r.html.New(name).Parse(string(raw))
		}
		if err != nil {
			return fmt.Errorf("parse %s: %w", file, err)
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("mailtemplates: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("mailtemplates: no templates under %s", dir)
	}
	for _, name := range htmlBodies {
		if !hasText[name] {
			return nil, fmt.Errorf("mailtemplates: %s has no text body; add %s.txt", path.Join(dir, name+".html"), name)
		}
	}
	return r, nil
}

// Render fills both bodies atomically; Content is returned only when both
// renders succeed.
func (r *Renderer) Render(name string, data any) (Content, error) {
	text, err := r.renderText(name, data)
	if err != nil {
		return Content{}, err
	}
	if r.html.Lookup(name) == nil {
		return Content{}, fmt.Errorf("mailtemplates: no html template %q; use RenderText for text-only mail", name)
	}
	var html bytes.Buffer
	if err := r.html.ExecuteTemplate(&html, name, data); err != nil {
		return Content{}, fmt.Errorf("mailtemplates: html body %q: %w", name, err)
	}
	return Content{Text: text, HTML: html.String()}, nil
}

// RenderText renders the text body alone and leaves HTML empty. A message is
// text-only because the caller asks for text, not because a template is
// missing.
func (r *Renderer) RenderText(name string, data any) (Content, error) {
	text, err := r.renderText(name, data)
	if err != nil {
		return Content{}, err
	}
	return Content{Text: text}, nil
}

func (r *Renderer) renderText(name string, data any) (string, error) {
	if r.text.Lookup(name) == nil {
		return "", fmt.Errorf("mailtemplates: no text template %q", name)
	}
	var buf bytes.Buffer
	if err := r.text.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("mailtemplates: text body %q: %w", name, err)
	}
	return buf.String(), nil
}
