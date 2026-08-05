package forms_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/forms"
)

type uploadInput struct {
	File  forms.File `form:"file"`
	Label string     `form:"label"`
}

type multiInput struct {
	Attachments []forms.File `form:"attachments"`
}

func multipartFiles(t *testing.T, build func(w *multipart.Writer)) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	build(w)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func addFile(t *testing.T, w *multipart.Writer, field, name, content string) {
	t.Helper()
	f, err := w.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

func TestFile_BindsBesideScalarFields(t *testing.T) {
	r := multipartFiles(t, func(w *multipart.Writer) {
		addFile(t, w, "file", "report.pdf", "content")
		_ = w.WriteField("label", "quarterly")
	})
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	up := form.Data.File
	if !up.Present() || up.ClientFilename() != "report.pdf" || up.Size() != 7 || form.Data.Label != "quarterly" {
		t.Fatalf("bound file = %v %q %d, label %q", up.Present(), up.ClientFilename(), up.Size(), form.Data.Label)
	}
	rc, err := up.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := rc.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(rc)
	if string(rest) != "tent" {
		t.Fatalf("seek+read = %q", rest)
	}
}

func TestFile_ClientFilenameStripsPaths(t *testing.T) {
	cases := map[string]string{
		`C:\fakepath\avatar.png`:  "avatar.png",
		"../weird/../report.pdf":  "report.pdf",
		"plain.txt":               "plain.txt",
		`mixed\dir/deep\name.bin`: "name.bin",
	}
	for sent, want := range cases {
		r := multipartFiles(t, func(w *multipart.Writer) { addFile(t, w, "file", sent, "x") })
		form, err := forms.Bind[uploadInput](r)
		if err != nil {
			t.Fatal(err)
		}
		if got := form.Data.File.ClientFilename(); got != want {
			t.Errorf("ClientFilename(%q) = %q, want %q", sent, got, want)
		}
	}
}

func TestFile_ClientContentTypeIsExposed(t *testing.T) {
	r := multipartFiles(t, func(w *multipart.Writer) { addFile(t, w, "file", "a.txt", "x") })
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if got := form.Data.File.ClientContentType(); got != "application/octet-stream" {
		t.Errorf("ClientContentType = %q", got)
	}
}

func TestFile_AbsentIsZeroAndOpenReturnsErrNoFile(t *testing.T) {
	r := multipartFiles(t, func(w *multipart.Writer) { _ = w.WriteField("label", "x") })
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if form.Data.File.Present() {
		t.Fatal("absent file reported Present")
	}
	if _, err := form.Data.File.Open(); !errors.Is(err, forms.ErrNoFile) {
		t.Fatalf("Open on absent = %v, want ErrNoFile", err)
	}
	if form.Data.File.ClientFilename() != "" || form.Data.File.Size() != 0 || form.Data.File.ClientContentType() != "" {
		t.Fatal("zero File leaked metadata")
	}
}

func TestFile_ZeroByteFileIsPresent(t *testing.T) {
	r := multipartFiles(t, func(w *multipart.Writer) { addFile(t, w, "file", "empty.bin", "") })
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if !form.Data.File.Present() || form.Data.File.Size() != 0 {
		t.Fatalf("zero-byte file: present=%v size=%d", form.Data.File.Present(), form.Data.File.Size())
	}
}

func TestFile_SingleTakesFirstSliceTakesAll(t *testing.T) {
	r := multipartFiles(t, func(w *multipart.Writer) {
		addFile(t, w, "file", "first.txt", "1")
		addFile(t, w, "file", "second.txt", "2")
	})
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if got := form.Data.File.ClientFilename(); got != "first.txt" {
		t.Fatalf("single field took %q, want the first part", got)
	}

	r = multipartFiles(t, func(w *multipart.Writer) {
		addFile(t, w, "attachments", "a.txt", "A")
		addFile(t, w, "attachments", "b.txt", "B")
	})
	multi, err := forms.Bind[multiInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if len(multi.Data.Attachments) != 2 || multi.Data.Attachments[1].ClientFilename() != "b.txt" {
		t.Fatalf("slice field = %d files", len(multi.Data.Attachments))
	}
}

func TestFile_SpillsToDiskOverMaxMemory(t *testing.T) {
	payload := strings.Repeat("s", 64<<10)
	r := multipartFiles(t, func(w *multipart.Writer) { addFile(t, w, "file", "big.bin", payload) })
	form, err := forms.Bind[uploadInput](r, forms.WithMaxMemory(1024))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := form.Data.File.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rc.(*os.File); !ok {
		t.Fatalf("spooled open = %T, want *os.File", rc)
	}
	if _, err := rc.Seek(int64(len(payload))-4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	tail, _ := io.ReadAll(rc)
	if string(tail) != "ssss" {
		t.Fatalf("spooled seek+read = %q", tail)
	}
	if _, err := rc.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != payload {
		t.Fatal("spooled content mismatch")
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.MultipartForm.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
}

func TestFile_OversizeKeepsErrBodyTooLargeWith413(t *testing.T) {
	r := multipartFiles(t, func(w *multipart.Writer) { addFile(t, w, "file", "big.bin", strings.Repeat("x", 4096)) })
	_, err := forms.Bind[uploadInput](r, forms.WithMaxBytes(1024))
	if !errors.Is(err, forms.ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	var se interface{ HTTPStatus() int }
	if !errors.As(err, &se) || se.HTTPStatus() != http.StatusRequestEntityTooLarge {
		t.Fatalf("ErrBodyTooLarge does not carry status 413: %v", err)
	}
}

func TestFile_MalformedMultipartCarries400(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not multipart"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=zzz")
	_, err := forms.Bind[uploadInput](r)
	var se interface{ HTTPStatus() int }
	if err == nil || !errors.As(err, &se) || se.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("malformed multipart = %v, want a 400-status error", err)
	}
}

func TestFile_JSONValuesRejectNullAndAbsentStayZero(t *testing.T) {
	for _, body := range []string{`{"file":"x"}`, `{"file":{}}`, `{"file":[]}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		_, err := forms.Bind[uploadInput](r)
		var se interface{ HTTPStatus() int }
		if err == nil || !errors.As(err, &se) || se.HTTPStatus() != http.StatusBadRequest {
			t.Fatalf("json %s = %v, want a 400-status error", body, err)
		}
	}
	for _, body := range []string{`{"attachments":[]}`, `{"attachments":[null]}`, `{"attachments":["x"]}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		_, err := forms.Bind[multiInput](r)
		var se interface{ HTTPStatus() int }
		if err == nil || !errors.As(err, &se) || se.HTTPStatus() != http.StatusBadRequest {
			t.Fatalf("json %s = %v, want a 400-status error", body, err)
		}
	}
	dup := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"attachments":null,"attachments":[]}`))
	dup.Header.Set("Content-Type", "application/json")
	if _, err := forms.Bind[multiInput](dup); err == nil {
		t.Fatal("duplicate-key JSON with a trailing array bound a file slice")
	}
	for _, body := range []string{`{"attachments":null}`, `{}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		form, err := forms.Bind[multiInput](r)
		if err != nil {
			t.Fatalf("json %s: %v", body, err)
		}
		if form.Data.Attachments != nil {
			t.Fatalf("json %s bound a file slice", body)
		}
	}
	for _, body := range []string{`{"file":null}`, `{}`, `{"label":"x"}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		form, err := forms.Bind[uploadInput](r)
		if err != nil {
			t.Fatalf("json %s: %v", body, err)
		}
		if form.Data.File.Present() {
			t.Fatalf("json %s bound a file", body)
		}
	}
}

func TestBind_MalformedBodiesCarry400(t *testing.T) {
	badJSON := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"label":`))
	badJSON.Header.Set("Content-Type", "application/json")
	_, err := forms.Bind[uploadInput](badJSON)
	var se interface{ HTTPStatus() int }
	if err == nil || !errors.As(err, &se) || se.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %v, want a 400-status error", err)
	}

	badForm := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("a=%zz"))
	badForm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, err = forms.Bind[uploadInput](badForm)
	if err == nil || !errors.As(err, &se) || se.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("malformed urlencoded = %v, want a 400-status error", err)
	}
}

func TestFile_URLEncodedLeavesFileZero(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("label=x&file=sneaky"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	if form.Data.File.Present() || form.Data.Label != "x" {
		t.Fatalf("urlencoded bind: present=%v label=%q", form.Data.File.Present(), form.Data.Label)
	}
}

type definedFile forms.File

type definedFiles []forms.File

type aliasFile = forms.File

func TestFile_OnlyFileAndSliceShapesBind(t *testing.T) {
	type odd struct {
		Defined      definedFile   `form:"file"`
		DefinedSlice definedFiles  `form:"file"`
		Ptr          *forms.File   `form:"file"`
		Arr          [1]forms.File `form:"file"`
	}
	r := multipartFiles(t, func(w *multipart.Writer) { addFile(t, w, "file", "a.txt", "x") })
	form, err := forms.Bind[odd](r)
	if err != nil {
		t.Fatal(err)
	}
	if forms.File(form.Data.Defined).Present() || len(form.Data.DefinedSlice) != 0 ||
		form.Data.Ptr != nil || form.Data.Arr[0].Present() {
		t.Fatal("unsupported shapes bound a file")
	}

	type aliased struct {
		File aliasFile `form:"file"`
	}
	r = multipartFiles(t, func(w *multipart.Writer) { addFile(t, w, "file", "a.txt", "x") })
	af, err := forms.Bind[aliased](r)
	if err != nil {
		t.Fatal(err)
	}
	if !af.Data.File.Present() {
		t.Fatal("true alias did not bind")
	}
}

func TestBind_FailsClosedOnConsumedForm(t *testing.T) {
	closed := []struct {
		name string
		r    func() *http.Request
	}{
		{"multipart", func() *http.Request {
			r := multipartFiles(t, func(w *multipart.Writer) { _ = w.WriteField("a", "1") })
			_ = r.FormValue("a")
			return r
		}},
		{"urlencoded post", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("a=1"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_ = r.FormValue("a")
			return r
		}},
		{"malformed content type", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("a=1"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded; bad")
			_ = r.FormValue("a")
			return r
		}},
	}
	for _, c := range closed {
		if _, err := forms.Bind[uploadInput](c.r()); !errors.Is(err, forms.ErrFormConsumed) {
			t.Errorf("%s: err = %v, want ErrFormConsumed", c.name, err)
		}
	}

	open := []struct {
		name string
		r    func() *http.Request
	}{
		{"GET", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/?a=1", nil)
			_ = r.FormValue("a")
			return r
		}},
		{"JSON", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"label":"x"}`))
			r.Header.Set("Content-Type", "application/json")
			_ = r.FormValue("a")
			return r
		}},
		{"text/plain", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
			r.Header.Set("Content-Type", "text/plain")
			_ = r.FormValue("a")
			return r
		}},
		{"urlencoded DELETE", func() *http.Request {
			r := httptest.NewRequest(http.MethodDelete, "/?a=1", strings.NewReader("b=2"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_ = r.FormValue("a")
			return r
		}},
	}
	for _, c := range open {
		if _, err := forms.Bind[uploadInput](c.r()); err != nil {
			t.Errorf("%s: err = %v, want nil", c.name, err)
		}
	}
}

// pdf is the shortest thing http.DetectContentType calls application/pdf.
const pdf = "%PDF-1.4\nnot really a document, but it sniffs like one\n"

func bindFile(t *testing.T, content string) forms.File {
	t.Helper()
	r := multipartFiles(t, func(w *multipart.Writer) {
		addFile(t, w, "file", "cv.pdf", content)
	})
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	return form.Data.File
}

func bindNoFile(t *testing.T) forms.File {
	t.Helper()
	r := multipartFiles(t, func(w *multipart.Writer) {})
	form, err := forms.Bind[uploadInput](r)
	if err != nil {
		t.Fatal(err)
	}
	return form.Data.File
}

func TestFileReadAll(t *testing.T) {
	t.Run("returns the content and closes the file", func(t *testing.T) {
		file := bindFile(t, pdf)
		got, err := file.ReadAll(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != pdf {
			t.Fatalf("content = %q, want %q", got, pdf)
		}
	})

	t.Run("exactly the limit is allowed", func(t *testing.T) {
		file := bindFile(t, "1234567890")
		got, err := file.ReadAll(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 10 {
			t.Fatalf("len = %d, want 10", len(got))
		}
	})

	t.Run("one byte over is ErrFileTooLarge", func(t *testing.T) {
		file := bindFile(t, "12345678901")
		if _, err := file.ReadAll(10); !errors.Is(err, forms.ErrFileTooLarge) {
			t.Fatalf("err = %v, want ErrFileTooLarge", err)
		}
	})

	t.Run("MaxInt64 limit returns the content", func(t *testing.T) {
		got, err := bindFile(t, pdf).ReadAll(math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != pdf {
			t.Fatalf("content = %q, want %q", got, pdf)
		}
	})

	t.Run("closes the descriptor it opened", func(t *testing.T) {
		r := multipartFiles(t, func(w *multipart.Writer) {
			addFile(t, w, "file", "cv.pdf", pdf)
		})
		// A zero memory threshold spools the upload to disk, so ReadAll
		// works on a real descriptor the count below can observe.
		form, err := forms.Bind[uploadInput](r, forms.WithMaxMemory(0))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				t.Error(err)
			}
		})
		before := openFDs(t)
		if _, err := form.Data.File.ReadAll(1 << 20); err != nil {
			t.Fatal(err)
		}
		if after := openFDs(t); after != before {
			t.Fatalf("open descriptors = %d, want %d: ReadAll left its file open", after, before)
		}
	})

	t.Run("absent is ErrNoFile", func(t *testing.T) {
		if _, err := bindNoFile(t).ReadAll(10); !errors.Is(err, forms.ErrNoFile) {
			t.Fatalf("err = %v, want ErrNoFile", err)
		}
	})

	t.Run("empty file reads as empty, not as absent", func(t *testing.T) {
		got, err := bindFile(t, "").ReadAll(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
	})

	t.Run("reading twice is allowed", func(t *testing.T) {
		file := bindFile(t, pdf)
		if _, err := file.ReadAll(1 << 20); err != nil {
			t.Fatal(err)
		}
		again, err := file.ReadAll(1 << 20)
		if err != nil {
			t.Fatalf("second read: %v", err)
		}
		if string(again) != pdf {
			t.Fatalf("second read = %q, want %q", again, pdf)
		}
	})
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("cannot count descriptors: %v", err)
	}
	return len(ents)
}
