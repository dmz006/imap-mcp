// Package output provides the enforced file output path for all MCP tools.
// It is the only code path in the MCP layer that writes files to disk.
// All writes are confined to the configured working_dir — paths outside
// that directory are rejected at the type level, not by convention.
package output

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Writer enforces that all file output lands inside a single root directory.
// Obtain one via New; use Write, Read, and List — never os.WriteFile directly.
type Writer struct {
	root string
}

// New creates a Writer rooted at dir. The directory is created if it does not exist.
func New(dir string) (*Writer, error) {
	if dir == "" {
		return nil, fmt.Errorf("output: working_dir is not configured")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("output: resolve %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0750); err != nil {
		return nil, fmt.Errorf("output: mkdir %s: %w", abs, err)
	}
	return &Writer{root: abs}, nil
}

// Root returns the absolute path of the working directory.
func (w *Writer) Root() string { return w.root }

// Resolve returns the absolute path for a relative filename, enforcing that
// the result stays inside the working directory. Returns an error if the
// resolved path would escape (e.g. via "../" traversal).
func (w *Writer) Resolve(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("output: filename is required")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("output: absolute paths are not allowed — use a filename or subdirectory relative to the working dir")
	}
	clean := filepath.Clean(name)
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("output: path traversal is not allowed")
	}
	abs := filepath.Join(w.root, clean)
	// Final guard: confirm resolved path is still inside root
	rel, err := filepath.Rel(w.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("output: path %q escapes working directory", name)
	}
	return abs, nil
}

// Write writes content to filename inside the working directory.
// Creates subdirectories as needed. Returns the absolute path written.
func (w *Writer) Write(name, content string) (string, error) {
	abs, err := w.Resolve(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0750); err != nil {
		return "", fmt.Errorf("output: mkdir for %s: %w", name, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0640); err != nil {
		return "", fmt.Errorf("output: write %s: %w", name, err)
	}
	return abs, nil
}

// Read reads the content of filename from the working directory.
func (w *Writer) Read(name string) (string, error) {
	abs, err := w.Resolve(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("output: read %s: %w", name, err)
	}
	return string(data), nil
}

// FileInfo is a summary of a file in the working directory.
type FileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size_bytes"`
	ModTime string `json:"modified"`
	IsDir   bool   `json:"is_dir"`
}

// List returns files and directories at the given subdirectory (empty = root).
// Non-recursive — lists one level only.
func (w *Writer) List(subdir string) ([]FileInfo, error) {
	base := w.root
	if subdir != "" {
		var err error
		base, err = w.Resolve(subdir)
		if err != nil {
			return nil, err
		}
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return []FileInfo{}, nil
		}
		return nil, fmt.Errorf("output: list %s: %w", subdir, err)
	}

	result := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(w.root, filepath.Join(base, e.Name()))
		result = append(result, FileInfo{
			Name:    e.Name(),
			Path:    rel,
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
			IsDir:   e.IsDir(),
		})
	}
	return result, nil
}

// Delete removes a file from the working directory.
func (w *Writer) Delete(name string) error {
	abs, err := w.Resolve(name)
	if err != nil {
		return err
	}
	return os.Remove(abs)
}
