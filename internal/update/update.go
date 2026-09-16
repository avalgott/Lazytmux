// Package update implements lazytmux's self-update command: it compares the
// embedded version with the latest GitHub release and, when a newer release
// exists, downloads the matching prebuilt binary and atomically replaces the
// running one. Standard library only.
package update

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// gitDescribeSuffix matches the suffixes `git describe` appends to tags:
// "-<count>-g<abbrev>" with an optional "-dirty". Anything else after the
// version triplet (rc.1, beta, and SemVer's numeric prerelease identifiers
// like "0" or "20260916") is a real prerelease marker.
var gitDescribeSuffix = regexp.MustCompile(`^[0-9]+-g[0-9a-f]+(-dirty)?$`)

const (
	userAgent       = "lazytmux"
	downloadTimeout = 2 * time.Minute
	maxJSONBytes    = 1 << 20 // cap the release metadata read
)

// The API and download endpoints are vars so tests can point them at an
// httptest server.
var (
	repoAPI = "https://api.github.com/repos/avalgott/Lazytmux/releases/latest"
	repoDL  = "https://github.com/avalgott/Lazytmux/releases/download"
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
// triplet is present (e.g. dev builds without tags). prerelease is true when
// the version carries anything that is not a git describe suffix after the
// triplet — rc/beta/alpha markers and SemVer numeric identifiers alike.
func versionTriplet(v string) (maj, min, pat int, prerelease, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")

	// Strip build metadata before looking for a prerelease suffix: build
	// metadata may itself contain hyphens ("+build-7") and never affects
	// ordering.
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	rest := ""
	if i := strings.Index(v, "-"); i >= 0 {
		rest = v[i:]
		v = v[:i]
	}

	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false, false
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false, false
		}
		nums[i] = n
	}

	marker := strings.TrimPrefix(rest, "-")
	prerelease = marker != "" && !gitDescribeSuffix.MatchString(marker)
	return nums[0], nums[1], nums[2], prerelease, true
}

// compareVersions returns -1, 0, or 1 for a < b, a == b, a > b. Versions
// without a numeric triplet sort as older than everything, and a prerelease
// sorts below the release with the same triplet (so a binary built from
// v0.1.0-rc.1 still updates to v0.1.0).
func compareVersions(a, b string) int {
	am, an, ap, apre, aok := versionTriplet(a)
	bm, bn, bp, bpre, bok := versionTriplet(b)
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
	switch {
	case apre && !bpre:
		return -1
	case !apre && bpre:
		return 1
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

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}

	return run(current, goos, goarch, executable, &http.Client{Timeout: downloadTimeout})
}

// run performs the update against the resolved endpoints. Split out of Run
// so tests can inject the platform, the executable path, and an httptest
// server.
func run(current, goos, goarch, executable string, client *http.Client) error {
	tag, err := latestTag(client)
	if err != nil {
		return err
	}
	if compareVersions(current, tag) >= 0 {
		fmt.Printf("lazytmux is up to date (%s)\n", current)
		return nil
	}

	asset := assetName(tag, goos, goarch)
	url := fmt.Sprintf("%s/%s/%s", repoDL, tag, asset)
	fmt.Printf("updating lazytmux %s -> %s...\n", displayVersion(current), strings.TrimPrefix(tag, "v"))

	// Verify the asset against the release's published checksums before
	// executing anything: the download replaces the running binary.
	checksums, err := fetchChecksums(client, tag)
	if err != nil {
		return err
	}
	expected, ok := checksums[asset]
	if !ok {
		return fmt.Errorf("release %s has no checksum for %s in checksums.txt", tag, asset)
	}

	resp, err := getWithUserAgent(client, url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	// Stage everything in temp files next to the binary so the final rename
	// is atomic and stays on the same filesystem.
	dir := filepath.Dir(executable)
	tarball, err := os.CreateTemp(dir, ".lazytmux-update-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tarballName := tarball.Name()
	// The archive is always temporary: it must not linger next to the
	// executable after a successful update.
	defer tarball.Close()
	defer os.Remove(tarballName)

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

	if _, err := io.Copy(tarball, resp.Body); err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := tarball.Close(); err != nil {
		return err
	}

	sum, err := sha256Hex(tarballName)
	if err != nil {
		return err
	}
	if sum != expected {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s)", asset, sum, expected)
	}

	f, err := os.Open(tarballName)
	if err != nil {
		return err
	}
	if err := extractBinary(f, tmp); err != nil {
		f.Close()
		return err
	}
	f.Close()

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

// fetchChecksums downloads and parses the release's checksums.txt into a
// map of asset name -> sha256 hex digest.
func fetchChecksums(client *http.Client, tag string) (map[string]string, error) {
	url := fmt.Sprintf("%s/%s/checksums.txt", repoDL, tag)
	resp, err := getWithUserAgent(client, url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}
	return parseChecksums(string(content)), nil
}

// parseChecksums parses goreleaser's checksums.txt format: one
// "<sha256>  <name>" line per asset.
func parseChecksums(content string) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		sums[fields[1]] = strings.ToLower(fields[0])
	}
	return sums
}

// sha256Hex returns the sha256 digest of the file at path, hex-encoded.
func sha256Hex(path string) (string, error) {
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

// displayVersion renders the current version for user-facing messages.
func displayVersion(v string) string {
	if v == "" || v == "dev" {
		return "unknown"
	}
	return v
}

// getWithUserAgent performs a GET with an explicit product User-Agent;
// GitHub's API rejects requests without one.
func getWithUserAgent(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return client.Do(req)
}

// latestTag returns the tag of the latest GitHub release.
func latestTag(client *http.Client) (string, error) {
	resp, err := getWithUserAgent(client, repoAPI)
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
