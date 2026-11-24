// Package imgmeta 在使用者照片送給 AI 供應商之前，剝掉 JPEG 的隱私中繼資料. [F37-34]
//
// 只用標準庫，不重新編碼：逐段走訪 JPEG marker（https://en.wikipedia.org/wiki/JPEG#Syntax_and_structure），
// 用白名單保留解碼需要的段落，其餘（EXIF 含 GPS、XMP、IPTC、註解、縮圖、MPF 副圖、EOI 之後的尾巴）一律丟掉.
// Exif 在 APP1（https://en.wikipedia.org/wiki/Exif）；Orientation 另以一個 36 位元組的合成 APP1 保留，
// 因為像素是依感光方向存的，顯示方向只寫在 Orientation 裡.
package imgmeta

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// ErrDamaged JPEG 結構走不完（截斷、段落長度不合理、缺 EOI 等）. 呼叫端必須 fail-closed，不可把原檔送出去.
var ErrDamaged = errors.New("JPEG 結構損壞，無法剝除中繼資料")

const (
	markerSOI  = 0xD8
	markerEOI  = 0xD9
	markerSOS  = 0xDA
	markerAPP0 = 0xE0
	markerAPP1 = 0xE1
	markerAPP2 = 0xE2
	markerAPPE = 0xEE
)

// orientationAPP1Len 合成 APP1 的總長：2 marker + 2 長度 + 32 payload.
const orientationAPP1Len = 36

// IsJPEG 以魔術位元組（FF D8 FF）判斷，不看 MIME 欄位.
func IsJPEG(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xFF && data[1] == markerSOI && data[2] == 0xFF
}

// Sanitize 回傳剝除中繼資料後的新切片，不修改 data. 非 JPEG 原封不動回傳（同一切片、nil error）；
// JPEG 結構壞掉時回 ErrDamaged 與 nil 切片. 冪等：Sanitize(Sanitize(x)) 與 Sanitize(x) 逐位元組相同.
func Sanitize(data []byte) ([]byte, error) {
	if !IsJPEG(data) {
		return data, nil
	}
	var (
		kept        [][]byte // 要保留的段落（指向 data 的子切片，標頭段落含 marker）
		firstIsJFIF bool
		orientation byte
		pos         = 2
		done        bool
	)
	for !done {
		marker, segStart, next, err := nextMarker(data, pos)
		if err != nil {
			return nil, err
		}
		switch {
		case marker == markerEOI:
			kept = append(kept, data[segStart:next])
			done = true
		case isStandalone(marker):
			// RST0-7 與 TEM 沒有長度；RST 照白名單保留，TEM 丟掉.
			if marker >= 0xD0 && marker <= 0xD7 {
				kept = append(kept, data[segStart:next])
			}
			pos = next
		default:
			// 有長度的段落：data[segStart:next] 是 marker + 長度 + payload.
			payload := data[segStart+4 : next]
			if marker == markerAPP1 && orientation == 0 {
				orientation = exifOrientation(payload)
			}
			if !keepSegment(marker, payload) {
				pos = next
				continue
			}
			if len(kept) == 0 && marker == markerAPP0 {
				firstIsJFIF = true
			}
			kept = append(kept, data[segStart:next])
			pos = next
			if marker == markerSOS {
				end, err := scanEnd(data, pos)
				if err != nil {
					return nil, err
				}
				kept = append(kept, data[pos:end])
				pos = end
			}
		}
	}
	return assemble(kept, firstIsJFIF, orientation), nil
}

// assemble 組出輸出；orientation 為 0 表示不需要合成 APP1.
func assemble(kept [][]byte, firstIsJFIF bool, orientation byte) []byte {
	size := 2
	for _, k := range kept {
		size += len(k)
	}
	if orientation != 0 {
		size += orientationAPP1Len
	}
	out := make([]byte, 0, size)
	out = append(out, 0xFF, markerSOI)
	rest := kept
	if orientation != 0 {
		if firstIsJFIF {
			out = append(out, rest[0]...)
			rest = rest[1:]
		}
		out = appendOrientation(out, orientation)
	}
	for _, k := range rest {
		out = append(out, k...)
	}
	return out
}

// nextMarker 從 pos 找下一個 marker：回傳 marker 位元組、該段起點（marker 的 0xFF）、下一段起點.
// 有長度的段落會檢查長度 >= 2 且不超出結尾.
func nextMarker(data []byte, pos int) (marker byte, start, next int, err error) {
	if pos >= len(data) || data[pos] != 0xFF {
		return 0, 0, 0, ErrDamaged
	}
	// 跳過填充用的連續 0xFF，起點取最後一個 0xFF.
	for pos+1 < len(data) && data[pos+1] == 0xFF {
		pos++
	}
	if pos+1 >= len(data) {
		return 0, 0, 0, ErrDamaged
	}
	marker, start = data[pos+1], pos
	if marker == 0x00 || marker == markerSOI {
		return 0, 0, 0, ErrDamaged
	}
	if marker == markerEOI || isStandalone(marker) {
		return marker, start, start + 2, nil
	}
	if start+4 > len(data) {
		return 0, 0, 0, ErrDamaged
	}
	segLen := int(binary.BigEndian.Uint16(data[start+2:]))
	if segLen < 2 || start+2+segLen > len(data) {
		return 0, 0, 0, ErrDamaged
	}
	return marker, start, start + 2 + segLen, nil
}

// isStandalone 沒有長度欄位的 marker：TEM 與 RST0-7.
func isStandalone(m byte) bool { return m == 0x01 || (m >= 0xD0 && m <= 0xD7) }

// keepSegment 白名單：回傳 true 的段落才進輸出. 白名單而不是黑名單，未知的廠商段落不會夾帶資料出去.
func keepSegment(marker byte, payload []byte) bool {
	switch {
	case marker >= 0xC0 && marker <= 0xCF:
		return marker != 0xC8 // SOF0-15 加上 C4（DHT）與 CC（DAC）；C8 是保留給 JPEG 擴充的 marker，不在白名單.
	case marker == 0xDB, marker == 0xDC, marker == 0xDD, marker == 0xDE, marker == 0xDF, marker == markerSOS:
		return true // DQT DNL DRI DHP EXP SOS
	case marker == markerAPP0:
		return bytes.HasPrefix(payload, []byte("JFIF\x00"))
	case marker == markerAPP2:
		return bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00"))
	case marker == markerAPPE:
		return bytes.HasPrefix(payload, []byte("Adobe"))
	}
	return false
}

// scanEnd 回傳 SOS 之後熵編碼資料的結尾（下一個真正 marker 的 0xFF 位置）. 到結尾都沒有 marker 視為截斷.
func scanEnd(data []byte, pos int) (int, error) {
	for i := pos; i < len(data); {
		if data[i] != 0xFF {
			i++
			continue
		}
		if i+1 >= len(data) {
			return 0, ErrDamaged
		}
		switch n := data[i+1]; {
		case n == 0x00, n >= 0xD0 && n <= 0xD7:
			i += 2
		case n == 0xFF:
			i++
		default:
			return i, nil
		}
	}
	return 0, ErrDamaged
}

// exifOrientation 從 Exif APP1 payload 讀 IFD0 的 Orientation（2-8 才回傳，其餘與解析失敗都回 0）.
func exifOrientation(payload []byte) byte {
	const hdr = "Exif\x00\x00"
	if !bytes.HasPrefix(payload, []byte(hdr)) {
		return 0
	}
	tiff := payload[len(hdr):]
	if len(tiff) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(tiff[2:]) != 0x2A {
		return 0
	}
	ifd := uint64(bo.Uint32(tiff[4:]))
	if ifd+2 > uint64(len(tiff)) {
		return 0
	}
	count := uint64(bo.Uint16(tiff[ifd:]))
	for i := range count {
		e := ifd + 2 + i*12
		if e+12 > uint64(len(tiff)) {
			return 0
		}
		if bo.Uint16(tiff[e:]) != 0x0112 {
			continue
		}
		if bo.Uint16(tiff[e+2:]) != 3 || bo.Uint32(tiff[e+4:]) != 1 {
			return 0
		}
		if v := bo.Uint16(tiff[e+8:]); v >= 2 && v <= 8 {
			return byte(v)
		}
		return 0
	}
	return 0
}

// appendOrientation 附上只含 Orientation 的 36 位元組 APP1（沒有 GPS、縮圖、maker note）.
func appendOrientation(out []byte, v byte) []byte {
	return append(out,
		0xFF, markerAPP1, 0x00, 0x22,
		'E', 'x', 'i', 'f', 0, 0,
		'M', 'M', 0, 0x2A, 0, 0, 0, 8, // TIFF header，IFD0 在 offset 8
		0, 1, // 1 個 entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, v, 0, 0, // Orientation SHORT x1
		0, 0, 0, 0, // 沒有下一個 IFD
	)
}
