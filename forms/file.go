package forms

import (
	"errors"
	"io"
	"math"
	"mime/multipart"
	"path/filepath"
	"reflect"
	"strings"
)

// ErrNoFile is returned by [File.Open] when no file was submitted.
var ErrNoFile = errors.New("forms: no file submitted")

// ErrFileTooLarge is returned by [File.ReadAll] when the content exceeds the
// caller's limit. It is not [ErrBodyTooLarge], which is about the whole
// request and carries an HTTP status; this one is about a single file and
// carries none, because whether it is the client's fault depends on what the
// caller already validated.
var ErrFileTooLarge = errors.New("forms: file larger than the limit")

// File represents a request-scoped multipart upload that must be consumed before the handler returns.
type File struct {
	header *multipart.FileHeader
}

// Present reports whether a file part was submitted, including an empty file.
func (f File) Present() bool { return f.header != nil }

// ClientFilename returns the untrusted client-supplied basename.
func (f File) ClientFilename() string {
	if f.header == nil {
		return ""
	}
	name := strings.ReplaceAll(f.header.Filename, `\`, "/")
	return filepath.Base(filepath.ToSlash(name))
}

// Size returns the multipart parser's received byte count.
func (f File) Size() int64 {
	if f.header == nil {
		return 0
	}
	return f.header.Size
}

// ClientContentType returns the untrusted client-declared media type.
func (f File) ClientContentType() string {
	if f.header == nil {
		return ""
	}
	return f.header.Header.Get("Content-Type")
}

// Open returns request-scoped uploaded content or [ErrNoFile]; the caller must close it.
func (f File) Open() (io.ReadSeekCloser, error) {
	if f.header == nil {
		return nil, ErrNoFile
	}
	return f.header.Open()
}

// ReadAll returns the whole file, refusing content longer than max bytes,
// and closes it before returning.
//
// It reports [ErrNoFile] when nothing was submitted and [ErrFileTooLarge] when
// the content is longer than max. [File.Size] is only the parser's count, so
// it is used as a cheap early reject and the read is bounded again on the way
// through: the limit holds whatever the header claimed.
//
// ReadAll retains ownership of the descriptor. The caller never holds it, so
// it cannot keep a spooled temporary file open across whatever it does with
// the bytes afterwards.
func (f File) ReadAll(max int64) ([]byte, error) {
	if !f.Present() {
		return nil, ErrNoFile
	}
	if f.Size() > max {
		return nil, ErrFileTooLarge
	}

	content, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer content.Close() //nolint:errcheck // read-only; the read error is what matters

	// One byte past the limit is enough to know the file exceeds it; at
	// MaxInt64 nothing can exceed it, and the increment must not overflow.
	limit := max
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(content, limit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrFileTooLarge
	}
	return data, nil
}

// UnmarshalJSON accepts null and rejects other JSON values because files bind only from multipart requests.
func (f *File) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = File{}
		return nil
	}
	return errors.New("forms: file fields bind from multipart requests only")
}

var (
	fileType      = reflect.TypeFor[File]()
	fileSliceType = reflect.TypeFor[[]File]()
)

// rejectJSONFiles rejects a non-null JSON value for any []File field.
func rejectJSONFiles(dst any) error {
	rv := reflect.ValueOf(dst).Elem()
	if rv.Kind() != reflect.Struct {
		return nil
	}
	for _, f := range rv.Fields() {
		if f.Type() == fileSliceType && !f.IsNil() {
			return errors.New("forms: file fields bind from multipart requests only")
		}
	}
	return nil
}

// setFile binds only exact File and []File types, excluding defined types with the same shape.
func setFile(fv reflect.Value, headers []*multipart.FileHeader) bool {
	switch {
	case fv.Type() == fileType:
		if len(headers) > 0 {
			fv.Set(reflect.ValueOf(File{header: headers[0]}))
		}
		return true
	case fv.Type() == fileSliceType:
		if len(headers) > 0 {
			s := reflect.MakeSlice(fv.Type(), len(headers), len(headers))
			for i, h := range headers {
				s.Index(i).Set(reflect.ValueOf(File{header: h}))
			}
			fv.Set(s)
		}
		return true
	}
	return false
}
