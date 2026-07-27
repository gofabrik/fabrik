package mailtemplates_test

import (
	"strings"
	"testing"
	"testing/fstest"

	mailtemplates "github.com/gofabrik/fabrik/mail/templates"
)

func tree(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys["templates/mail/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func load(t *testing.T, files map[string]string) *mailtemplates.Renderer {
	t.Helper()
	r, err := mailtemplates.Load(tree(files), "templates/mail")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRender_FillsBothBodies(t *testing.T) {
	r := load(t, map[string]string{
		"welcome.txt":  "Hi {{.Name}}!",
		"welcome.html": "<p>Hi {{.Name}}!</p>",
	})
	content, err := r.Render("welcome", struct{ Name string }{"A & B"})
	if err != nil {
		t.Fatal(err)
	}
	if content.Text != "Hi A & B!" {
		t.Errorf("text = %q; text bodies must not be HTML-escaped", content.Text)
	}
	if content.HTML != "<p>Hi A &amp; B!</p>" {
		t.Errorf("html = %q; html bodies must be escaped", content.HTML)
	}
}

func TestRender_SubdirectoryNames(t *testing.T) {
	r := load(t, map[string]string{
		"digest/weekly.txt":  "digest",
		"digest/weekly.html": "<p>digest</p>",
	})
	content, err := r.Render("digest/weekly", nil)
	if err != nil {
		t.Fatal(err)
	}
	if content.Text != "digest" || content.HTML != "<p>digest</p>" {
		t.Fatalf("bodies = %q / %q", content.Text, content.HTML)
	}
}

func TestRender_PartialsInBothPlanes(t *testing.T) {
	r := load(t, map[string]string{
		"welcome.txt":  "Hi!\n{{ template \"_footer\" . }}",
		"welcome.html": "<p>Hi!</p>{{ template \"_footer\" . }}",
		"_footer.txt":  "-- The app",
		"_footer.html": "<p>The app</p>",
	})
	content, err := r.Render("welcome", nil)
	if err != nil {
		t.Fatal(err)
	}
	if content.Text != "Hi!\n-- The app" || content.HTML != "<p>Hi!</p><p>The app</p>" {
		t.Fatalf("bodies = %q / %q", content.Text, content.HTML)
	}
}

func TestRenderText_LeavesHTMLEmpty(t *testing.T) {
	r := load(t, map[string]string{
		"welcome.txt":  "text body",
		"welcome.html": "<p>html</p>",
	})
	content, err := r.RenderText("welcome", nil)
	if err != nil {
		t.Fatal(err)
	}
	if content.Text != "text body" || content.HTML != "" {
		t.Fatalf("bodies = %q / %q; RenderText must not render html", content.Text, content.HTML)
	}
}

func TestRender_TextOnlyTemplate(t *testing.T) {
	r := load(t, map[string]string{"reminder.txt": "text"})
	if content, err := r.RenderText("reminder", nil); err != nil || content.Text != "text" {
		t.Errorf("RenderText = %+v, %v", content, err)
	}
	if _, err := r.Render("reminder", nil); err == nil || !strings.Contains(err.Error(), "RenderText") {
		t.Errorf("Render without an html template = %v, want a hint at RenderText", err)
	}
}

func TestRender_UnknownName(t *testing.T) {
	r := load(t, map[string]string{"welcome.txt": "text"})
	if _, err := r.Render("missing", nil); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Errorf("Render = %v, want the unknown name", err)
	}
}

func TestRender_FailureReturnsNoContent(t *testing.T) {
	r := load(t, map[string]string{
		"welcome.txt":  "{{.Name}}",
		"welcome.html": "{{.Name}}",
	})
	content, err := r.Render("welcome", struct{}{})
	if err == nil || !strings.Contains(err.Error(), `text body "welcome"`) {
		t.Errorf("err = %v, want the failing body and name", err)
	}
	if content != (mailtemplates.Content{}) {
		t.Errorf("content = %+v; a failed render must return no content", content)
	}
}

func TestLoad_HTMLWithoutTextSibling(t *testing.T) {
	_, err := mailtemplates.Load(tree(map[string]string{"welcome.html": "<p>Hi</p>"}), "templates/mail")
	if err == nil || !strings.Contains(err.Error(), "welcome.txt") {
		t.Errorf("Load = %v, want the missing text sibling", err)
	}
}

func TestLoad_ParseErrorNamesFile(t *testing.T) {
	_, err := mailtemplates.Load(tree(map[string]string{"bad.txt": "{{"}), "templates/mail")
	if err == nil || !strings.Contains(err.Error(), "templates/mail/bad.txt") {
		t.Errorf("Load = %v, want the failing file", err)
	}
}

func TestLoad_EmptyTree(t *testing.T) {
	fsys := fstest.MapFS{"templates/mail/notes.md": &fstest.MapFile{Data: []byte("x")}}
	if _, err := mailtemplates.Load(fsys, "templates/mail"); err == nil {
		t.Error("Load without templates must error")
	}
}
