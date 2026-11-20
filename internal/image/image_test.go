package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSniffFormat(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nrest")
	jpeg := []byte("\xff\xd8\xff\xe0payload")
	gif87 := []byte("GIF87a...")
	gif89 := []byte("GIF89a...")
	webp := append(append([]byte("RIFF"), 0x00, 0x00, 0x00, 0x00), append([]byte("WEBP"), []byte("payload")...)...)

	tests := []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{"png", png, ".png", true},
		{"jpeg", jpeg, ".jpg", true},
		{"gif87a", gif87, ".gif", true},
		{"gif89a", gif89, ".gif", true},
		{"webp", webp, ".webp", true},
		{"empty", nil, "", false},
		{"truncated png", png[:3], "", false},
		{"plain text", []byte("hello world"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ext, ok := SniffFormat(tt.data)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, ext)
		})
	}
}

func TestWriteTemp(t *testing.T) {
	data := []byte("\x89PNG\r\n\x1a\nimage bytes")
	path, err := WriteTemp(data, ".png")
	require.NoError(t, err)
	t.Cleanup(func() { os.Remove(path) })

	assert.Equal(t, "/tmp", filepath.Dir(path))
	base := filepath.Base(path)
	assert.Contains(t, base, "lazytmux-img-")
	assert.True(t, filepath.Ext(base) == ".png", "extension must survive the random suffix: %s", base)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(data, got), "file content must round-trip")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestOSC52ReadQueries(t *testing.T) {
	assert.Equal(t, []byte("\x1b]52;c;?\x1b\\"), OSC52ReadQueryST())
	assert.Equal(t, []byte("\x1b]52;c;?\x07"), OSC52ReadQueryBEL())
}

func TestPlatformImageTool(t *testing.T) {
	env := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name    string
		goos    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{"wayland", "linux", env("WAYLAND_DISPLAY", "wayland-0"), "wl-paste", false},
		{"wayland wins over x11", "linux", env("WAYLAND_DISPLAY", "wayland-0", "DISPLAY", ":0"), "wl-paste", false},
		{"x11", "linux", env("DISPLAY", ":0"), "xclip", false},
		{"darwin", "darwin", env(), "osascript", false},
		{"linux no display", "linux", env(), "", true},
		{"windows unsupported", "windows", env(), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, args, err := platformImageTool(tt.goos, tt.env)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, name)
			assert.NotEmpty(t, args)
		})
	}
}

// TestReadPlatformImagePATHStub runs the real exec path against stub
// clipboard tools on PATH (same pattern as the fake tmux binary in
// internal/core/tmux/exec_test.go).
func TestReadPlatformImagePATHStub(t *testing.T) {
	dir := t.TempDir()
	writeStub := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		// Absolute cat: the test PATH points only at dir, so sh must not
		// have to resolve coreutils through it.
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n/bin/cat '"+content+"'\n"), 0o755))
	}

	fixture := filepath.Join(t.TempDir(), "img.png")
	require.NoError(t, os.WriteFile(fixture, []byte("\x89PNG\r\n\x1a\nfixture"), 0o644))
	writeStub("wl-paste", fixture)
	writeStub("xclip", fixture)

	t.Run("wayland", func(t *testing.T) {
		t.Setenv("PATH", dir)
		t.Setenv("WAYLAND_DISPLAY", "wayland-0")
		got, err := ReadPlatformImage(t.Context())
		require.NoError(t, err)
		assert.True(t, bytes.Equal([]byte("\x89PNG\r\n\x1a\nfixture"), got))
	})

	t.Run("x11", func(t *testing.T) {
		t.Setenv("PATH", dir)
		t.Setenv("DISPLAY", ":0")
		got, err := ReadPlatformImage(t.Context())
		require.NoError(t, err)
		assert.True(t, bytes.Equal([]byte("\x89PNG\r\n\x1a\nfixture"), got))
	})

	t.Run("missing tool", func(t *testing.T) {
		empty := t.TempDir()
		t.Setenv("PATH", empty)
		t.Setenv("WAYLAND_DISPLAY", "wayland-0")
		_, err := ReadPlatformImage(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wl-paste")
	})
}
