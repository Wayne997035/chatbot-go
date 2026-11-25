//go:build !race

package redact

// raceSlowdown 一般模式不放寬效能時限.
const raceSlowdown = 1
