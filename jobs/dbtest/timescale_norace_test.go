//go:build !race

package dbtest

// timeScale is 1 without the race detector; see the race build for why.
const timeScale = 1
