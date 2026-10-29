package main

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Photo é uma foto guardada no acervo.
type Photo struct {
	ID         int64  `json:"id"`
	Hash       string `json:"hash"`
	Name       string `json:"name"`
	Path       string `json:"-"`
	Size       int64  `json:"size"`
	Day        string `json:"day"`        // AAAA-MM-DD em que a foto foi tirada
	TakenAt    string `json:"takenAt"`    // data/hora completa, quando conhecida
	DateSource string `json:"dateSource"` // exif, filename, file, upload
	UploadedAt string `json:"uploadedAt"`
	HasThumb   bool   `json:"hasThumb"`
	Favorite   bool   `json:"favorite"`
	DeletedAt  string `json:"deletedAt,omitempty"` // preenchido quando a foto está na lixeira
	IsVideo    bool   `json:"isVideo"`
}

type Album struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Count     int    `json:"count"`   // fotos e vídeos
	Videos    int    `json:"videos"`  // quantos do total são vídeos
	CoverID   int64  `json:"coverId"` // 0 quando o álbum está vazio
	CoverHash string `json:"coverHash"`
	CreatedAt string `json:"createdAt"`
}

type DaySummary struct {
	Day       string `json:"day"`
	Count     int    `json:"count"`  // fotos e vídeos
	Videos    int    `json:"videos"` // quantos do total são vídeos
	CoverID   int64  `json:"coverId"`
	CoverHash string `json:"coverHash"`
}

var (
	ErrDuplicate = errors.New("foto duplicada")
	ErrNotFound  = errors.New("não encontrado")
)

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS photos (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	hash        TEXT NOT NULL UNIQUE,
	name        TEXT NOT NULL,
	path        TEXT NOT NULL,
	size        INTEGER NOT NULL,
	day         TEXT NOT NULL,
	taken_at    TEXT NOT NULL,
	date_source TEXT NOT NULL,
	uploaded_at TEXT NOT NULL,
	has_thumb   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS photos_day ON photos(day, taken_at);
CREATE TABLE IF NOT EXISTS albums (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS album_photos (
	album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
	photo_id INTEGER NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
	added_at TEXT NOT NULL,
	PRIMARY KEY (album_id, photo_id)
);
CREATE INDEX IF NOT EXISTS album_photos_photo ON album_photos(photo_id);`)
	if err != nil {
		return nil, err
	}
	// Bancos criados antes dos favoritos e da lixeira não têm essas colunas.
	for _, col := range []string{
		`favorite INTEGER NOT NULL DEFAULT 0`,
		`deleted_at TEXT`,
	} {
		if _, err := db.Exec(`ALTER TABLE photos ADD COLUMN ` + col); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

const photoCols = `id, hash, name, path, size, day, taken_at, date_source, uploaded_at, has_thumb, favorite, COALESCE(deleted_at, '')`

// live filtra as fotos que não estão na lixeira.
const live = `deleted_at IS NULL`

func scanPhoto(row interface{ Scan(...any) error }) (*Photo, error) {
	var p Photo
	err := row.Scan(&p.ID, &p.Hash, &p.Name, &p.Path, &p.Size, &p.Day, &p.TakenAt, &p.DateSource, &p.UploadedAt, &p.HasThumb, &p.Favorite, &p.DeletedAt)
	if err != nil {
		return nil, err
	}
	p.IsVideo = isVideo(p.Name)
	return &p, nil
}

// videoCond devolve a condição SQL equivalente a isVideo para a coluna col.
func videoCond(col string) string {
	conds := make([]string, len(videoExts))
	for i, e := range videoExts {
		conds[i] = `lower(` + col + `) LIKE '%` + e + `'`
	}
	return "(" + strings.Join(conds, " OR ") + ")"
}

func (s *Store) ByHash(hash string) (*Photo, error) {
	p, err := scanPhoto(s.db.QueryRow(`SELECT `+photoCols+` FROM photos WHERE hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Store) ByID(id int64) (*Photo, error) {
	p, err := scanPhoto(s.db.QueryRow(`SELECT `+photoCols+` FROM photos WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Store) Insert(p *Photo) error {
	p.UploadedAt = time.Now().Format(time.RFC3339)
	p.IsVideo = isVideo(p.Name)
	res, err := s.db.Exec(`INSERT INTO photos (hash, name, path, size, day, taken_at, date_source, uploaded_at, has_thumb)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Hash, p.Name, p.Path, p.Size, p.Day, p.TakenAt, p.DateSource, p.UploadedAt, p.HasThumb)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrDuplicate
		}
		return err
	}
	p.ID, err = res.LastInsertId()
	return err
}

func (s *Store) Days() ([]DaySummary, error) {
	rows, err := s.db.Query(`SELECT day, COUNT(*), SUM(` + videoCond("name") + `), MIN(id), hash FROM photos WHERE ` + live + ` GROUP BY day ORDER BY day DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := []DaySummary{}
	for rows.Next() {
		var d DaySummary
		if err := rows.Scan(&d.Day, &d.Count, &d.Videos, &d.CoverID, &d.CoverHash); err != nil {
			return nil, err
		}
		days = append(days, d)
	}
	return days, rows.Err()
}

func (s *Store) PhotosOfDay(day string) ([]*Photo, error) {
	return s.queryPhotos(`SELECT `+photoCols+` FROM photos WHERE day = ? AND `+live+` ORDER BY taken_at, id`, day)
}

func (s *Store) Favorites() ([]*Photo, error) {
	return s.queryPhotos(`SELECT ` + photoCols + ` FROM photos WHERE favorite = 1 AND ` + live + ` ORDER BY taken_at DESC, id DESC`)
}

func (s *Store) SetFavorite(id int64, fav bool) error {
	return s.execOne(`UPDATE photos SET favorite = ? WHERE id = ?`, fav, id)
}

func (s *Store) Albums() ([]Album, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.name, a.created_at,
       (SELECT COUNT(*) FROM album_photos ap JOIN photos p ON p.id = ap.photo_id
         WHERE ap.album_id = a.id AND p.` + live + `),
       (SELECT COUNT(*) FROM album_photos ap JOIN photos p ON p.id = ap.photo_id
         WHERE ap.album_id = a.id AND p.` + live + ` AND ` + videoCond("p.name") + `),
       COALESCE((SELECT ap.photo_id FROM album_photos ap JOIN photos p ON p.id = ap.photo_id
         WHERE ap.album_id = a.id AND p.` + live + ` ORDER BY ap.added_at, ap.photo_id LIMIT 1), 0),
       COALESCE((SELECT p.hash FROM album_photos ap JOIN photos p ON p.id = ap.photo_id
         WHERE ap.album_id = a.id AND p.` + live + ` ORDER BY ap.added_at, ap.photo_id LIMIT 1), '')
FROM albums a ORDER BY a.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	albums := []Album{}
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Name, &a.CreatedAt, &a.Count, &a.Videos, &a.CoverID, &a.CoverHash); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, rows.Err()
}

func (s *Store) AlbumByID(id int64) (*Album, error) {
	var a Album
	err := s.db.QueryRow(`SELECT id, name, created_at, (SELECT COUNT(*) FROM album_photos ap JOIN photos p ON p.id = ap.photo_id
			WHERE ap.album_id = albums.id AND p.`+live+`)
		FROM albums WHERE id = ?`, id).Scan(&a.ID, &a.Name, &a.CreatedAt, &a.Count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) CreateAlbum(name string) (*Album, error) {
	a := &Album{Name: name, CreatedAt: time.Now().Format(time.RFC3339)}
	res, err := s.db.Exec(`INSERT INTO albums (name, created_at) VALUES (?, ?)`, a.Name, a.CreatedAt)
	if err != nil {
		return nil, err
	}
	a.ID, err = res.LastInsertId()
	return a, err
}

func (s *Store) RenameAlbum(id int64, name string) error {
	return s.execOne(`UPDATE albums SET name = ? WHERE id = ?`, name, id)
}

// DeleteAlbum apaga só o álbum; as fotos continuam no acervo.
func (s *Store) DeleteAlbum(id int64) error {
	return s.execOne(`DELETE FROM albums WHERE id = ?`, id)
}

func (s *Store) AlbumPhotos(id int64) ([]*Photo, error) {
	return s.queryPhotos(`SELECT `+photoCols+` FROM photos
		WHERE id IN (SELECT photo_id FROM album_photos WHERE album_id = ?) AND `+live+`
		ORDER BY taken_at, id`, id)
}

// AddToAlbum ignora fotos que já estão no álbum e devolve quantas entraram.
func (s *Store) AddToAlbum(albumID int64, photoIDs []int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Format(time.RFC3339)
	added := 0
	for _, pid := range photoIDs {
		res, err := tx.Exec(`INSERT OR IGNORE INTO album_photos (album_id, photo_id, added_at)
			SELECT ?, id, ? FROM photos WHERE id = ? AND `+live, albumID, now, pid)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
	}
	return added, tx.Commit()
}

func (s *Store) RemoveFromAlbum(albumID int64, photoIDs []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, pid := range photoIDs {
		if _, err := tx.Exec(`DELETE FROM album_photos WHERE album_id = ? AND photo_id = ?`, albumID, pid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AlbumsOfPhoto devolve os IDs dos álbuns em que a foto está.
func (s *Store) AlbumsOfPhoto(photoID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT album_id FROM album_photos WHERE photo_id = ?`, photoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Trash move as fotos para a lixeira. Elas somem de Recentes, Favoritos e
// álbuns, mas guardam favorito e álbuns caso sejam restauradas.
func (s *Store) Trash(ids []int64) (int, error) {
	// Em UTC para a comparação de texto em TrashedBefore funcionar mesmo se o fuso mudar.
	now := time.Now().UTC().Format(time.RFC3339)
	return s.eachID(ids, `UPDATE photos SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, now)
}

func (s *Store) Restore(ids []int64) (int, error) {
	return s.eachID(ids, `UPDATE photos SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`)
}

// Trashed devolve as fotos da lixeira, das apagadas mais recentemente às mais antigas.
func (s *Store) Trashed() ([]*Photo, error) {
	return s.queryPhotos(`SELECT ` + photoCols + ` FROM photos WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC, id DESC`)
}

// TrashedBefore devolve as fotos que foram para a lixeira antes de cutoff.
func (s *Store) TrashedBefore(cutoff time.Time) ([]*Photo, error) {
	return s.queryPhotos(`SELECT `+photoCols+` FROM photos WHERE deleted_at IS NOT NULL AND deleted_at < ?`,
		cutoff.UTC().Format(time.RFC3339))
}

// Purge apaga o registro da foto de vez. Só funciona para fotos na lixeira.
func (s *Store) Purge(id int64) error {
	return s.execOne(`DELETE FROM photos WHERE id = ? AND deleted_at IS NOT NULL`, id)
}

func (s *Store) eachID(ids []int64, query string, args ...any) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	changed := 0
	for _, id := range ids {
		res, err := tx.Exec(query, append(append([]any{}, args...), id)...)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		changed += int(n)
	}
	return changed, tx.Commit()
}

// WithoutMetadataDate devolve as fotos cuja data não veio dos metadados do arquivo.
func (s *Store) WithoutMetadataDate() ([]*Photo, error) {
	return s.queryPhotos(`SELECT ` + photoCols + ` FROM photos WHERE date_source NOT IN ('exif', 'video')`)
}

func (s *Store) UpdateDate(id int64, day, takenAt, source, path string) error {
	return s.execOne(`UPDATE photos SET day = ?, taken_at = ?, date_source = ?, path = ? WHERE id = ?`,
		day, takenAt, source, path, id)
}

// WithoutThumb devolve as fotos e vídeos que ainda não têm miniatura.
func (s *Store) WithoutThumb() ([]*Photo, error) {
	return s.queryPhotos(`SELECT ` + photoCols + ` FROM photos WHERE has_thumb = 0`)
}

func (s *Store) SetHasThumb(id int64) error {
	return s.execOne(`UPDATE photos SET has_thumb = 1 WHERE id = ?`, id)
}

func (s *Store) execOne(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) queryPhotos(query string, args ...any) ([]*Photo, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	photos := []*Photo{}
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, rows.Err()
}
