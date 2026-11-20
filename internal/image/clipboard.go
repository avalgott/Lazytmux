package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ReadPlatformImage reads an image from the local clipboard via the
// platform tool (wl-paste / xclip / osascript), selected by GOOS and the
// display environment. PATH-based, never a shell. Only useful when
// lazytmux runs on a host with the clipboard (the local scenario); over
// SSH the OSC 52 path supplies the bytes instead.
func ReadPlatformImage(ctx context.Context) ([]byte, error) {
	name, args, err := platformImageTool(runtime.GOOS, environMap())
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

// environMap flattens os.Environ into a lookup map.
func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// platformImageTool selects the clipboard read tool for the platform.
func platformImageTool(goos string, env map[string]string) (name string, args []string, err error) {
	switch goos {
	case "darwin":
		return "osascript", []string{"-e", "the clipboard as «class PNGf»"}, nil
	case "linux":
		if env["WAYLAND_DISPLAY"] != "" {
			return "wl-paste", []string{"--type", "image/png"}, nil
		}
		if env["DISPLAY"] != "" {
			return "xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}, nil
		}
		return "", nil, fmt.Errorf("no display: set WAYLAND_DISPLAY or DISPLAY and install wl-clipboard or xclip")
	case "windows":
		return "", nil, fmt.Errorf("clipboard image reads are unsupported on windows")
	default:
		return "", nil, fmt.Errorf("clipboard image reads are unsupported on %s", goos)
	}
}
