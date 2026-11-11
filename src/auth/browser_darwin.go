package auth

import (
	"context"
	"os/exec"
)

// OpenBrowser asks the desktop to open url. It returns once the opener has
// been started.
func OpenBrowser(ctx context.Context, url string) error {
	return exec.CommandContext(ctx, "open", url).Run()
}
