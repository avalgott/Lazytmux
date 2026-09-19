package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePlan writes a plan YAML file under the test's config dir
// (<XDG_CONFIG_HOME>/lazytmux/plans/<name>.yaml).
func writePlan(t *testing.T, name, yaml string) {
	t.Helper()
	dir, err := os.UserConfigDir()
	require.NoError(t, err)
	path := filepath.Join(dir, "lazytmux", "plans", name+".yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))
}

func TestLoadValid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := filepath.Join(home, "projects", "myapp")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "backend"), 0o755))
	personal := filepath.Join(home, "personal")
	require.NoError(t, os.MkdirAll(personal, 0o755))
	absCwd := t.TempDir() // absolute, outside home

	writePlan(t, "myapp", fmt.Sprintf(`
version: 1
name: myapp
root: ~/projects/myapp
sessions:
  - name: web
    command: docker compose up web
  - name: worker
    cwd: backend
    command: docker compose exec php bin/console messenger:consume async
  - name: logs
    cwd: %s
    command: docker compose logs -f web
  - name: shell
  - name: other
    cwd: ~/personal
`, absCwd))

	p, err := Load("myapp")
	require.NoError(t, err)

	assert.Equal(t, 1, p.Version)
	assert.Equal(t, "myapp", p.Name)
	assert.Equal(t, root, p.Root)
	require.Len(t, p.Sessions, 5)

	assert.Equal(t, Session{Name: "web", Cwd: root, Command: "docker compose up web"}, p.Sessions[0])
	assert.Equal(t, Session{Name: "worker", Cwd: filepath.Join(root, "backend"), Command: "docker compose exec php bin/console messenger:consume async"}, p.Sessions[1])
	assert.Equal(t, Session{Name: "logs", Cwd: absCwd, Command: "docker compose logs -f web"}, p.Sessions[2])
	assert.Equal(t, Session{Name: "shell", Cwd: root}, p.Sessions[3], "no session cwd means the plan root")
	assert.Equal(t, Session{Name: "other", Cwd: personal}, p.Sessions[4])
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "missing version", yaml: "name: myapp\nsessions:\n  - name: web\n", wantErr: "missing version"},
		{name: "unsupported version", yaml: "version: 2\nname: myapp\nsessions:\n  - name: web\n", wantErr: "unsupported version 2"},
		{name: "missing plan name", yaml: "version: 1\nsessions:\n  - name: web\n", wantErr: "plan name is required"},
		{name: "name mismatch", yaml: "version: 1\nname: otherapp\nsessions:\n  - name: web\n", wantErr: `plan name "otherapp" does not match the requested plan "myapp"`},
		{name: "empty sessions", yaml: "version: 1\nname: myapp\nsessions: []\n", wantErr: "at least one session"},
		{name: "sessions key missing", yaml: "version: 1\nname: myapp\n", wantErr: "at least one session"},
		{name: "unknown field", yaml: "version: 1\nname: myapp\nsesssions:\n  - name: web\n", wantErr: "field sesssions not found"},
		{name: "duplicate session names", yaml: "version: 1\nname: myapp\nsessions:\n  - name: web\n  - name: web\n", wantErr: "duplicate name"},
		{name: "empty session name", yaml: "version: 1\nname: myapp\nsessions:\n  - name: \"\"\n", wantErr: "invalid name"},
		{name: "session name with colon", yaml: "version: 1\nname: myapp\nsessions:\n  - name: \"a:b\"\n", wantErr: "invalid character"},
		{name: "session name with semicolon", yaml: "version: 1\nname: myapp\nsessions:\n  - name: \"a;b\"\n", wantErr: "unsafe character"},
		{name: "relative cwd without root", yaml: "version: 1\nname: myapp\nsessions:\n  - name: web\n    cwd: backend\n", wantErr: "relative cwd but the plan has no root"},
		{name: "relative root", yaml: "version: 1\nname: myapp\nroot: projects/myapp\nsessions:\n  - name: web\n", wantErr: "must be absolute or start with ~"},
		{name: "malformed yaml", yaml: "version: [1\n", wantErr: "parse plan"},
		{name: "multi-document", yaml: "version: 1\nname: myapp\nsessions:\n  - name: web\n---\nversion: 1\n", wantErr: "single YAML document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			writePlan(t, "myapp", tt.yaml)
			_, err := Load("myapp")
			if assert.Error(t, err, "want error containing %q", tt.wantErr) {
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadPathErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    func(t *testing.T) string
		wantErr string
	}{
		{name: "root missing", yaml: func(t *testing.T) string {
			return "version: 1\nname: myapp\nroot: /nonexistent-lazytmux-root\nsessions:\n  - name: web\n"
		}, wantErr: "does not exist"},
		{name: "root is a file", yaml: func(t *testing.T) string {
			f := filepath.Join(t.TempDir(), "rootfile")
			require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
			return fmt.Sprintf("version: 1\nname: myapp\nroot: %s\nsessions:\n  - name: web\n", f)
		}, wantErr: "is not a directory"},
		{name: "cwd missing", yaml: func(t *testing.T) string {
			return fmt.Sprintf("version: 1\nname: myapp\nroot: %s\nsessions:\n  - name: web\n    cwd: /nonexistent-lazytmux-cwd\n", t.TempDir())
		}, wantErr: "does not exist"},
		{name: "cwd is a file", yaml: func(t *testing.T) string {
			f := filepath.Join(t.TempDir(), "cwdfile")
			require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
			return fmt.Sprintf("version: 1\nname: myapp\nroot: %s\nsessions:\n  - name: web\n    cwd: %s\n", t.TempDir(), f)
		}, wantErr: "is not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			writePlan(t, "myapp", tt.yaml(t))
			_, err := Load("myapp")
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadNotFound(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	_, err := Load("myapp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `plan "myapp" not found at`)
}

func TestLoadRejectsInvalidPlanName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	for _, name := range []string{"", "   ", "../escape", "sub/dir"} {
		_, err := Load(name)
		require.Error(t, err, "plan name %q must be rejected", name)
	}
}

func TestLoadNoUserConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	_, err := Load("myapp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config dir")
}
