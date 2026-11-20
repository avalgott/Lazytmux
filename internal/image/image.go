// Package image handles clipboard images: format sniffing, temp-file
// storage, the platform clipboard readers, and OSC 52 query bytes.
package image

import (
	"bytes"
	"os"
)

// SniffFormat detects the image format from magic bytes and returns the
// lowercase file extension including the dot (".png"), or ok=false.
func SniffFormat(data []byte) (ext string, ok bool) {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return ".png", true
	case len(data) >= 3 && bytes.Equal(data[:3], []byte("\xff\xd8\xff")):
		return ".jpg", true
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return ".gif", true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return ".webp", true
	}
	return "", false
}

// WriteTemp writes data to a new 0600 temp file with the given extension,
// directly under /tmp. The direct /tmp path (rather than os.TempDir) keeps
// the path free of spaces and surprises when it is later typed into a pane.
func WriteTemp(data []byte, ext string) (string, error) {
	f, err := os.CreateTemp("/tmp", "lazytmux-img-*"+ext)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// OSC52ReadQueryST returns the ST-terminated OSC 52 clipboard read request.
func OSC52ReadQueryST() []byte { return []byte("\x1b]52;c;?\x1b\\") }

// OSC52ReadQueryBEL returns the BEL-terminated OSC 52 clipboard read request.
func OSC52ReadQueryBEL() []byte { return []byte("\x1b]52;c;?\x07") }
