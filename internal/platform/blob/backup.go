package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Backup copies every stored file into dir, at dir/<key>. Keys are
// content-addressed, so a file already in dir is the same and is skipped:
// running it again into the same directory copies only new files. Objects
// whose key is not one the pipeline makes are reported and skipped, so a key
// can never name a path outside dir.
func Backup(ctx context.Context, store Store, dir string) (copied, skipped int, err error) {
	var bad []string
	err = store.List(ctx, func(key string) error {
		if !ValidKey(key) {
			bad = append(bad, key)
			return nil
		}
		path := filepath.Join(dir, filepath.FromSlash(key))
		if _, err := os.Stat(path); err == nil {
			skipped++
			return nil
		}
		body, err := store.Get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			return nil // collected meanwhile
		}
		if err != nil {
			return err
		}
		if err := writeAtomic(path, body); err != nil {
			return err
		}
		copied++
		return nil
	})
	if err == nil && len(bad) > 0 {
		err = fmt.Errorf("blob: backup skipped %d objects with unexpected keys, e.g. %q", len(bad), bad[0])
	}
	return copied, skipped, err
}

// Restore uploads every file under dir whose path is a valid key. Files the
// store already has are uploaded again; content is the same by construction.
func Restore(ctx context.Context, store Store, dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !ValidKey(key) {
			return nil // a partial download (.tmp) or a stray file
		}
		body, err := os.ReadFile(path) //nolint:gosec // the path comes from walking the backup directory
		if err != nil {
			return err
		}
		if err := store.Put(ctx, key, body, ContentType(key[strings.LastIndexByte(key, '.')+1:])); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

// writeAtomic writes through a temporary file, so an interrupted backup never
// leaves a truncated file that a later run would skip as already copied.
func writeAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
