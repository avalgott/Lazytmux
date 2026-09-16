package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionTriplet(t *testing.T) {
	cases := []struct {
		in         string
		maj, min   int
		pat        int
		prerelease bool
		ok         bool
	}{
		{"v0.1.0", 0, 1, 0, false, true},
		{"0.1.0", 0, 1, 0, false, true},
		{"v0.1.0-2-gabc1234-dirty", 0, 1, 0, false, true},
		{"v0.1.0-dirty", 0, 1, 0, false, true},
		{"v0.1.0-rc.1", 0, 1, 0, true, true},
		{"v0.2.0-beta", 0, 2, 0, true, true},
		{"v1.0.0-0", 1, 0, 0, true, true},
		{"v1.0.0-20260916", 1, 0, 0, true, true},
		{"v1.0.0+build-7", 1, 0, 0, false, true},
		{"v0.1.0-rc.1+build-7", 0, 1, 0, true, true},
		{"v1.12.3", 1, 12, 3, false, true},
		{"dev", 0, 0, 0, false, false},
		{"abc1234-dirty", 0, 0, 0, false, false},
		{"v1.2", 0, 0, 0, false, false},
		{"", 0, 0, 0, false, false},
	}
	for _, c := range cases {
		maj, min, pat, pre, ok := versionTriplet(c.in)
		assert.Equal(t, c.maj, maj, "major of %q", c.in)
		assert.Equal(t, c.min, min, "minor of %q", c.in)
		assert.Equal(t, c.pat, pat, "patch of %q", c.in)
		assert.Equal(t, c.prerelease, pre, "prerelease of %q", c.in)
		assert.Equal(t, c.ok, ok, "ok of %q", c.in)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0},
		{"0.1.0", "v0.1.0", 0},
		{"v0.0.1", "v0.1.0", -1},
		{"v0.2.0", "v0.1.0", 1},
		{"v0.1.10", "v0.1.9", 1},
		{"v0.1.0-2-gabc", "v0.1.0", 0}, // git describe suffix does not count
		{"v0.1.0-dirty", "v0.1.0", 0},  // dirty-at-tag is not a prerelease
		{"v0.1.0-rc.1", "v0.1.0", -1},  // prereleases update to the release
		{"v0.1.0", "v0.1.0-rc.1", 1},
		{"v1.0.0-0", "v1.0.0", -1}, // SemVer numeric prerelease identifiers
		{"v1.0.0-20260916", "v1.0.0", -1},
		{"v1.0.0+build-7", "v1.0.0", 0}, // build metadata never affects ordering
		{"v0.2.0-beta", "v0.1.0", 1},
		{"dev", "v0.1.0", -1}, // unknown versions are older
		{"abc1234", "v0.1.0", -1},
		{"v0.1.0", "dev", 1},
		{"dev", "unknown", 0},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, compareVersions(c.a, c.b), "compare %q vs %q", c.a, c.b)
	}
}

func TestAssetName(t *testing.T) {
	assert.Equal(t, "lazytmux_0.1.0_linux_amd64.tar.gz", assetName("v0.1.0", "linux", "amd64"))
	assert.Equal(t, "lazytmux_1.2.3_darwin_arm64.tar.gz", assetName("1.2.3", "darwin", "arm64"))
}

func TestExtractBinary(t *testing.T) {
	// Build a tar.gz with two entries; extraction must pick "lazytmux".
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	writeEntry := func(name string, data string) {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}))
		_, err := tw.Write([]byte(data))
		require.NoError(t, err)
	}
	writeEntry("README", "not the binary")
	writeEntry("lazytmux", "BINARY-CONTENT")
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	var out bytes.Buffer
	require.NoError(t, extractBinary(&buf, &out))
	assert.Equal(t, "BINARY-CONTENT", out.String())
}

func TestParseChecksums(t *testing.T) {
	content := strings.Join([]string{
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  lazytmux_0.1.0_linux_amd64.tar.gz",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  lazytmux_0.1.0_darwin_arm64.tar.gz",
		"not a checksum line",
	}, "\n") + "\n"

	sums := parseChecksums(content)
	assert.Equal(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", sums["lazytmux_0.1.0_linux_amd64.tar.gz"])
	assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", sums["lazytmux_0.1.0_darwin_arm64.tar.gz"])
	_, ok := sums["lazytmux_0.1.0_windows_amd64.tar.gz"]
	assert.False(t, ok)
}

func TestSha256Hex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(path, []byte("hello\n"), 0o644))

	sum, err := sha256Hex(path)
	require.NoError(t, err)
	// sha256("hello\n") — precomputed.
	assert.Equal(t, "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03", sum)
}

func TestExtractBinaryMissing(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "README", Mode: 0o644, Size: 4}))
	_, err := tw.Write([]byte("text"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	var out bytes.Buffer
	err = extractBinary(&buf, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not contain")
}

func makeTarball(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "lazytmux", Mode: 0o755, Size: int64(len(content))}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newUpdateServer serves a fake GitHub release: a latest-tag JSON endpoint
// and a download area with checksums.txt and the platform tarball.
func newUpdateServer(t *testing.T, tag string, tarball []byte, sum string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": %q}`, tag)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			asset := fmt.Sprintf("lazytmux_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
			return
		}
		_, _ = w.Write(tarball)
	})
	return httptest.NewServer(mux)
}

func TestRunFlow(t *testing.T) {
	tarball := makeTarball(t, "NEW-BINARY")
	goodSum := sha256HexBytes(tarball)

	setup := func(t *testing.T, tag, checksum string) (executable, dir string) {
		t.Helper()
		dir = t.TempDir()
		executable = filepath.Join(dir, "lazytmux")
		require.NoError(t, os.WriteFile(executable, []byte("OLD-BINARY"), 0o755))

		srv := newUpdateServer(t, tag, tarball, checksum)
		t.Cleanup(srv.Close)
		oldAPI, oldDL := repoAPI, repoDL
		repoAPI = srv.URL + "/releases/latest"
		repoDL = srv.URL + "/download"
		t.Cleanup(func() { repoAPI, repoDL = oldAPI, oldDL })
		return executable, dir
	}

	assertNoLeftovers := func(t *testing.T, dir string) {
		t.Helper()
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			assert.NotContains(t, e.Name(), ".lazytmux-update-", "no temp files may remain")
		}
	}

	client := &http.Client{Timeout: time.Second}

	t.Run("up to date skips the download", func(t *testing.T) {
		exe, _ := setup(t, "v0.1.0", goodSum)
		require.NoError(t, run("v0.1.0", runtime.GOOS, runtime.GOARCH, exe, client))
		data, err := os.ReadFile(exe)
		require.NoError(t, err)
		assert.Equal(t, "OLD-BINARY", string(data))
	})

	t.Run("updates and cleans up", func(t *testing.T) {
		exe, dir := setup(t, "v0.2.0", goodSum)
		require.NoError(t, run("v0.1.0", runtime.GOOS, runtime.GOARCH, exe, client))
		data, err := os.ReadFile(exe)
		require.NoError(t, err)
		assert.Equal(t, "NEW-BINARY", string(data))
		assertNoLeftovers(t, dir)
	})

	t.Run("checksum mismatch aborts without touching the binary", func(t *testing.T) {
		exe, dir := setup(t, "v0.2.0", strings.Repeat("0", 64))
		err := run("v0.1.0", runtime.GOOS, runtime.GOARCH, exe, client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "checksum mismatch")
		data, readErr := os.ReadFile(exe)
		require.NoError(t, readErr)
		assert.Equal(t, "OLD-BINARY", string(data))
		assertNoLeftovers(t, dir)
	})
}
