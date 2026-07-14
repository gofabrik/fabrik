package migrations

import (
	"errors"
	"testing"
	"testing/fstest"
)

func TestCheck(t *testing.T) {
	ok := Sources{{Stream: "auth", FS: fstest.MapFS{"0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}}}
	if err := ok.Check(); err != nil {
		t.Fatalf("valid sources: %v", err)
	}

	cases := []struct {
		name string
		srcs Sources
		want error
	}{
		{"empty", Sources{}, ErrInvalidSource},
		{"nil FS", Sources{{Stream: "web"}}, ErrInvalidSource},
		{"absolute dir", Sources{{Stream: "web", FS: fstest.MapFS{}, Dir: "/m"}}, ErrInvalidSource},
		{"dotdot dir", Sources{{Stream: "web", FS: fstest.MapFS{}, Dir: "../m"}}, ErrInvalidSource},
		{"bad stream", Sources{{Stream: "web//x", FS: fstest.MapFS{}}}, ErrInvalidSource},
		{"duplicate stream", Sources{{Stream: "web", FS: fstest.MapFS{}}, {Stream: "web", FS: fstest.MapFS{}}}, ErrDuplicateStream},
		{"bad filename", Sources{{Stream: "web", FS: fstest.MapFS{"nope.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}}}, ErrInvalidFilename},
		{"duplicate version", Sources{{Stream: "web", FS: fstest.MapFS{
			"0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			"0001_b.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		}}}, ErrDuplicateVersion},
		{"nested directory", Sources{{Stream: "web", FS: fstest.MapFS{
			"auth/0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		}}}, ErrInvalidSource},
	}
	for _, tc := range cases {
		if err := tc.srcs.Check(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}
