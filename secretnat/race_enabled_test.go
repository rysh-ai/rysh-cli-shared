//go:build race

package secretnat

// raceEnabled reports whether this test binary was built with the race
// detector, which slows execution 10-20x — wall-clock assertions must scale
// their budgets accordingly.
const raceEnabled = true
