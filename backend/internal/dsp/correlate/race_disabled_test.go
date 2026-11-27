//go:build !race

package correlate

// raceEnabled is false in a normal build; it is the counterpart of
// race_enabled_test.go, and exists so the timing gate can tell the two builds
// apart and skip itself when instrumentation would decide the measurement.
const raceEnabled = false
