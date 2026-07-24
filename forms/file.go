package forms

import (
	"errors"
	"io"
	"mime/multipart"
	"path/filepath"
	"reflect"
	"strings"
)

// ErrNoFile is returned by [File.Open] when no file was submitted.
var ErrNoFile = errors.New("forms: no file submitted")

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

// UnmarshalJSON accepts null and rejects other JSON values because files bind only from multipart requests.
func (f *File) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = File{}
		return nil
	}
	return errors.New("forms: file fields bind from multipart requests only")
}

var (
	fileType      = reflect.TypeOf(File{})
	fileSliceType = reflect.TypeOf([]File{})
)

// rejectJSONFiles rejects a non-null JSON value for any []File field.
func rejectJSONFiles(dst any) error {
	rv := reflect.ValueOf(dst).Elem()
	if rv.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
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
