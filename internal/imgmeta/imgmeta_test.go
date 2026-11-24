package imgmeta

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"runtime"
	"testing"
	"time"
)

// order 位元組序（讀寫都要）.
type order interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// ---- 夾具：全部合成，座標與內容都是假的，沒有任何使用者照片 ----

func seg(marker byte, payload []byte) []byte {
	n := len(payload) + 2
	if n > math.MaxUint16 {
		panic("fixture segment too large")
	}
	out := binary.BigEndian.AppendUint16([]byte{0xFF, marker}, uint16(n))
	return append(out, payload...)
}

// baseJPEG 用標準庫編碼一張 16x16 漸層：SOI、DQT、SOF0、DHT、SOS、掃描資料、EOI，沒有 JFIF 與任何中繼資料.
func baseJPEG(t testing.TB) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := range 16 {
		for x := range 16 {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 16), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// insertAfterSOI 把 segs 依序放在 SOI 之後.
func insertAfterSOI(base []byte, segs ...[]byte) []byte {
	out := append([]byte{}, base[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, base[2:]...)
}

// exifPayload 組一個 Exif APP1 payload：IFD0 有 Orientation（orientation 為 0 時省略）與 GPSInfo 指標，GPS 區放 GPSSENTINEL.
func exifPayload(bo order, orientation uint16) []byte {
	order := "MM"
	if bo == binary.LittleEndian {
		order = "II"
	}
	tiff := []byte(order)
	tiff = bo.AppendUint16(tiff, 0x2A)
	tiff = bo.AppendUint32(tiff, 8)
	entries := 1
	if orientation != 0 {
		entries = 2
	}
	tiff = bo.AppendUint16(tiff, uint16(entries))
	gpsOff := uint32(8 + 2 + entries*12 + 4)
	if orientation != 0 {
		tiff = bo.AppendUint16(tiff, 0x0112)
		tiff = bo.AppendUint16(tiff, 3)
		tiff = bo.AppendUint32(tiff, 1)
		tiff = bo.AppendUint16(tiff, orientation)
		tiff = bo.AppendUint16(tiff, 0)
	}
	tiff = bo.AppendUint16(tiff, 0x8825)
	tiff = bo.AppendUint16(tiff, 4)
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint32(tiff, gpsOff)
	tiff = bo.AppendUint32(tiff, 0)
	tiff = append(tiff, []byte("GPSSENTINEL")...)
	return append([]byte("Exif\x00\x00"), tiff...)
}

var sentinels = []string{"GPSSENTINEL", "XMPSENTINEL", "IPTCSENTINEL", "COMSENTINEL", "TRAILERSENTINEL"}

// metadataJPEG 手機照片的形狀：JFIF、Exif(GPS+Orientation)、XMP、IPTC、COM、MPF、ICC、本體、EOI、EOI 後的副圖.
func metadataJPEG(t testing.TB, bo order, orientation uint16) []byte {
	t.Helper()
	base := baseJPEG(t)
	jfif := seg(0xE0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))
	out := insertAfterSOI(base,
		jfif,
		seg(0xE1, exifPayload(bo, orientation)),
		seg(0xE1, append([]byte("http://ns.adobe.com/xap/1.0/\x00"), "XMPSENTINEL"...)),
		seg(0xED, []byte("Photoshop 3.0\x00IPTCSENTINEL")),
		seg(0xFE, []byte("COMSENTINEL")),
		seg(0xE2, []byte("MPF\x00mpfdata")),
		seg(0xE2, []byte("ICC_PROFILE\x00\x01\x01ICCSENTINEL")),
		seg(0xEA, []byte("AROT vendor")),
	)
	return append(out, 0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x14, 'T', 'R', 'A', 'I', 'L', 'E', 'R', 'S', 'E', 'N', 'T', 'I', 'N', 'E', 'L', 0xFF, 0xD9)
}

// exifSegments 回傳輸出裡 Exif APP1 的數量與第一個的 Orientation（IFD0 第一個 entry 的值，位元組序 MM）.
func exifSegments(out []byte) (count int, orientation byte, length int) {
	marker := []byte("Exif\x00\x00")
	for i := 0; i+len(marker) < len(out); i++ {
		if !bytes.HasPrefix(out[i:], marker) {
			continue
		}
		count++
		if count == 1 && i >= 4 && i+6+20 <= len(out) {
			length = int(binary.BigEndian.Uint16(out[i-2:])) + 2
			orientation = out[i+6+19]
		}
	}
	return count, orientation, length
}

func decodePixels(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

func samePixels(t *testing.T, a, b image.Image) {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("bounds differ: %v vs %v", a.Bounds(), b.Bounds())
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				t.Fatalf("pixel (%d,%d) differs", x, y)
			}
		}
	}
}

// ---- 測試 ----

// [F37-34] 五種哨兵字串都不得出現在輸出；輸出剛好一個 36 位元組的 Exif、Orientation 6.
func TestSanitizeStripsMetadata(t *testing.T) {
	in := metadataJPEG(t, binary.BigEndian, 6)
	for _, s := range sentinels {
		if !bytes.Contains(in, []byte(s)) {
			t.Fatalf("fixture lacks sentinel %s", s)
		}
	}
	out, err := Sanitize(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sentinels {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("output still contains %s", s)
		}
	}
	if !bytes.HasPrefix(out, []byte{0xFF, 0xD8}) || !bytes.HasSuffix(out, []byte{0xFF, 0xD9}) {
		t.Errorf("output must start with SOI and end with EOI")
	}
	count, orientation, length := exifSegments(out)
	if count != 1 || length != 36 || orientation != 6 {
		t.Errorf("exif segments = %d, length = %d, orientation = %d; want 1, 36, 6", count, length, orientation)
	}
	if bytes.Contains(out, []byte("MPF\x00")) || bytes.Contains(out, []byte("AROT")) {
		t.Errorf("MPF or vendor APP10 survived")
	}
}

// [F37-34] 像素不變、ICC 保留.
func TestSanitizeKeepsPixelsAndICC(t *testing.T) {
	in := metadataJPEG(t, binary.BigEndian, 6)
	out, err := Sanitize(in)
	if err != nil {
		t.Fatal(err)
	}
	samePixels(t, decodePixels(t, in), decodePixels(t, out))
	if !bytes.Contains(out, []byte("ICC_PROFILE\x00")) || !bytes.Contains(out, []byte("ICCSENTINEL")) {
		t.Errorf("ICC profile dropped")
	}
	if !bytes.Contains(out, []byte("JFIF\x00")) {
		t.Errorf("JFIF dropped")
	}
}

// [F37-34] Pillow 產生的漸進式 JPEG（假座標）：多個掃描都在、像素相同、Exif 的 GPS 消失.
func TestSanitizeProgressive(t *testing.T) {
	in, err := os.ReadFile("testdata/progressive_exif.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(in, []byte("Exif\x00\x00")) {
		t.Fatal("fixture has no Exif")
	}
	out, err := Sanitize(in)
	if err != nil {
		t.Fatal(err)
	}
	samePixels(t, decodePixels(t, in), decodePixels(t, out))
	count, orientation, length := exifSegments(out)
	if count > 1 || (count == 1 && (length != 36 || orientation < 2)) {
		t.Errorf("exif count %d length %d orientation %d: only a bare Orientation APP1 may remain", count, length, orientation)
	}
	// 掃描段不得被截斷：SOS 的數量與輸入相同（漸進式有多個）.
	if a, b := countMarker(in, 0xDA), countMarker(out, 0xDA); a != b || a < 2 {
		t.Errorf("SOS count in=%d out=%d, want equal and >= 2", a, b)
	}
}

func countMarker(data []byte, m byte) int {
	n := 0
	for i := 0; i+1 < len(data); i++ {
		if data[i] == 0xFF && data[i+1] == m {
			n++
		}
	}
	return n
}

// [F37-34] Orientation 1 或沒有 Orientation 時不補合成 APP1，輸出沒有任何 Exif.
func TestSanitizeOrientationOmittedWhenNormal(t *testing.T) {
	for name, o := range map[string]uint16{"orientation_1": 1, "no_orientation": 0} {
		t.Run(name, func(t *testing.T) {
			out, err := Sanitize(metadataJPEG(t, binary.BigEndian, o))
			if err != nil {
				t.Fatal(err)
			}
			if count, _, _ := exifSegments(out); count != 0 {
				t.Errorf("exif segments = %d, want 0", count)
			}
			if bytes.Contains(out, []byte("GPSSENTINEL")) {
				t.Errorf("GPS survived")
			}
		})
	}
}

// [F37-34] 大小端的 Exif 都要讀得到 Orientation 6，輸出恰一個 36 位元組 Exif.
func TestSanitizeKeepsOrientationBothByteOrders(t *testing.T) {
	for name, bo := range map[string]order{"MM_big_endian": binary.BigEndian, "II_little_endian": binary.LittleEndian} {
		t.Run(name, func(t *testing.T) {
			out, err := Sanitize(metadataJPEG(t, bo, 6))
			if err != nil {
				t.Fatal(err)
			}
			count, orientation, length := exifSegments(out)
			if count != 1 || length != 36 || orientation != 6 {
				t.Errorf("exif segments = %d, length = %d, orientation = %d; want 1, 36, 6", count, length, orientation)
			}
		})
	}
}

// [F37-34] 再剝一次得到逐位元組相同的結果.
func TestSanitizeIdempotent(t *testing.T) {
	for name, in := range map[string][]byte{
		"with_orientation": metadataJPEG(t, binary.BigEndian, 6),
		"no_orientation":   metadataJPEG(t, binary.BigEndian, 0),
		"no_jfif":          insertAfterSOI(baseJPEG(t), seg(0xE1, exifPayload(binary.LittleEndian, 8))),
	} {
		t.Run(name, func(t *testing.T) {
			once, err := Sanitize(in)
			if err != nil {
				t.Fatal(err)
			}
			twice, err := Sanitize(once)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(once, twice) {
				t.Errorf("not idempotent: %d bytes then %d bytes", len(once), len(twice))
			}
		})
	}
}

// [F37-34] 沒有任何中繼資料的照片原封不動.
func TestSanitizeNoMetadataUnchanged(t *testing.T) {
	in := baseJPEG(t)
	out, err := Sanitize(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("clean JPEG changed: %d bytes in, %d bytes out", len(in), len(out))
	}
}

// [F37-34] 壞檔一律回錯、不回任何位元組（fail-closed）.
func TestSanitizeRejectsDamaged(t *testing.T) {
	cases := map[string][]byte{
		"truncated_before_EOI": baseJPEG(t)[:len(baseJPEG(t))-2],
		"truncated_in_scan":    baseJPEG(t)[:len(baseJPEG(t))/2],
		"segment_length_1":     {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x01, 0, 0},
		"segment_length_0":     {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x00, 0, 0},
		"length_past_end":      {0xFF, 0xD8, 0xFF, 0xE1, 0x7F, 0xFF, 'E', 'x', 'i', 'f'},
		"magic_plus_3_bytes":   {0xFF, 0xD8, 0xFF, 0x01, 0x02, 0x03},
		"magic_only":           {0xFF, 0xD8, 0xFF},
		"junk_between_markers": {0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 'J', 'F', 0x12, 0x34, 0x56},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := Sanitize(in)
			if !errors.Is(err, ErrDamaged) {
				t.Fatalf("err = %v, want ErrDamaged", err)
			}
			if out != nil {
				t.Errorf("damaged input must produce no output bytes, got %d", len(out))
			}
		})
	}
}

// [F37-34] PNG 與 GIF（以及空資料）原封不動、不是錯誤.
func TestSanitizePassesNonJPEG(t *testing.T) {
	cases := map[string][]byte{
		"png":            {0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0x0D, 'I', 'H', 'D', 'R'},
		"gif":            []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"empty":          {},
		"soi_only_no_ff": {0xFF, 0xD8},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := Sanitize(in)
			if err != nil {
				t.Fatalf("non-JPEG must not be an error: %v", err)
			}
			if !bytes.Equal(in, out) {
				t.Errorf("non-JPEG changed")
			}
		})
	}
}

// [F37-34] Sanitize 不得修改呼叫端的切片.
func TestSanitizeDoesNotMutateInput(t *testing.T) {
	in := metadataJPEG(t, binary.BigEndian, 6)
	snapshot := append([]byte{}, in...)
	if _, err := Sanitize(in); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, snapshot) {
		t.Errorf("input mutated")
	}
}

// [F37-34] 夾具的每個前綴都只能回錯或有效輸出，不 panic.
func TestSanitizeEveryPrefixNeverPanics(t *testing.T) {
	full := metadataJPEG(t, binary.LittleEndian, 3)
	for n := range len(full) + 1 {
		out, err := Sanitize(full[:n])
		if err == nil && IsJPEG(full[:n]) {
			if again, err2 := Sanitize(out); err2 != nil || !bytes.Equal(again, out) {
				t.Fatalf("prefix %d: accepted output is not stable (%v)", n, err2)
			}
		}
	}
}

// [F37-34] 10 MiB 的假 JPEG：300 ms 內完成、配置不超過約 2 倍輸入（只在 O(n^2) 或多重複製時紅）.
func TestSanitizeLargeInputBound(t *testing.T) {
	const size = 10 << 20
	in := []byte{0xFF, 0xD8}
	in = append(in, seg(0xC0, []byte{8, 0, 16, 0, 16, 1, 1, 0x11, 0})...)
	in = append(in, seg(0xDA, []byte{1, 1, 0, 0, 63, 0})...)
	in = append(in, bytes.Repeat([]byte{0x55}, size)...)
	in = append(in, 0xFF, 0xD9)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	out, err := Sanitize(in)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Errorf("clean large input changed")
	}
	if limit := 300 * time.Millisecond * raceSlowdown; elapsed > limit {
		t.Errorf("Sanitize took %v, want < %v", elapsed, limit)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 2*size+(1<<20) {
		t.Errorf("allocated %d bytes for %d byte input, want <= ~2x", alloc, size)
	}
}
