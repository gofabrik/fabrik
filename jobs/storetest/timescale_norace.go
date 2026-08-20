//go:build !race

package storetest

// timeScale is 1 without the race detector; see the race build for why.
const timeScale = 1
