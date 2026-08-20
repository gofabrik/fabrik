package forms_test

import (
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/forms"
)

type record struct {
	Title    string
	Count    int
	Ratio    float64
	Done     bool
	Nickname *string
	Tags     []string
	Secret   string `form:"-"`
	Renamed  string `form:"custom_name"`
	unseen   string //nolint:unused // an unexported field is not a form field
	CV       forms.File
}

func TestEmpty(t *testing.T) {
	form := forms.Empty[record]()

	if !form.Valid() {
		t.Error("a blank form has no errors")
	}
	for _, field := range []string{"title", "count", "ratio", "done"} {
		if got := form.Value(field); got != "" {
			t.Errorf("Value(%q) = %q, want empty", field, got)
		}
	}
}

// Empty is not From of a zero value: a zero int is 0, and a blank field is
// nothing. Rendering "0" into a new record's form would be wrong.
func TestEmptyIsNotAZeroValue(t *testing.T) {
	if got := forms.Empty[record]().Value("count"); got != "" {
		t.Errorf("Empty count = %q, want empty", got)
	}
	if got := forms.From(record{}).Value("count"); got != "0" {
		t.Errorf("From zero count = %q, want %q", got, "0")
	}
}

func TestFrom(t *testing.T) {
	nickname := "Ada"
	form := forms.From(record{
		Title:    "Hello",
		Count:    42,
		Ratio:    1.5,
		Done:     true,
		Nickname: &nickname,
		Tags:     []string{"a", "b"},
		Secret:   "not a field",
		Renamed:  "tagged",
	})

	for field, want := range map[string]string{
		"title":       "Hello",
		"count":       "42",
		"ratio":       "1.5",
		"done":        "true",
		"nickname":    "Ada",
		"custom_name": "tagged",
	} {
		if got := form.Value(field); got != want {
			t.Errorf("Value(%q) = %q, want %q", field, got, want)
		}
	}

	if got := form.Values("tags"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("Values(tags) = %v, want [a b]", got)
	}
	if got := form.Value("secret"); got != "" {
		t.Errorf(`form:"-" should not be written, got %q`, got)
	}
	if got := form.Data.Title; got != "Hello" {
		t.Errorf("Data.Title = %q, want it kept", got)
	}
}

// A field a browser would not send is left out rather than sent empty, which
// is what lets a decode read it back as the same value.
func TestFromOmitsWhatABrowserWouldNotSend(t *testing.T) {
	form := forms.From(record{Done: false, Nickname: nil})

	if got := form.Values("done"); got != nil {
		t.Errorf("false bool = %v, want the field omitted", got)
	}
	if got := form.Values("nickname"); got != nil {
		t.Errorf("nil pointer = %v, want the field omitted", got)
	}
}

func TestFromSkipsUploads(t *testing.T) {
	r := multipartFiles(t, func(w *multipart.Writer) {
		addFile(t, w, "cv", "cv.pdf", pdf)
	})
	bound, err := forms.Bind[record](r)
	if err != nil {
		t.Fatal(err)
	}
	if !bound.Data.CV.Present() {
		t.Fatal("fixture should carry a file")
	}

	// A file input cannot be given a value, so there is nothing to write.
	if got := forms.From(bound.Data).Values("cv"); got != nil {
		t.Errorf("Values(cv) = %v, want the field omitted", got)
	}
}

// What From writes, Bind reads back as the same value.
func TestFromRoundTripsThroughBind(t *testing.T) {
	nickname := "Ada"
	original := record{
		Title:    "Hello",
		Count:    42,
		Ratio:    1.5,
		Done:     true,
		Nickname: &nickname,
		Tags:     []string{"a", "b"},
		Renamed:  "tagged",
	}

	values := url.Values{}
	from := forms.From(original)
	for _, field := range []string{"title", "count", "ratio", "done", "nickname", "custom_name"} {
		if v := from.Value(field); v != "" {
			values.Set(field, v)
		}
	}
	for _, tag := range from.Values("tags") {
		values.Add("tags", tag)
	}

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	back, err := forms.Bind[record](r)
	if err != nil {
		t.Fatal(err)
	}

	got := back.Data
	if got.Title != original.Title || got.Count != original.Count ||
		got.Ratio != original.Ratio || got.Done != original.Done ||
		got.Renamed != original.Renamed {
		t.Errorf("round trip = %+v, want %+v", got, original)
	}
	if got.Nickname == nil || *got.Nickname != nickname {
		t.Errorf("Nickname = %v, want %q", got.Nickname, nickname)
	}
	if !slices.Equal(got.Tags, original.Tags) {
		t.Errorf("Tags = %v, want %v", got.Tags, original.Tags)
	}
	if got.Secret != "" {
		t.Errorf(`form:"-" should not survive a round trip, got %q`, got.Secret)
	}
}

// A false bool round-trips because omitting it is exactly what an unchecked
// box does, and decoding an absent field gives false.
func TestFromRoundTripsFalseAndNil(t *testing.T) {
	from := forms.From(record{Title: "x", Done: false, Nickname: nil})
	values := url.Values{}
	for _, field := range []string{"title", "count", "ratio", "done", "nickname", "custom_name"} {
		for _, v := range from.Values(field) {
			values.Add(field, v)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	back, err := forms.Bind[record](r)
	if err != nil {
		t.Fatal(err)
	}
	if back.Data.Done {
		t.Error("absent bool should decode false")
	}
	if back.Data.Nickname != nil {
		t.Error("absent pointer should decode nil")
	}
}
