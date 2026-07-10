package migrations

import (
	"errors"
	"testing"
	"testing/fstest"
)

func TestCheck(t *testing.T) {
	ok := Sources{{Module: "auth", FS: fstest.MapFS{"0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}}}
	if err := ok.Check(); err != nil {
		t.Fatalf("valid sources: %v", err)
	}

	cases := []struct {
		name string
		srcs Sources
		want error
	}{
		{"empty", Sources{}, ErrInvalidSource},
		{"nil FS", Sources{{Module: "web"}}, ErrInvalidSource},
		{"absolute dir", Sources{{Module: "web", FS: fstest.MapFS{}, Dir: "/m"}}, ErrInvalidSource},
		{"dotdot dir", Sources{{Module: "web", FS: fstest.MapFS{}, Dir: "../m"}}, ErrInvalidSource},
		{"bad module", Sources{{Module: "web//x", FS: fstest.MapFS{}}}, ErrInvalidSource},
		{"duplicate module", Sources{{Module: "web", FS: fstest.MapFS{}}, {Module: "web", FS: fstest.MapFS{}}}, ErrDuplicateModule},
		{"bad filename", Sources{{Module: "web", FS: fstest.MapFS{"nope.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}}}, ErrInvalidFilename},
		{"duplicate version", Sources{{Module: "web", FS: fstest.MapFS{
			"0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			"0001_b.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		}}}, ErrDuplicateVersion},
		{"nested directory", Sources{{Module: "web", FS: fstest.MapFS{
			"auth/0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		}}}, ErrInvalidSource},
	}
	for _, tc := range cases {
		if err := tc.srcs.Check(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}
