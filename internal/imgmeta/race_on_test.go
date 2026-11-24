//go:build race

package imgmeta

// raceSlowdown 競態偵測模式下程式慢數倍,效能時限依此放寬;平方級實作在 10 MiB 輸入上仍遠超放寬後的時限.
const raceSlowdown = 10
