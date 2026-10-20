package host

import "encoding/binary"

// imageSize 从响应头几个字节里读出宽高（JPEG/PNG/GIF/WEBP）。
// 用途：判断“服务端直接返回的图片”是不是占位图/缩略图，避免把它当原图存下来。
func imageSize(d []byte) (w, h int, ok bool) {
	if len(d) < 16 {
		return 0, 0, false
	}
	switch {
	case d[0] == 0xFF && d[1] == 0xD8: // JPEG
		i := 2
		for i+9 < len(d) {
			if d[i] != 0xFF {
				i++
				continue
			}
			m := d[i+1]
			if m == 0xD8 || m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
				i += 2
				continue
			}
			if i+4 > len(d) {
				return 0, 0, false
			}
			segLen := int(binary.BigEndian.Uint16(d[i+2 : i+4]))
			if segLen < 2 {
				return 0, 0, false
			}
			if (m >= 0xC0 && m <= 0xCF) && m != 0xC4 && m != 0xC8 && m != 0xCC {
				if i+9 > len(d) {
					return 0, 0, false
				}
				h = int(binary.BigEndian.Uint16(d[i+5 : i+7]))
				w = int(binary.BigEndian.Uint16(d[i+7 : i+9]))
				return w, h, w > 0 && h > 0
			}
			i += 2 + segLen
		}
	case string(d[0:8]) == "\x89PNG\r\n\x1a\n":
		return int(binary.BigEndian.Uint32(d[16:20])), int(binary.BigEndian.Uint32(d[20:24])), true
	case string(d[0:3]) == "GIF":
		return int(binary.LittleEndian.Uint16(d[6:8])), int(binary.LittleEndian.Uint16(d[8:10])), true
	case string(d[0:4]) == "RIFF" && string(d[8:12]) == "WEBP":
		if string(d[12:16]) == "VP8X" && len(d) >= 30 {
			w := int(uint32(d[24])|uint32(d[25])<<8|uint32(d[26])<<16) + 1
			h := int(uint32(d[27])|uint32(d[28])<<8|uint32(d[29])<<16) + 1
			return w, h, true
		}
	}
	return 0, 0, false
}

// looksLikePlaceholderSize 判断尺寸像不像占位图/缩略图（imagevenue 的
// "Thumbnail Temporarily Unavailable" 就是 240x240，imagetwist 的防盗图是 200x300）。
func looksLikePlaceholderSize(w, h int) bool {
	if w <= 0 || h <= 0 {
		return false
	}
	mn, mx := w, h
	if mn > mx {
		mn, mx = mx, mn
	}
	return mn <= 260 && mx <= 420
}
