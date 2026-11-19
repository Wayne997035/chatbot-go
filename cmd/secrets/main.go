// Command secrets 是 user 在自己終端機執行的機敏設定加密工具（產 keyset、加密、解密檢查、輪替）.
//
// [F34-41] keyset 只經剪貼簿、明文只經隱藏輸入；stdout/stderr 不出現 keyset 與明文（decrypt 在真終端機是唯一例外）.
package main

import "os"

func main() {
	d := productionDeps()
	os.Exit(run(os.Args[1:], &d))
}
