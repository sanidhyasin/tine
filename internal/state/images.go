package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Image is an image tined knows about. The image itself (kernel, disk,
// image.json) lives on disk; this is tined's record of it.
type Image struct {
	Name string
	// Dir is the image's directory.
	Dir string
	// Metadata is the image's image.json, as written by internal/image.
	Metadata json.RawMessage
	// RegisteredAt is when tined last recorded it.
	RegisteredAt time.Time
}

// PutImage records an image, or updates the record of one with that name.
func (s *Store) PutImage(ctx context.Context, img Image) (Image, error) {
	if img.Name == "" || img.Dir == "" {
		return Image{}, errors.New("put image: name and dir are required")
	}
	if !json.Valid(img.Metadata) {
		return Image{}, fmt.Errorf("put image %s: metadata isn't valid JSON", img.Name)
	}
	img.RegisteredAt = s.now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO images (name, dir, metadata, registered_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET dir = excluded.dir, metadata = excluded.metadata, registered_at = excluded.registered_at`,
		img.Name, img.Dir, string(img.Metadata), formatTime(img.RegisteredAt))
	if err != nil {
		return Image{}, fmt.Errorf("put image %s: %w", img.Name, err)
	}
	return img, nil
}

// GetImage returns one image's record.
func (s *Store) GetImage(ctx context.Context, name string) (Image, error) {
	img, err := scanImage(s.db.QueryRowContext(ctx, "SELECT name, dir, metadata, registered_at FROM images WHERE name = ?", name))
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, fmt.Errorf("image %s: %w", name, ErrNotFound)
	}
	return img, err
}

// ListImages returns every image, by name.
func (s *Store) ListImages(ctx context.Context) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, dir, metadata, registered_at FROM images ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var images []Image
	for rows.Next() {
		img, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, rows.Err()
}

// DeleteImage forgets an image. It fails with ErrInUse while live VMs use it.
func (s *Store) DeleteImage(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var users int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM vms WHERE image = ? AND state != 'destroyed'", name).Scan(&users); err != nil {
		return err
	}
	if users > 0 {
		return fmt.Errorf("image %s: %d VMs use it: %w", name, users, ErrInUse)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM images WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("image %s: %w", name, ErrNotFound)
	}
	return tx.Commit()
}

func scanImage(row scanner) (Image, error) {
	var (
		img        Image
		meta, when string
	)
	if err := row.Scan(&img.Name, &img.Dir, &meta, &when); err != nil {
		return Image{}, err
	}
	img.Metadata = json.RawMessage(meta)
	var err error
	img.RegisteredAt, err = parseTime(when)
	return img, err
}
