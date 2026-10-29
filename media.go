package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const thumbSize = 400

// Library guarda os arquivos no disco e os registra no banco.
type Library struct {
	store   *Store
	dataDir string
	mu      sync.Mutex // um upload por vez: o CPU é fraco e isso evita corridas no hash
}

func NewLibrary(store *Store, dataDir string) (*Library, error) {
	for _, d := range []string{"photos", "thumbs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Library{store: store, dataDir: dataDir}, nil
}

func (l *Library) thumbPath(hash string) string {
	return filepath.Join(l.dataDir, "thumbs", hash+".jpg")
}

// Ingest grava a foto, a não ser que um arquivo idêntico já exista.
// Nesse caso devolve a foto existente e ErrDuplicate.
// fileTime é a data de modificação informada pelo navegador (pode ser zero).
func (l *Library) Ingest(src io.Reader, name string, fileTime time.Time) (*Photo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	tmp, err := os.CreateTemp(filepath.Join(l.dataDir, "tmp"), "up-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // não faz nada se o arquivo já foi movido

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), src)
	tmp.Close()
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, errors.New("arquivo vazio")
	}
	hash := hex.EncodeToString(h.Sum(nil))

	if existing, err := l.store.ByHash(hash); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, ErrDuplicate
	}

	taken, source := detectDate(tmpName, name, fileTime)
	p := &Photo{
		Hash:       hash,
		Name:       name,
		Size:       size,
		Day:        taken.Format("2006-01-02"),
		TakenAt:    taken.Format("2006-01-02T15:04:05"),
		DateSource: source,
	}

	rel := storagePath(taken, hash, name)
	p.Path = rel
	dst := filepath.Join(l.dataDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return nil, err
	}

	if err := makeAnyThumb(dst, name, l.thumbPath(hash)); err == nil {
		p.HasThumb = true
	}

	if err := l.store.Insert(p); err != nil {
		os.Remove(dst)
		os.Remove(l.thumbPath(hash))
		return nil, err
	}
	return p, nil
}

// TrashRetention é quanto tempo uma foto fica na lixeira antes de ser excluída de vez.
const TrashRetention = 30 * 24 * time.Hour

// Purge exclui de vez fotos que estão na lixeira: registro no banco, arquivo
// original e miniatura. Fotos fora da lixeira são ignoradas.
func (l *Library) Purge(photos []*Photo) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, p := range photos {
		if p.DeletedAt == "" {
			continue
		}
		if err := l.store.Purge(p.ID); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return n, err
		}
		l.removeFiles(p)
		n++
	}
	return n, nil
}

func (l *Library) removeFiles(p *Photo) {
	orig := filepath.Join(l.dataDir, p.Path)
	for _, f := range []string{orig, l.thumbPath(p.Hash)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("não consegui apagar %s: %v", f, err)
		}
	}
	// Apaga as pastas de dia, mês e ano que ficaram vazias.
	removeEmptyDirs(filepath.Join(l.dataDir, "photos"), filepath.Dir(orig))
}

// PurgeExpired exclui de vez as fotos que estão na lixeira há mais de TrashRetention.
func (l *Library) PurgeExpired() {
	photos, err := l.store.TrashedBefore(time.Now().Add(-TrashRetention))
	if err != nil {
		log.Printf("limpeza da lixeira: %v", err)
		return
	}
	if len(photos) == 0 {
		return
	}
	n, err := l.Purge(photos)
	if err != nil {
		log.Printf("limpeza da lixeira: %v", err)
	}
	log.Printf("limpeza da lixeira: %d fotos excluídas de vez", n)
}

// StartTrashJanitor roda a limpeza agora e depois de hora em hora.
func (l *Library) StartTrashJanitor() {
	go func() {
		for {
			l.PurgeExpired()
			time.Sleep(time.Hour)
		}
	}()
}

func storagePath(taken time.Time, hash, name string) string {
	return filepath.Join("photos", taken.Format("2006"), taken.Format("01"), taken.Format("02"), hash[:12]+"-"+safeName(name))
}

// Redate relê a data de captura de fotos e vídeos que não tinham metadados
// reconhecidos quando foram enviados, e move os que mudarem de dia.
func (l *Library) Redate() (checked, fixed int, err error) {
	photos, err := l.store.WithoutMetadataDate()
	if err != nil {
		return 0, 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range photos {
		checked++
		orig := filepath.Join(l.dataDir, p.Path)
		taken, src, ok := metadataDate(orig)
		if !ok {
			continue
		}
		rel := storagePath(taken, p.Hash, p.Name)
		dst := filepath.Join(l.dataDir, rel)
		if rel != p.Path {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return checked, fixed, err
			}
			if err := os.Rename(orig, dst); err != nil {
				return checked, fixed, err
			}
		}
		day, takenAt := taken.Format("2006-01-02"), taken.Format("2006-01-02T15:04:05")
		if err := l.store.UpdateDate(p.ID, day, takenAt, src, rel); err != nil {
			os.Rename(dst, orig)
			return checked, fixed, err
		}
		if rel != p.Path {
			removeEmptyDirs(filepath.Join(l.dataDir, "photos"), filepath.Dir(orig))
		}
		log.Printf("%s: dia %s -> %s (%s)", p.Name, p.Day, day, src)
		fixed++
	}
	return checked, fixed, nil
}

// removeEmptyDirs apaga dir e as pastas acima dele que ficaram vazias, até root.
func removeEmptyDirs(root, dir string) {
	for ; dir != root && strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(name string) string {
	name = unsafeChars.ReplaceAllString(filepath.Base(name), "_")
	if len(name) > 80 {
		ext := filepath.Ext(name)
		name = name[:80-len(ext)] + ext
	}
	if name == "" || name == "." || name == ".." {
		name = "foto"
	}
	return name
}

// detectDate descobre quando a foto foi tirada, em ordem de confiança:
// metadados do arquivo (EXIF ou vídeo), data no nome do arquivo, data de
// modificação do arquivo, agora.
func detectDate(path, name string, fileTime time.Time) (time.Time, string) {
	if t, src, ok := metadataDate(path); ok {
		return t, src
	}
	if t, ok := filenameDate(name); ok {
		return t, "filename"
	}
	if !fileTime.IsZero() {
		return fileTime.Local(), "file"
	}
	return time.Now(), "upload"
}

func readExif(path string) (*exif.Exif, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return exif.Decode(f)
}

func exifDate(path string) (time.Time, bool) {
	x, err := readExif(path)
	if err != nil {
		return time.Time{}, false
	}
	t, err := x.DateTime()
	if err != nil || t.Year() < 1900 {
		return time.Time{}, false
	}
	return t, true
}

// Pega nomes como IMG_20240512_101500.jpg, PXL_20240512..., IMG-20240512-WA0001.jpg
// e "2024-05-12 10.15.00.jpg".
var nameDate = regexp.MustCompile(`(?:^|\D)((?:19|20)\d{2})[-_.]?(\d{2})[-_.]?(\d{2})(?:[-_ T.]?(\d{2})[-_.:]?(\d{2})[-_.:]?(\d{2}))?`)

func filenameDate(name string) (time.Time, bool) {
	m := nameDate.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return time.Time{}, false
	}
	s := m[1] + m[2] + m[3]
	layout := "20060102"
	if m[4] != "" {
		s += m[4] + m[5] + m[6]
		layout += "150405"
	}
	t, err := time.ParseInLocation(layout, s, time.Local)
	if err != nil || t.After(time.Now().Add(48*time.Hour)) {
		return time.Time{}, false
	}
	return t, true
}

// Tipos aceitos no envio e na importação de pasta. O botão Enviar (atributo
// accept em web/index.html) lista as mesmas extensões.
var (
	imageExts = []string{".jpg", ".jpeg", ".jpe", ".jfif", ".png", ".gif", ".webp", ".heic", ".heif", ".avif",
		".bmp", ".tif", ".tiff", ".dng", ".cr2", ".cr3", ".nef", ".arw", ".orf", ".rw2", ".raf"}
	videoExts = []string{".mov", ".mp4", ".m4v", ".3gp", ".3g2", ".webm", ".mkv", ".avi", ".mts", ".m2ts",
		".wmv", ".mpg", ".mpeg", ".flv", ".ogv"}
)

func isVideo(name string) bool {
	return slices.Contains(videoExts, strings.ToLower(filepath.Ext(name)))
}

func isMediaFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return slices.Contains(imageExts, ext) || slices.Contains(videoExts, ext)
}

// checkMediaType recusa o que não é foto nem vídeo (PDF, documentos, planilhas,
// textos, compactados). Olha a extensão e também o começo do arquivo (head),
// para pegar um documento renomeado para .jpg.
func checkMediaType(name string, head []byte) error {
	errType := fmt.Errorf("%q não é foto nem vídeo: só fotos e vídeos são aceitos", name)
	if !isMediaFile(name) {
		return errType
	}
	ct := http.DetectContentType(head)
	switch {
	case ct == "application/pdf", ct == "application/postscript", ct == "application/zip", // zip: docx, xlsx, odt...
		ct == "application/x-gzip", ct == "application/x-rar-compressed", ct == "application/x-7z-compressed",
		strings.HasPrefix(ct, "text/"):
		return errType
	case bytes.HasPrefix(head, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}): // doc, xls, ppt antigos
		return errType
	}
	return nil
}

// makeAnyThumb gera a miniatura de uma foto ou, com o ffmpeg, de um vídeo.
func makeAnyThumb(src, name, dst string) error {
	if isVideo(name) {
		return makeVideoThumb(src, dst)
	}
	err := makeThumb(src, dst)
	if err != nil && makeVideoThumb(src, dst) == nil {
		// HEIC, AVIF, BMP, TIFF e RAW o Go não lê, mas o ffmpeg muitas vezes sim.
		return nil
	}
	return err
}

// makeVideoThumb tira um quadro do vídeo com o ffmpeg. Tenta no segundo 1 e,
// para vídeos muito curtos, no começo. O ffmpeg já aplica a rotação do vídeo.
func makeVideoThumb(src, dst string) error {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("miniatura de vídeo: ffmpeg não encontrado")
	}
	tmp := dst + ".tmp.jpg"
	defer os.Remove(tmp)
	scale := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", thumbSize, thumbSize)
	for _, at := range []string{"1", "0"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-loglevel", "error", "-y",
			"-ss", at, "-i", src, "-frames:v", "1", "-vf", scale, "-q:v", "4", tmp)
		out, err := cmd.CombinedOutput()
		cancel()
		if info, statErr := os.Stat(tmp); err == nil && statErr == nil && info.Size() > 0 {
			return os.Rename(tmp, dst)
		} else if err != nil {
			log.Printf("ffmpeg em %s: %v %s", filepath.Base(src), err, strings.TrimSpace(string(out)))
		}
	}
	return fmt.Errorf("miniatura de vídeo: nenhum quadro extraído")
}

// BackfillThumbs gera, em segundo plano, as miniaturas que faltam (por exemplo
// dos vídeos enviados antes de a app saber fazer miniatura de vídeo).
func (l *Library) BackfillThumbs() {
	go func() {
		photos, err := l.store.WithoutThumb()
		if err != nil {
			log.Printf("miniaturas: %v", err)
			return
		}
		made := 0
		for _, p := range photos {
			if !isVideo(p.Name) {
				continue
			}
			l.mu.Lock()
			err := makeAnyThumb(filepath.Join(l.dataDir, p.Path), p.Name, l.thumbPath(p.Hash))
			if err == nil {
				err = l.store.SetHasThumb(p.ID)
			}
			l.mu.Unlock()
			if err == nil {
				made++
			}
		}
		if made > 0 {
			log.Printf("miniaturas: %d geradas para arquivos antigos", made)
		}
	}()
}

func makeThumb(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("miniatura: %w", err)
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > thumbSize || h > thumbSize {
		if w > h {
			w, h = thumbSize, h*thumbSize/w
		} else {
			w, h = w*thumbSize/h, thumbSize
		}
	}
	small := image.NewRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, b, draw.Src, nil)

	var out image.Image = small
	if x, err := readExif(src); err == nil {
		if tag, err := x.Get(exif.Orientation); err == nil {
			if o, err := tag.Int(0); err == nil {
				out = orient(small, o)
			}
		}
	}

	tmp := dst + ".tmp"
	of, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(of, out, &jpeg.Options{Quality: 80}); err != nil {
		of.Close()
		os.Remove(tmp)
		return err
	}
	of.Close()
	return os.Rename(tmp, dst)
}

// orient aplica a orientação EXIF (1 a 8) para a miniatura ficar em pé.
func orient(src *image.RGBA, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			dst.SetRGBA(nx, ny, src.RGBAAt(x, y))
		}
	}
	return dst
}
