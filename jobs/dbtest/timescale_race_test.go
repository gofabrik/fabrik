//go:build race

package dbtest

// timeScale gives race-instrumented SQLite operations more deadline headroom.
const timeScale = 10
