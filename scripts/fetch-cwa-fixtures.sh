#!/usr/bin/env bash
# [F24-99] 抓氣象署天氣特報的真實回傳，用來核對 internal/alert/testdata 的結構.
#
# 用法：bash scripts/fetch-cwa-fixtures.sh [輸出目錄]（預設 $HOME/cwa-fixtures）
# 執行後會要求輸入氣象署授權碼（輸入時不顯示）。授權碼：
#   - 只放在這個行程的記憶體裡，經管線交給 curl 當 Authorization 標頭，不出現在命令參數與命令歷史；
#   - 每個抓下來的檔都檢查不含授權碼，含的話刪掉該檔、以非 0 結束；
#   - 結束時清空剪貼簿（有 pbcopy 的話），避免從後台複製的授權碼留在剪貼簿.
# 抓下來的是公開的天氣特報資料；要更新測試資料時，人工比對後再複製進 internal/alert/testdata.
set -euo pipefail

out="${1:-$HOME/cwa-fixtures}"
base="${CWA_BASE_URL:-https://opendata.cwa.gov.tw/api/v1/rest/datastore}" # 只為本機假伺服器驗證而可覆寫
datasets=(W-C0033-001 W-C0033-003 W-C0033-004 W-C0033-006)

mkdir -p "$out"
read -rsp "CWA 授權碼: " key
echo
if [[ -z "$key" ]]; then
	echo "沒有輸入授權碼" >&2
	exit 1
fi

status=0
for d in "${datasets[@]}"; do
	file="$out/$d.json"
	printf 'Authorization: %s\n' "$key" | curl -sS -m 30 -H @- -o "$file" "$base/$d?format=JSON"
	body="$(<"$file")"
	if [[ "$body" == *"$key"* ]]; then
		rm -f "$file"
		echo "$d 警告：回傳內容含授權碼，已刪除該檔" >&2
		status=1
		continue
	fi
	echo "$d ${#body} 字元，不含授權碼"
done

unset key
if command -v pbcopy >/dev/null 2>&1; then
	pbcopy </dev/null
fi
exit "$status"
