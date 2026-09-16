package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

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
		{"v0.1.0-rc.1", 0, 1, 0, true, true},
		{"v0.2.0-beta", 0, 2, 0, true, true},
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
		{"v0.1.0-rc.1", "v0.1.0", -1},  // prereleases update to the release
		{"v0.1.0", "v0.1.0-rc.1", 1},
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
