module github.com/gofabrik/fabrik/auth/store/sqlite

go 1.26

require (
	github.com/gofabrik/fabrik/auth/password v0.0.0
	modernc.org/sqlite v1.53.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gofabrik/fabrik/auth v0.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.43.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	modernc.org/libc v1.73.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/gofabrik/fabrik/auth/password => ../../password

replace github.com/gofabrik/fabrik/auth => ../../
