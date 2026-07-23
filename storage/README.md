# storage

Package `storage` provides standard-library blob storage with memory,
local, and S3-compatible backends.

```go
func save(ctx context.Context, dir string, avatar io.Reader) error {
	store, err := storage.NewLocal(dir)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Put(ctx, "avatars/42.png", avatar); err != nil {
		return err
	}
	rc, err := store.Open(ctx, "avatars/42.png")
	if err != nil {
		return err
	}
	defer rc.Close()

	for info, err := range store.List(ctx, "avatars/") {
		if err != nil {
			return err
		}
		fmt.Println(info.Key, info.Size)
	}
	return nil
}
```

## Keys

Keys are slash-separated identifiers, not filesystem paths. They
cannot be empty or absolute, contain empty segments, backslashes, or
control bytes, or have segments beginning with `.`. The dot namespace
is reserved for backends. Use `CheckKey` for early validation. `List`
accepts a key prefix and yields entries in lexical order; directories
are backend details.

## Semantics

`Put` replaces atomically, so open readers retain their version. A
failed `Put` leaves no partial blob. `Delete` is idempotent and frees
the key's prefix for reuse. Missing keys wrap `ErrNotExist`, an alias
of `fs.ErrNotExist`. Operations reject pre-canceled contexts before
side effects. `List` yields one error and stops.

Readers implement `io.ReadSeeker` when supported by the backend, which
allows range serving with `http.ServeContent`.

## Backends

`Memory` is process-local and intended for tests and development.
`Local` uses `os.Root` to contain keys and symlinks and reserves
`.tmp/` for temporary files. It inherits filesystem restrictions and
collation, and a key cannot coexist with another key for which it is a
path prefix.

`S3` implements path-style S3-compatible storage with SigV4 and
ListObjectsV2 using only the standard library:

```go
store, err := storage.NewS3(storage.S3Options{
	Endpoint:  "https://s3.eu-central-1.amazonaws.com",
	Bucket:    "my-app-files",
	AccessKey: accessKey,
	SecretKey: secretKey,
	Region:    "eu-central-1",
})
```

Endpoints must be `scheme://host[:port]`. HTTPS is required unless
`AllowInsecure` is set because `UNSIGNED-PAYLOAD` relies on transport
integrity. `CreateBucket` is intended for development and tests;
production buckets are provisioned externally.

To run the conformance suite against a local MinIO:

```
docker run -d --rm -p 9000:9000 -e MINIO_ROOT_USER=admin   -e MINIO_ROOT_PASSWORD=secret minio/minio server /data
TEST_S3_ENDPOINT=http://localhost:9000 TEST_S3_ACCESS=admin   TEST_S3_SECRET=secret go test ./...
```

The `storage/storagetest` package runs the shared contract against
custom backends:

```go
func TestMyStorage(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage { ... })
}
```
