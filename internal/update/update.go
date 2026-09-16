// Package update implements lazytmux's self-update command: it compares the
// embedded version with the latest GitHub release and, when a newer release
// exists, downloads the matching prebuilt binary and atomically replaces the
// running one. Standard library only.
package update

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	repoAPI         = "https://api.github.com/repos/avalgott/Lazytmux/releases/latest"
	repoDL          = "https://github.com/avalgott/Lazytmux/releases/download"
	downloadTimeout = 2 * time.Minute
	maxJSONBytes    = 1 << 20 // cap the release metadata read
)

// releaseInfo is the subset of the GitHub releases/latest response we use.
type releaseInfo struct {
	TagName string `json:"tag_name"`
}

// assetName builds the goreleaser tarball name for this platform, matching
// install.sh (version without the leading v).
func assetName(tag, goos, goarch string) string {
	return fmt.Sprintf("lazytmux_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// versionTriplet extracts the leading MAJOR.MINOR.PATCH from a version
// string ("v0.1.0", "0.1.0", "v0.1.0-2-gabc1234-dirty"). ok is false when no
// triplet is present (e.g. dev builds without tags).
func versionTriplet(v string) (maj, min, pat int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// compareVersions returns -1, 0, or 1 for a < b, a == b, a > b. Versions
// without a numeric triplet sort as older than everything.
func compareVersions(a, b string) int {
	am, an, ap, aok := versionTriplet(a)
	bm, bn, bp, bok := versionTriplet(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	}
	for _, pair := range [][2]int{{am, bm}, {an, bn}, {ap, bp}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// Run checks for a newer release and replaces the running binary when found.
// current is the embedded version (main.version); when it has no numeric
// triplet the latest release is always installed.
func Run(current string) error {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if (goos != "linux" && goos != "darwin") || (goarch != "amd64" && goarch != "arm64") {
		return fmt.Errorf("no prebuilt binary for %s/%s — install from source instead", goos, goarch)
	}

	client := &http.Client{Timeout: downloadTimeout}

	tag, err := latestTag(client)
	if err != nil {
		return err
	}
	if compareVersions(current, tag) >= 0 {
		fmt.Printf("lazytmux is up to date (%s)\n", current)
		return nil
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}

	url := fmt.Sprintf("%s/%s/%s", repoDL, tag, assetName(tag, goos, goarch))
	fmt.Printf("updating lazytmux %s -> %s...\n", displayVersion(current), strings.TrimPrefix(tag, "v"))

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	// Extract into a temp file next to the binary so the final rename is
	// atomic and stays on the same filesystem.
	dir := filepath.Dir(executable)
	tmp, err := os.CreateTemp(dir, ".lazytmux-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	replaced := false
	defer func() {
		tmp.Close()
		if !replaced {
			os.Remove(tmpName)
		}
	}()

	if err := extractBinary(resp.Body, tmp); err != nil {
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, executable); err != nil {
		return fmt.Errorf("replace %s: %w (you may need root if the binary is in a system directory)", executable, err)
	}
	replaced = true
	fmt.Printf("updated lazytmux to %s\n", strings.TrimPrefix(tag, "v"))
	return nil
}

// displayVersion renders the current version for user-facing messages.
func displayVersion(v string) string {
	if v == "" || v == "dev" {
		return "unknown"
	}
	return v
}

// latestTag returns the tag of the latest GitHub release.
func latestTag(client *http.Client) (string, error) {
	resp, err := client.Get(repoAPI)
	if err != nil {
		return "", fmt.Errorf("check for updates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("check for updates: %s", resp.Status)
	}
	var rel releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&rel); err != nil {
		return "", fmt.Errorf("parse release info: %w", err)
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("no release found — install from source (see README)")
	}
	return rel.TagName, nil
}

// extractBinary copies the "lazytmux" entry of a tar.gz stream into w.
func extractBinary(r io.Reader, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("archive does not contain the lazytmux binary")
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == "lazytmux" {
			if _, err := io.Copy(w, tr); err != nil {
				return fmt.Errorf("extract binary: %w", err)
			}
			return nil
		}
	}
}
