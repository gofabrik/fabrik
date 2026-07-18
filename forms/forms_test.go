package forms_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/forms"
	"github.com/gofabrik/fabrik/validation"
)

// kind verifies named string conversions.
type kind string

type monitorInput struct {
	Name           string
	URL            string
	Kind           kind
	ExpectedStatus *int
	Active         bool
	Tags           []string
}

func (in monitorInput) Validate() validation.Errors {
	return validation.Check(
		validation.Field("name", in.Name, validation.Required()),
		validation.Field("url", in.URL, validation.Required(), validation.URL()),
		validation.Field("kind", string(in.Kind), validation.In("http", "tcp")),
	)
}

func postForm(values url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestBind_DecodeAndValidate(t *testing.T) {
	status := "200"
	r := postForm(url.Values{
		"name":            {"prod"},
		"url":             {"https://x.test"},
		"kind":            {"http"},
		"expected_status": {status},
		"active":          {"on"},
		"tags":            {"a", "b"},
		"csrf_token":      {"ignored"}, // unknown fields are ignored
	})
	form, err := forms.Bind[monitorInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if !form.Valid() {
		t.Fatalf("expected valid, got %v", form.Errors)
	}
	d := form.Data
	if d.Name != "prod" || d.URL != "https://x.test" || d.Kind != "http" {
		t.Errorf("scalars wrong: %+v", d)
	}
	if d.ExpectedStatus == nil || *d.ExpectedStatus != 200 {
		t.Errorf("pointer field wrong: %v", d.ExpectedStatus)
	}
	if !d.Active {
		t.Error("checkbox 'on' should bind true")
	}
	if len(d.Tags) != 2 || d.Tags[0] != "a" {
		t.Errorf("[]string wrong: %v", d.Tags)
	}
}

func TestBind_ValidationErrors(t *testing.T) {
	r := postForm(url.Values{"name": {""}, "url": {"not a url"}, "kind": {"ftp"}})
	form, _ := forms.Bind[monitorInput](r)
	if form.Valid() {
		t.Fatal("expected invalid")
	}
	if form.Error("name") != "is required" {
		t.Errorf("name error = %q", form.Error("name"))
	}
	if form.Error("url") == "" || form.Error("kind") == "" {
		t.Errorf("expected url+kind errors, got %v", form.Errors)
	}
}

func TestBind_ConversionErrorKeepsRawForRepopulation(t *testing.T) {
	r := postForm(url.Values{"name": {"x"}, "url": {"https://x.test"}, "kind": {"http"}, "expected_status": {"abc"}})
	form, _ := forms.Bind[monitorInput](r)
	if form.Valid() {
		t.Fatal("expected a conversion error")
	}
	if form.Error("expected_status") != "must be a whole number" {
		t.Errorf("conversion error = %q", form.Error("expected_status"))
	}
	if form.Data.ExpectedStatus != nil {
		t.Errorf("failed field should stay nil, got %v", form.Data.ExpectedStatus)
	}
	// Value returns the raw input, not the zero value.
	if form.Value("expected_status") != "abc" {
		t.Errorf("Value = %q, want raw 'abc'", form.Value("expected_status"))
	}
}

func TestBind_OptionalPointerAbsentIsNil(t *testing.T) {
	r := postForm(url.Values{"name": {"x"}, "url": {"https://x.test"}, "kind": {"http"}})
	form, _ := forms.Bind[monitorInput](r)
	if form.Data.ExpectedStatus != nil {
		t.Errorf("absent pointer should be nil, got %v", *form.Data.ExpectedStatus)
	}
	if form.Data.Active {
		t.Error("absent checkbox should be false")
	}
}

func TestBind_Query(t *testing.T) {
	type filter struct {
		Q     string
		Page  int
		Limit int
	}
	r := httptest.NewRequest(http.MethodGet, "/?q=hi&page=2", nil)
	form, err := forms.Bind[filter](r)
	if err != nil {
		t.Fatal(err)
	}
	if form.Data.Q != "hi" || form.Data.Page != 2 || form.Data.Limit != 0 {
		t.Errorf("query decode wrong: %+v", form.Data)
	}
}

func TestBind_JSON(t *testing.T) {
	type loginInput struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	body := `{"email":"a@b.com","password":"secret"}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	form, err := forms.Bind[loginInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if form.Data.Email != "a@b.com" || form.Data.Password != "secret" {
		t.Errorf("json decode wrong: %+v", form.Data)
	}

	bad := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":`))
	bad.Header.Set("Content-Type", "application/json")
	if _, err := forms.Bind[loginInput](bad); err == nil {
		t.Error("malformed JSON should return an error")
	}
}

func TestBind_BodyTooLarge(t *testing.T) {
	big := "name=" + strings.Repeat("x", 200)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := forms.Bind[monitorInput](r, forms.WithMaxBytes(50)); err != forms.ErrBodyTooLarge {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
}

func multipartForm(values url.Values) *http.Request {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, vs := range values {
		for _, v := range vs {
			_ = w.WriteField(k, v)
		}
	}
	_ = w.Close()
	r := httptest.NewRequest(http.MethodPost, "/", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func TestBind_Multipart(t *testing.T) {
	r := multipartForm(url.Values{
		"name": {"prod"},
		"url":  {"https://x.test"},
		"kind": {"http"},
		"tags": {"a", "b"},
	})
	form, err := forms.Bind[monitorInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if !form.Valid() {
		t.Fatalf("expected valid, got %v", form.Errors)
	}
	if form.Data.Name != "prod" || len(form.Data.Tags) != 2 {
		t.Errorf("multipart decode wrong: %+v", form.Data)
	}
}

func TestBind_BodyTooLargeMultipart(t *testing.T) {
	r := multipartForm(url.Values{"name": {strings.Repeat("x", 5000)}})
	if _, err := forms.Bind[monitorInput](r, forms.WithMaxBytes(50)); err != forms.ErrBodyTooLarge {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
}

func TestBind_BodyTooLargeJSON(t *testing.T) {
	type loginInput struct {
		Email string `json:"email"`
	}
	body := `{"email":"` + strings.Repeat("x", 200) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if _, err := forms.Bind[loginInput](r, forms.WithMaxBytes(50)); err != forms.ErrBodyTooLarge {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
}

func TestBind_JSONTrailingBodyOverLimit(t *testing.T) {
	type loginInput struct {
		Email string `json:"email"`
	}
	body := `{"email":"x"}` + strings.Repeat("y", 500)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if _, err := forms.Bind[loginInput](r, forms.WithMaxBytes(50)); err != forms.ErrBodyTooLarge {
		t.Errorf("trailing body past the cap: err = %v, want ErrBodyTooLarge", err)
	}
}

func TestBind_JSONTrailingJunkRejected(t *testing.T) {
	type loginInput struct {
		Email string `json:"email"`
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":"x"}{"extra":true}`))
	r.Header.Set("Content-Type", "application/json")
	_, err := forms.Bind[loginInput](r)
	if err == nil {
		t.Fatal("trailing JSON value should be rejected")
	}
	if err == forms.ErrBodyTooLarge {
		t.Error("trailing junk under the cap should be a decode error, not ErrBodyTooLarge")
	}
}

func TestBind_URLEncodedAboveParseFormCap(t *testing.T) {
	const size = 11 << 20
	r := postForm(url.Values{"name": {strings.Repeat("x", size)}})
	form, err := forms.Bind[struct{ Name string }](r, forms.WithMaxBytes(20<<20))
	if err != nil {
		t.Fatalf("11 MiB body under a 20 MiB limit should bind, got %v", err)
	}
	if len(form.Data.Name) != size {
		t.Errorf("Name length = %d, want %d", len(form.Data.Name), size)
	}
}

func TestBind_NamedElementSlice(t *testing.T) {
	type tagged struct {
		Tags []kind
	}
	r := postForm(url.Values{"tags": {"http", "tcp"}})
	form, err := forms.Bind[tagged](r)
	if err != nil {
		t.Fatal(err)
	}
	if len(form.Data.Tags) != 2 || form.Data.Tags[0] != "http" {
		t.Errorf("named-element slice wrong: %v", form.Data.Tags)
	}
}

func TestBind_PointerToUnsupportedStaysNil(t *testing.T) {
	type inner struct{ X int }
	type outer struct {
		Name  string
		Inner *inner
	}
	r := postForm(url.Values{"name": {"x"}, "inner": {"whatever"}})
	form, err := forms.Bind[outer](r)
	if err != nil {
		t.Fatal(err)
	}
	if form.Data.Inner != nil {
		t.Errorf("pointer to unsupported kind should stay nil, got %+v", form.Data.Inner)
	}
}

func TestBind_EmptyJSONBody(t *testing.T) {
	type loginInput struct {
		Email string `json:"email"`
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/json")
	if _, err := forms.Bind[loginInput](r); err == nil {
		t.Error("empty JSON body should return an error")
	}
}

func TestBind_TagOptionsStripped(t *testing.T) {
	type input struct {
		Name string `form:"name,omitempty"`
	}
	r := postForm(url.Values{"name": {"prod"}})
	form, err := forms.Bind[input](r)
	if err != nil {
		t.Fatal(err)
	}
	if form.Data.Name != "prod" {
		t.Errorf("comma options in form tag should be stripped, got %q", form.Data.Name)
	}
}

func TestBind_UnsupportedFieldsNoPanic(t *testing.T) {
	type embedded struct{ Extra string }
	type input struct {
		embedded
		Name   string
		Codes  [3]int
		Meta   map[string]string
		Nested struct{ X int }
	}
	r := postForm(url.Values{
		"name":   {"prod"},
		"codes":  {"1"},
		"meta":   {"whatever"},
		"nested": {"whatever"},
	})
	form, err := forms.Bind[input](r)
	if err != nil {
		t.Fatal(err)
	}
	if form.Data.Name != "prod" {
		t.Errorf("supported field should still bind, got %q", form.Data.Name)
	}
	if form.Data.Codes != [3]int{} || form.Data.Meta != nil || form.Data.Nested.X != 0 {
		t.Errorf("unsupported fields should stay zero: %+v", form.Data)
	}
}

func TestBind_NoValidateMethod(t *testing.T) {
	type plain struct{ Name string }
	r := postForm(url.Values{"name": {"x"}})
	form, err := forms.Bind[plain](r)
	if err != nil || !form.Valid() || form.Data.Name != "x" {
		t.Errorf("plain struct (no Validate) bind failed: %+v %v", form, err)
	}
}
