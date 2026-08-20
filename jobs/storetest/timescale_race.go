//go:build race

package storetest

// timeScale stretches lease durations and deadlines so the race
// detector's scheduling overhead does not expire leases mid-test.
const timeScale = 10
