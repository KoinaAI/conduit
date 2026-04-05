package observability

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

func newRotatingWriter(path string, maxSizeMB, maxBackups int) (*rotatingWriter, error) {
	if maxSizeMB <= 0 {
		return nil, fmt.Errorf("log max size must be greater than 0")
	}
	if maxBackups <= 0 {
		return nil, fmt.Errorf("log max backups must be greater than 0")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &rotatingWriter{
		path:       path,
		maxBytes:   int64(maxSizeMB) << 20,
		maxBackups: maxBackups,
		file:       file,
		size:       info.Size(),
	}, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, fmt.Errorf("rotating writer is closed")
	}
	if w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	w.size = 0
	return err
}

func (w *rotatingWriter) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	if err := os.Remove(rotatedLogPath(w.path, w.maxBackups)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for index := w.maxBackups - 1; index >= 1; index-- {
		source := rotatedLogPath(w.path, index)
		if _, err := os.Stat(source); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.Rename(source, rotatedLogPath(w.path, index+1)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(w.path); err == nil {
		if err := os.Rename(w.path, rotatedLogPath(w.path, 1)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.file = file
	w.size = 0
	return nil
}

func rotatedLogPath(path string, index int) string {
	return fmt.Sprintf("%s.%d", path, index)
}

var _ io.WriteCloser = (*rotatingWriter)(nil)
