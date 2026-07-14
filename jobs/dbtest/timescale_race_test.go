//go:build race

package dbtest

// timeScale stretches test deadlines under the race detector, whose
// instrumentation makes SQLite calls several times slower. Deadlines are
// upper bounds - a passing condition returns early - so overshooting here
// costs nothing on success and only buys headroom on genuine failures.
const timeScale = 10
