package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed web
var webFS embed.FS

func main() {
	dataDir := env("FOTOS_DATA", "./data")
	addr := env("FOTOS_ADDR", ":8080")

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dataDir, "fotos.db"))
	if err != nil {
		log.Fatal(err)
	}
	lib, err := NewLibrary(store, dataDir)
	if err != nil {
		log.Fatal(err)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "import":
			if len(os.Args) != 3 {
				log.Fatal("uso: fotos import <pasta>")
			}
			runImport(lib, os.Args[2])
			return
		case "redate":
			checked, fixed, err := lib.Redate()
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("fim: %d arquivos sem data nos metadados verificados, %d corrigidos", checked, fixed)
			return
		default:
			log.Fatalf("comando desconhecido: %s", os.Args[1])
		}
	}

	lib.StartTrashJanitor()
	lib.BackfillThumbs()
	log.Printf("acervo de fotos em %s, ouvindo em %s", dataDir, addr)
	srv := &http.Server{Addr: addr, Handler: routes(lib), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var dayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func routes(lib *Library) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/days", func(w http.ResponseWriter, r *http.Request) {
		days, err := lib.store.Days()
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, days)
	})

	mux.HandleFunc("GET /api/days/{day}", func(w http.ResponseWriter, r *http.Request) {
		day := r.PathValue("day")
		if !dayPattern.MatchString(day) {
			http.Error(w, "dia inválido", http.StatusBadRequest)
			return
		}
		photos, err := lib.store.PhotosOfDay(day)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, photos)
	})

	mux.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		handleUpload(lib, w, r)
	})

	// Lixeira. Os corpos são {"photoIds": [1, 2, 3]}.
	mux.HandleFunc("POST /api/trash", func(w http.ResponseWriter, r *http.Request) {
		var body idList
		if !readJSON(w, r, &body) {
			return
		}
		n, err := lib.store.Trash(body.PhotoIDs)
		writeResult(w, err, http.StatusOK, map[string]int{"trashed": n})
	})

	mux.HandleFunc("POST /api/trash/restore", func(w http.ResponseWriter, r *http.Request) {
		var body idList
		if !readJSON(w, r, &body) {
			return
		}
		n, err := lib.store.Restore(body.PhotoIDs)
		writeResult(w, err, http.StatusOK, map[string]int{"restored": n})
	})

	mux.HandleFunc("POST /api/trash/purge", func(w http.ResponseWriter, r *http.Request) {
		var body idList
		if !readJSON(w, r, &body) {
			return
		}
		var photos []*Photo
		for _, id := range body.PhotoIDs {
			p, err := lib.store.ByID(id)
			if err != nil {
				httpError(w, err)
				return
			}
			if p != nil {
				photos = append(photos, p)
			}
		}
		n, err := lib.Purge(photos)
		writeResult(w, err, http.StatusOK, map[string]int{"purged": n})
	})

	mux.HandleFunc("GET /api/trash", func(w http.ResponseWriter, r *http.Request) {
		photos, err := lib.store.Trashed()
		writeResult(w, err, http.StatusOK, map[string]any{
			"retentionDays": int(TrashRetention.Hours() / 24),
			"photos":        photos,
		})
	})

	mux.HandleFunc("DELETE /api/trash", func(w http.ResponseWriter, r *http.Request) {
		photos, err := lib.store.Trashed()
		if err != nil {
			httpError(w, err)
			return
		}
		n, err := lib.Purge(photos)
		writeResult(w, err, http.StatusOK, map[string]int{"purged": n})
	})

	mux.HandleFunc("PUT /api/photos/{id}/favorite", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Favorite bool `json:"favorite"`
		}
		id, ok := pathID(w, r, "id")
		if !ok || !readJSON(w, r, &body) {
			return
		}
		writeResult(w, lib.store.SetFavorite(id, body.Favorite), http.StatusNoContent, nil)
	})

	mux.HandleFunc("GET /api/photos/{id}/albums", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		ids, err := lib.store.AlbumsOfPhoto(id)
		writeResult(w, err, http.StatusOK, ids)
	})

	mux.HandleFunc("GET /api/favorites", func(w http.ResponseWriter, r *http.Request) {
		photos, err := lib.store.Favorites()
		writeResult(w, err, http.StatusOK, photos)
	})

	mux.HandleFunc("GET /api/albums", func(w http.ResponseWriter, r *http.Request) {
		albums, err := lib.store.Albums()
		writeResult(w, err, http.StatusOK, albums)
	})

	mux.HandleFunc("POST /api/albums", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		name, ok := albumName(w, body.Name)
		if !ok {
			return
		}
		a, err := lib.store.CreateAlbum(name)
		writeResult(w, err, http.StatusCreated, a)
	})

	mux.HandleFunc("GET /api/albums/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		a, err := lib.store.AlbumByID(id)
		if err == nil && a == nil {
			err = ErrNotFound
		}
		if err != nil {
			writeResult(w, err, 0, nil)
			return
		}
		photos, err := lib.store.AlbumPhotos(id)
		writeResult(w, err, http.StatusOK, map[string]any{"album": a, "photos": photos})
	})

	mux.HandleFunc("PATCH /api/albums/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		id, ok := pathID(w, r, "id")
		if !ok || !readJSON(w, r, &body) {
			return
		}
		name, ok := albumName(w, body.Name)
		if !ok {
			return
		}
		writeResult(w, lib.store.RenameAlbum(id, name), http.StatusNoContent, nil)
	})

	mux.HandleFunc("DELETE /api/albums/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "id")
		if !ok {
			return
		}
		writeResult(w, lib.store.DeleteAlbum(id), http.StatusNoContent, nil)
	})

	// Corpo: {"photoIds": [1, 2, 3]}. POST adiciona ao álbum, DELETE remove.
	albumPhotos := func(add bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				PhotoIDs []int64 `json:"photoIds"`
			}
			id, ok := pathID(w, r, "id")
			if !ok || !readJSON(w, r, &body) {
				return
			}
			if a, err := lib.store.AlbumByID(id); err != nil || a == nil {
				writeResult(w, cmpErr(err, ErrNotFound), 0, nil)
				return
			}
			if add {
				n, err := lib.store.AddToAlbum(id, body.PhotoIDs)
				writeResult(w, err, http.StatusOK, map[string]int{"added": n})
				return
			}
			writeResult(w, lib.store.RemoveFromAlbum(id, body.PhotoIDs), http.StatusNoContent, nil)
		}
	}
	mux.HandleFunc("POST /api/albums/{id}/photos", albumPhotos(true))
	mux.HandleFunc("DELETE /api/albums/{id}/photos", albumPhotos(false))

	mux.HandleFunc("GET /photo/{id}", func(w http.ResponseWriter, r *http.Request) {
		p := photoFromPath(lib, w, r)
		if p == nil {
			return
		}
		setCache(w, r, p)
		// ?download=1 faz o navegador salvar o arquivo em vez de abrir.
		disposition := "inline"
		if r.URL.Query().Get("download") != "" {
			disposition = "attachment"
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, safeName(p.Name)))
		// MOV é o mesmo formato de contêiner do MP4. Como video/quicktime, o
		// Chrome e o Android se recusam a tocar; como video/mp4, tocam.
		if disposition == "inline" && strings.EqualFold(filepath.Ext(p.Name), ".mov") {
			w.Header().Set("Content-Type", "video/mp4")
		}
		http.ServeFile(w, r, filepath.Join(lib.dataDir, p.Path))
	})

	mux.HandleFunc("GET /thumb/{id}", func(w http.ResponseWriter, r *http.Request) {
		p := photoFromPath(lib, w, r)
		if p == nil {
			return
		}
		if !p.HasThumb {
			http.NotFound(w, r)
			return
		}
		setCache(w, r, p)
		http.ServeFile(w, r, lib.thumbPath(p.Hash))
	})

	return mux
}

type idList struct {
	PhotoIDs []int64 `json:"photoIds"`
}

type uploadResult struct {
	Status  string `json:"status"` // ok, duplicate, error
	Message string `json:"message"`
	Photo   *Photo `json:"photo,omitempty"`
}

// handleUpload recebe um arquivo por requisição. O campo "lastModified"
// (milissegundos, vindo do navegador) precisa vir antes do campo "file".
func handleUpload(lib *Library, w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, uploadResult{Status: "error", Message: "envio inválido"})
		return
	}
	var fileTime time.Time
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, uploadResult{Status: "error", Message: "envio interrompido"})
			return
		}
		switch part.FormName() {
		case "lastModified":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			if ms, err := strconv.ParseInt(string(b), 10, 64); err == nil && ms > 0 {
				fileTime = time.UnixMilli(ms)
			}
		case "file":
			name := part.FileName()
			br := bufio.NewReader(part)
			head, _ := br.Peek(512)
			if err := checkMediaType(name, head); err != nil {
				writeJSON(w, http.StatusUnsupportedMediaType, uploadResult{Status: "error", Message: err.Error()})
				return
			}
			p, err := lib.Ingest(br, name, fileTime)
			switch {
			case errors.Is(err, ErrDuplicate) && p.DeletedAt != "":
				writeJSON(w, http.StatusConflict, uploadResult{
					Status:  "duplicate",
					Message: fmt.Sprintf("Esta foto já existe e está na Lixeira (%s). Restaure ela por lá.", p.Name),
					Photo:   p,
				})
			case errors.Is(err, ErrDuplicate):
				writeJSON(w, http.StatusConflict, uploadResult{
					Status:  "duplicate",
					Message: fmt.Sprintf("Esta foto já existe no acervo (%s, dia %s).", p.Name, brDate(p.Day)),
					Photo:   p,
				})
			case err != nil:
				log.Printf("upload de %q falhou: %v", name, err)
				writeJSON(w, http.StatusInternalServerError, uploadResult{Status: "error", Message: err.Error()})
			default:
				msg := "Foto salva."
				if p.IsVideo {
					msg = "Vídeo salvo."
				}
				writeJSON(w, http.StatusCreated, uploadResult{Status: "ok", Message: msg, Photo: p})
			}
			return
		}
	}
	writeJSON(w, http.StatusBadRequest, uploadResult{Status: "error", Message: "nenhum arquivo enviado"})
}

func photoFromPath(lib *Library, w http.ResponseWriter, r *http.Request) *Photo {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	p, err := lib.store.ByID(id)
	if err != nil {
		httpError(w, err)
		return nil
	}
	if p == nil {
		http.NotFound(w, r)
	}
	return p
}

// brDate converte AAAA-MM-DD em DD/MM/AAAA.
func brDate(day string) string {
	if t, err := time.Parse("2006-01-02", day); err == nil {
		return t.Format("02/01/2006")
	}
	return day
}

// setCache só deixa o navegador guardar o arquivo para sempre quando a URL
// traz o hash (?v=...). Sem isso, um ID que passou a apontar para outra foto
// (por exemplo depois de recriar o acervo) mostraria a foto antiga do cache.
func setCache(w http.ResponseWriter, r *http.Request, p *Photo) {
	if v := r.URL.Query().Get("v"); len(v) >= 8 && strings.HasPrefix(p.Hash, v) {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	w.Header().Set("ETag", `"`+p.Hash+`"`)
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return false
	}
	return true
}

func albumName(w http.ResponseWriter, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		http.Error(w, "o nome do álbum precisa ter entre 1 e 100 caracteres", http.StatusBadRequest)
		return "", false
	}
	return name, true
}

// cmpErr devolve err, ou fallback quando err é nil.
func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// writeResult responde com o erro adequado ou, se err for nil, com status e v.
func writeResult(w http.ResponseWriter, err error, status int, v any) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "não encontrado", http.StatusNotFound)
	case err != nil:
		httpError(w, err)
	case v == nil:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, v)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	log.Print(err)
	http.Error(w, "erro interno", http.StatusInternalServerError)
}

// runImport importa uma pasta inteira, com a mesma regra de duplicatas do upload.
func runImport(lib *Library, dir string) {
	var added, dup, failed int
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isMediaFile(path) {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			failed++
			log.Printf("ERRO %s: %v", path, err)
			return nil
		}
		p, err := lib.Ingest(f, filepath.Base(path), info.ModTime())
		f.Close()
		switch {
		case errors.Is(err, ErrDuplicate):
			dup++
			where := ""
			if p.DeletedAt != "" {
				where = ", que está na lixeira"
			}
			log.Printf("JÁ EXISTE %s (igual a %s%s)", path, p.Name, where)
		case err != nil:
			failed++
			log.Printf("ERRO %s: %v", path, err)
		default:
			added++
			log.Printf("ok %s -> dia %s (%s)", path, p.Day, p.DateSource)
		}
		return nil
	})
	if err != nil {
		log.Print(err)
	}
	log.Printf("fim: %d novas, %d duplicadas, %d com erro", added, dup, failed)
}
