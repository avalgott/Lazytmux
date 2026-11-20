package gui

import (
	"context"
	"fmt"
	"time"

	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/image"
)

// Clipboard image paste: Ctrl+G in fullscreen reads the image from the
// local terminal's clipboard via OSC 52 (so it works over SSH, where the
// clipboard lives on the local machine) and injects a file reference into
// the pane. If the terminal does not answer (or tmux answers the read
// itself), the platform clipboard tools are tried as a fallback.

// pasteImageHandler starts the clipboard read for the fullscreen target.
// Runs on the event loop. Guards mirror forwardEOFHandler: nothing may be
// injected while a dialog is open or a frozen snapshot is being browsed.
func (a *App) pasteImageHandler(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone || !a.fullscreen.IsActive() || a.scroll.IsActive() {
		return nil
	}
	a.pasteImageTarget = a.fullscreen.Target()
	gen := a.pasteImageGen.Add(1)
	a.pasteImageWaiting = true
	a.showBarMsg("reading clipboard image...", 5*time.Second)
	g.Update(func(*gocui.Gui) error { return nil })

	wait := a.pasteImageReadWait
	if wait <= 0 {
		wait = 1500 * time.Millisecond
	}

	// Stage 1: ST-terminated read. TtyWrite fails silently on headless
	// screens; the fallback path handles every outcome.
	_ = g.TtyWrite(image.OSC52ReadQueryST())
	time.AfterFunc(wait, func() {
		g.Update(func(*gocui.Gui) error {
			if a.pasteImageGen.Load() != gen || !a.pasteImageWaiting {
				return nil
			}
			// Stage 2: BEL-terminated read for terminals that prefer it.
			_ = g.TtyWrite(image.OSC52ReadQueryBEL())
			time.AfterFunc(wait, func() {
				g.Update(func(*gocui.Gui) error {
					if a.pasteImageGen.Load() != gen || !a.pasteImageWaiting {
						return nil
					}
					a.pasteImageWaiting = false
					a.runPlatformImageFallback(gen)
					return nil
				})
			})
			return nil
		})
	})
	return nil
}

// onClipboardData receives an OSC 52 clipboard reply on the event loop. A
// stray reply (no pending paste) is ignored; an empty reply means tmux
// answered the read itself from its own buffers, so the wait continues and
// the fallback eventually fires.
func (a *App) onClipboardData(data []byte) {
	if !a.pasteImageWaiting {
		return
	}
	if len(data) == 0 {
		return
	}
	a.pasteImageWaiting = false
	gen := a.pasteImageGen.Add(1) // cancels the pending timer stages logically
	a.completePasteImage(gen, data)
}

// runPlatformImageFallback reads the clipboard with the platform tool
// after both OSC stages timed out.
func (a *App) runPlatformImageFallback(gen uint64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, err := image.ReadPlatformImage(ctx)
		a.g.Update(func(*gocui.Gui) error {
			if a.pasteImageGen.Load() != gen {
				return nil
			}
			if err != nil {
				a.setError(err.Error())
				a.showBarMsg("no clipboard image: the terminal must answer OSC 52 reads (tmux: set -g allow-passthrough on) or install wl-clipboard/xclip", 8*time.Second)
				return nil
			}
			a.completePasteImage(gen, data)
			return nil
		})
	}()
}

// completePasteImage writes the image file and injects the reference into
// the pane. The svc call runs off the event loop; the injection is guarded
// against the user leaving fullscreen or switching sessions mid-flight.
func (a *App) completePasteImage(gen uint64, data []byte) {
	target := a.pasteImageTarget
	go func() {
		path, err := a.svc.PasteImageRef(context.Background(), target, data)
		a.g.Update(func(*gocui.Gui) error {
			if a.pasteImageGen.Load() != gen {
				return nil
			}
			if !a.fullscreen.IsActive() || a.fullscreen.Target() != target || a.scroll.IsActive() {
				return nil // the session changed while the image was in flight
			}
			if err != nil {
				a.setError(err.Error())
				a.showBarMsg("image paste failed: "+err.Error(), 8*time.Second)
				return nil
			}
			a.showBarMsg("pasted "+path, 5*time.Second)
			a.setStatus(fmt.Sprintf("pasted %s (%d bytes)", path, len(data)))
			a.refreshPreviewSoon()
			return nil
		})
	}()
}

// showBarMsg sets a transient fullscreen bar message (the logs view does
// not exist in fullscreen). Event-loop only.
func (a *App) showBarMsg(msg string, d time.Duration) {
	a.barMsg = msg
	a.barMsgUntil = time.Now().Add(d)
}
