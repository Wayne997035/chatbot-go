//go:build race

package redact

// raceSlowdown 競態偵測模式下程式慢數倍,效能時限依此放寬;災難性比對在 100 KB 輸入上仍遠超放寬後的時限.
const raceSlowdown = 10
