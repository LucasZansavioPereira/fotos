package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// metadataDate lê a data de captura gravada dentro do arquivo: EXIF de fotos
// (JPEG, HEIC, PNG, WebP...) ou os metadados de vídeos MOV/MP4.
func metadataDate(path string) (time.Time, string, bool) {
	if t, ok := exifDate(path); ok {
		return t, "exif", true
	}
	if t, ok := videoDate(path); ok {
		return t, "video", true
	}
	if t, ok := embeddedExifDate(path); ok {
		return t, "exif", true
	}
	return time.Time{}, "", false
}

// embeddedExifDate procura um bloco EXIF (cabeçalho TIFF) no começo do arquivo.
// Serve para HEIC, PNG e WebP, onde o EXIF fica dentro de outra estrutura.
func embeddedExifDate(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, 16<<20))
	if err != nil {
		return time.Time{}, false
	}
	headers := [][]byte{[]byte("MM\x00\x2a\x00\x00\x00\x08"), []byte("II\x2a\x00\x08\x00\x00\x00")}
	tries := 0
	for _, h := range headers {
		for off := 0; tries < 20; tries++ {
			i := bytes.Index(buf[off:], h)
			if i < 0 {
				break
			}
			start := off + i
			if x, err := exif.Decode(bytes.NewReader(buf[start:])); err == nil {
				if t, err := x.DateTime(); err == nil && t.Year() >= 1900 {
					return t, true
				}
			}
			off = start + 1
		}
	}
	return time.Time{}, false
}

// videoDate lê a data de gravação de MOV/MP4. Prefere a chave da Apple
// com.apple.quicktime.creationdate, que traz a hora local com fuso; se não
// houver, usa o creation_time do mvhd, que é em UTC.
func videoDate(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	moov, ok := findBox(f, 0, info.Size(), "moov")
	if !ok || moov.size > 64<<20 {
		return time.Time{}, false
	}
	data := make([]byte, moov.size)
	if _, err := f.ReadAt(data, moov.start); err != nil {
		return time.Time{}, false
	}
	if t, ok := appleCreationDate(data); ok {
		return t, true
	}
	if t, ok := mvhdDate(data); ok {
		return t, true
	}
	return time.Time{}, false
}

type box struct {
	start, size int64 // conteúdo da caixa, sem o cabeçalho
}

// findBox procura uma caixa de primeiro nível entre from e to.
func findBox(r io.ReaderAt, from, to int64, typ string) (box, bool) {
	hdr := make([]byte, 16)
	for off := from; off+8 <= to; {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return box{}, false
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		name := string(hdr[4:8])
		headerLen := int64(8)
		switch size {
		case 1:
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return box{}, false
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			size = to - off
		}
		if size < headerLen || off+size > to {
			return box{}, false
		}
		if name == typ {
			return box{off + headerLen, size - headerLen}, true
		}
		off += size
	}
	return box{}, false
}

// child procura uma caixa filha dentro de data (que já está em memória).
func child(data []byte, typ string) ([]byte, bool) {
	b, ok := findBox(bytes.NewReader(data), 0, int64(len(data)), typ)
	if !ok {
		return nil, false
	}
	return data[b.start : b.start+b.size], true
}

var qtEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

func mvhdDate(moov []byte) (time.Time, bool) {
	mvhd, ok := child(moov, "mvhd")
	if !ok || len(mvhd) < 12 {
		return time.Time{}, false
	}
	var secs uint64
	if mvhd[0] == 1 {
		secs = binary.BigEndian.Uint64(mvhd[4:12])
	} else {
		secs = uint64(binary.BigEndian.Uint32(mvhd[4:8]))
	}
	t := qtEpoch.Add(time.Duration(secs) * time.Second)
	// Zero, ou datas absurdas, significam que a câmera não gravou a data.
	if secs == 0 || t.Year() < 1990 || t.After(time.Now().Add(48*time.Hour)) {
		return time.Time{}, false
	}
	return t.Local(), true
}

func appleCreationDate(moov []byte) (time.Time, bool) {
	meta, ok := child(moov, "meta")
	if !ok {
		if udta, ok2 := child(moov, "udta"); ok2 {
			meta, ok = child(udta, "meta")
		}
		if !ok {
			return time.Time{}, false
		}
	}
	// No MP4 o meta tem 4 bytes de versão/flags antes das filhas; no QuickTime não.
	if len(meta) >= 12 && string(meta[4:8]) != "hdlr" && string(meta[8:12]) == "hdlr" {
		meta = meta[4:]
	}
	keys, ok1 := child(meta, "keys")
	ilst, ok2 := child(meta, "ilst")
	if !ok1 || !ok2 || len(keys) < 8 {
		return time.Time{}, false
	}

	// keys: versão/flags (4), quantidade (4), depois [tamanho][namespace][nome].
	index := uint32(0)
	count := binary.BigEndian.Uint32(keys[4:8])
	p := 8
	for i := uint32(1); i <= count && p+8 <= len(keys); i++ {
		size := int(binary.BigEndian.Uint32(keys[p : p+4]))
		if size < 8 || p+size > len(keys) {
			break
		}
		if string(keys[p+8:p+size]) == "com.apple.quicktime.creationdate" {
			index = i
			break
		}
		p += size
	}
	if index == 0 {
		return time.Time{}, false
	}

	// ilst: cada filha tem como "tipo" o número da chave e dentro uma caixa data.
	name := string(binary.BigEndian.AppendUint32(nil, index))
	item, ok := child(ilst, name)
	if !ok {
		return time.Time{}, false
	}
	data, ok := child(item, "data")
	if !ok || len(data) < 8 {
		return time.Time{}, false
	}
	value := strings.TrimSpace(string(data[8:]))
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, value); err == nil && t.Year() >= 1990 {
			return t, true
		}
	}
	return time.Time{}, false
}
