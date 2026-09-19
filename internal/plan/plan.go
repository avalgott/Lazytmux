// Package plan implements Session Plans: declarative YAML files that name
// tmux sessions for a development workflow. Load parses and fully validates
// a plan; session.Service applies it by creating missing sessions once.
package plan

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/avalgott/Lazytmux/internal/core/tmux"
)

// Session is one planned tmux session. Cwd is the resolved absolute working
// directory (empty = tmux default, i.e. neither plan root nor session cwd
// were set). Command is the raw command (empty = normal shell).
type Session struct {
	Name    string
	Cwd     string
	Command string
}

// Plan is a fully validated session plan. Root is the resolved absolute plan
// root, or "" when the plan declares none.
type Plan struct {
	Version  int
	Name     string
	Root     string
	Sessions []Session
}

// filePlan mirrors the YAML wire format. The raw strings are resolved and
// validated into Plan by Load.
type filePlan struct {
	Version  int           `yaml:"version"`
	Name     string        `yaml:"name"`
	Root     string        `yaml:"root"`
	Sessions []fileSession `yaml:"sessions"`
}

type fileSession struct {
	Name    string `yaml:"name"`
	Cwd     string `yaml:"cwd"`
	Command string `yaml:"command"`
}

// Load reads, parses, and fully validates the session plan
// <config-dir>/lazytmux/plans/<name>.yaml. The whole plan is validated before
// any session is created, so an invalid plan never results in a partially
// created environment. The YAML name field must equal the requested plan
// name: a plan has exactly one identity.
func Load(name string) (*Plan, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("plan name is required")
	}
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("invalid plan name %q", name)
	}

	dir, err := configDir()
	if err != nil {
		return nil, fmt.Errorf("config dir: %w", err)
	}
	path := filepath.Join(dir, "lazytmux", "plans", name+".yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("plan %q not found at %s", name, path)
		}
		return nil, fmt.Errorf("read plan %s: %w", path, err)
	}

	var fp filePlan
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fp); err != nil {
		return nil, fmt.Errorf("parse plan %s: %w", path, err)
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse plan %s: must contain a single YAML document", path)
		}
		return nil, fmt.Errorf("parse plan %s: %w", path, err)
	}

	return resolve(fp, name)
}

// configDir returns the user config directory (~/.config by default,
// respecting XDG_CONFIG_HOME).
func configDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		if home, herr := os.UserHomeDir(); herr == nil && home != "" {
			return filepath.Join(home, ".config"), nil
		}
		return "", err
	}
	return dir, nil
}

// resolve validates the raw file plan and resolves all paths. Every error is
// reported before any session is created.
func resolve(fp filePlan, requested string) (*Plan, error) {
	switch {
	case fp.Version == 0:
		return nil, fmt.Errorf("missing version (expected 1)")
	case fp.Version != 1:
		return nil, fmt.Errorf("unsupported version %d (only 1 is supported)", fp.Version)
	}

	fp.Name = strings.TrimSpace(fp.Name)
	if fp.Name == "" {
		return nil, fmt.Errorf("plan name is required")
	}
	if fp.Name != requested {
		return nil, fmt.Errorf("plan name %q does not match the requested plan %q", fp.Name, requested)
	}
	if len(fp.Sessions) == 0 {
		return nil, fmt.Errorf("plan %q must define at least one session", fp.Name)
	}

	var root string
	if fp.Root = strings.TrimSpace(fp.Root); fp.Root != "" {
		r, err := expandHome(fp.Root)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("root %q must be absolute or start with ~", fp.Root)
		}
		r = filepath.Clean(r)
		if err := checkDir(r); err != nil {
			return nil, err
		}
		root = r
	}

	seen := make(map[string]struct{}, len(fp.Sessions))
	sessions := make([]Session, 0, len(fp.Sessions))
	for i, fs := range fp.Sessions {
		fs.Name = strings.TrimSpace(fs.Name)
		if err := tmux.ValidateSessionName(fs.Name); err != nil {
			return nil, fmt.Errorf("session %d: invalid name %q: %w", i, fs.Name, err)
		}
		if _, dup := seen[fs.Name]; dup {
			return nil, fmt.Errorf("session %d: duplicate name %q", i, fs.Name)
		}
		seen[fs.Name] = struct{}{}

		cwd, err := resolveCwd(fs.Cwd, root, fs.Name)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, Session{Name: fs.Name, Cwd: cwd, Command: fs.Command})
	}

	return &Plan{Version: fp.Version, Name: fp.Name, Root: root, Sessions: sessions}, nil
}

// resolveCwd resolves one session's working directory: empty means the plan
// root, a relative path is resolved against the plan root (which must
// therefore exist), an absolute path is used as-is. ~ is expanded and the
// result must exist and be a directory.
func resolveCwd(raw, root, sessionName string) (string, error) {
	cwd := strings.TrimSpace(raw)
	if cwd == "" {
		return root, nil
	}
	p, err := expandHome(cwd)
	if err != nil {
		return "", fmt.Errorf("session %q: %w", sessionName, err)
	}
	if !filepath.IsAbs(p) {
		if root == "" {
			return "", fmt.Errorf("session %q has a relative cwd but the plan has no root", sessionName)
		}
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	if err := checkDir(p); err != nil {
		return "", err
	}
	return p, nil
}

// expandHome expands a leading ~ (or ~/) to the user's home directory.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return p, nil
}

// checkDir reports an error unless path exists and is a directory.
func checkDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("path %q does not exist", path)
		}
		return fmt.Errorf("path %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %q is not a directory", path)
	}
	return nil
}
