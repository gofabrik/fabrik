package storage_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gofabrik/fabrik/storage"
	"github.com/gofabrik/fabrik/storage/storagetest"
)

func TestMemoryConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage { return storage.NewMemory() })
}

func TestLocalConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		s, err := storage.NewLocal(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// TestS3Conformance runs the storage contract against TEST_S3_ENDPOINT.
//
//	TEST_S3_ENDPOINT=http://localhost:9000 TEST_S3_ACCESS=... TEST_S3_SECRET=... go test
func TestS3Conformance(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	n := 0
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		n++
		bucket := fmt.Sprintf("conformance-%d-%d", os.Getpid(), n)
		s, err := storage.NewS3(storage.S3Options{
			Endpoint:      endpoint,
			Bucket:        bucket,
			AccessKey:     os.Getenv("TEST_S3_ACCESS"),
			SecretKey:     os.Getenv("TEST_S3_SECRET"),
			AllowInsecure: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateBucket(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	})
}
