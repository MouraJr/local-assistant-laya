//go:build darwin

package notify

import (
	"fmt"
	"os/exec"
	"strconv"
)

// sound plays the system ping.
func sound() error {
	for _, candidate := range []string{
		"/System/Library/Sounds/Ping.aiff",
		"/System/Library/Sounds/Glass.aiff",
	} {
		if _, err := exec.LookPath("afplay"); err == nil {
			if out, err := exec.Command("afplay", candidate).CombinedOutput(); err == nil {
				return nil
			} else {
				_ = out
			}
		}
	}
	// Fall back to a terminal bell, which works even in a bare session.
	if _, err := exec.LookPath("osascript"); err == nil {
		if out, err := exec.Command("osascript", "-e", "beep").CombinedOutput(); err == nil {
			return nil
		} else {
			_ = out
		}
	}
	return fmt.Errorf("darwin: no working sound player")
}

// toast shows a Notification Center banner.
//
// osascript takes its arguments as separate argv entries, never as a script
// string, so user-controlled task labels cannot become AppleScript source.
func toast(title, body string) error {
	script := fmt.Sprintf(
		`display notification %s with title %s`,
		asString(body), asString(title))
	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}

func asString(s string) string { return strconv.Quote(s) }
