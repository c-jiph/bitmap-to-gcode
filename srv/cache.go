package srv

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// DefaultAIPrompt is the default prompt for AI image transformation
const DefaultAIPrompt = "Reduce this image to a two color line-art image suitable for use in a " +
	"child's coloring book. The lines should be black and the background " +
	"white. The image will be reproduced by an X-Y plotter, so the final " +
	"image should have only lines (no solid/filled areas)."

// AIImageCache manages cached AI-generated images
type AIImageCache struct {
	db       *sql.DB
	cacheDir string
}

// NewAIImageCache creates a new cache with SQLite storage
func NewAIImageCache(dbPath, cacheDir string) (*AIImageCache, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Create table if not exists (with prompt column)
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS ai_image_cache (
			cache_key TEXT PRIMARY KEY,
			input_hash TEXT NOT NULL,
			prompt TEXT NOT NULL,
			output_filename TEXT NOT NULL,
			mime_type TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}

	// Check if we need to migrate old schema (input_hash as primary key without prompt)
	if err := migrateOldSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	return &AIImageCache{
		db:       db,
		cacheDir: cacheDir,
	}, nil
}

// migrateOldSchema migrates from old schema (input_hash as primary key) to new schema (cache_key)
func migrateOldSchema(db *sql.DB) error {
	// Check if old table exists with input_hash as primary key
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('ai_image_cache') 
		WHERE name = 'input_hash' AND pk = 1
	`).Scan(&count)
	if err != nil {
		return err
	}

	if count == 0 {
		// No migration needed - either new schema or empty database
		return nil
	}

	// Old schema detected - migrate data
	// 1. Rename old table
	_, err = db.Exec(`ALTER TABLE ai_image_cache RENAME TO ai_image_cache_old`)
	if err != nil {
		return fmt.Errorf("rename old table: %w", err)
	}

	// 2. Create new table
	_, err = db.Exec(`
		CREATE TABLE ai_image_cache (
			cache_key TEXT PRIMARY KEY,
			input_hash TEXT NOT NULL,
			prompt TEXT NOT NULL,
			output_filename TEXT NOT NULL,
			mime_type TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("create new table: %w", err)
	}

	// 3. Migrate data with default prompt
	_, err = db.Exec(`
		INSERT INTO ai_image_cache (cache_key, input_hash, prompt, output_filename, mime_type, created_at)
		SELECT 
			input_hash || ':' || ?,
			input_hash,
			?,
			output_filename,
			mime_type,
			created_at
		FROM ai_image_cache_old
	`, hashString(DefaultAIPrompt), DefaultAIPrompt)
	if err != nil {
		return fmt.Errorf("migrate data: %w", err)
	}

	// 4. Drop old table
	_, err = db.Exec(`DROP TABLE ai_image_cache_old`)
	if err != nil {
		return fmt.Errorf("drop old table: %w", err)
	}

	return nil
}

// Close closes the database connection
func (c *AIImageCache) Close() error {
	return c.db.Close()
}

// CacheDir returns the cache directory path
func (c *AIImageCache) CacheDir() string {
	return c.cacheDir
}

// HashFile computes SHA256 hash of a file
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashString computes SHA256 hash of a string (first 16 chars)
func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

// MakeCacheKey creates a cache key from input hash and prompt
func MakeCacheKey(inputHash, prompt string) string {
	return inputHash + ":" + hashString(prompt)
}

// CachedResult represents a cached AI transformation result
type CachedResult struct {
	Filename string
	MimeType string
	FullPath string
	Prompt   string
}

// Lookup checks if we have a cached result for the given input hash and prompt
func (c *AIImageCache) Lookup(inputHash, prompt string) (*CachedResult, error) {
	cacheKey := MakeCacheKey(inputHash, prompt)

	var filename, mimeType, storedPrompt string
	err := c.db.QueryRow(
		"SELECT output_filename, mime_type, prompt FROM ai_image_cache WHERE cache_key = ?",
		cacheKey,
	).Scan(&filename, &mimeType, &storedPrompt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	fullPath := filepath.Join(c.cacheDir, filename)
	// Verify the file still exists
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		// File is missing, remove from database
		c.db.Exec("DELETE FROM ai_image_cache WHERE cache_key = ?", cacheKey)
		return nil, nil
	}

	return &CachedResult{
		Filename: filename,
		MimeType: mimeType,
		FullPath: fullPath,
		Prompt:   storedPrompt,
	}, nil
}

// Store saves a new cached result
func (c *AIImageCache) Store(inputHash, prompt string, imageData []byte, mimeType string) (*CachedResult, error) {
	cacheKey := MakeCacheKey(inputHash, prompt)

	// Determine extension from MIME type
	ext := ".png"
	switch mimeType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}

	// Use hash + timestamp for filename to ensure uniqueness
	filename := fmt.Sprintf("%s_%d%s", inputHash[:16], time.Now().UnixNano(), ext)
	fullPath := filepath.Join(c.cacheDir, filename)

	// Write the file
	if err := os.WriteFile(fullPath, imageData, 0644); err != nil {
		return nil, fmt.Errorf("write cache file: %w", err)
	}

	// Insert into database
	_, err := c.db.Exec(
		"INSERT OR REPLACE INTO ai_image_cache (cache_key, input_hash, prompt, output_filename, mime_type) VALUES (?, ?, ?, ?, ?)",
		cacheKey, inputHash, prompt, filename, mimeType,
	)
	if err != nil {
		os.Remove(fullPath) // Clean up on error
		return nil, fmt.Errorf("insert cache record: %w", err)
	}

	return &CachedResult{
		Filename: filename,
		MimeType: mimeType,
		FullPath: fullPath,
		Prompt:   prompt,
	}, nil
}
