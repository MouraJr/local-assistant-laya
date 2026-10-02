//go:build !windows && !darwin

package notify

import (
	"fmt"
	"os/exec"
	"strings"
)

// sound plays an alert, trying the common desktop players in turn.
func sound() error {
	type cmdSpec struct {
		bin  string
		args []string
	}
	candidates := []cmdSpec{
		{"paplay", []string{"/usr/share/sounds/freedesktop/stereo/message.oga"}},
		{"aplay", []string{"/usr/share/sounds/alsa/Front_Center.wav"}},
		{"canberra-gtk-play", []string{"-i", "message"}},
		{"pw-play", nil},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c.bin); err != nil {
			continue
		}
		if out, err := exec.Command(c.bin, c.args...).CombinedOutput(); err == nil {
			return nil
		} else if len(out) > 0 {
			// Keep looking: a missing sound file is not worth reporting when
			// another player might work.
			continue
		}
	}
	return fmt.Errorf("linux: no working sound player found")
}

// toast uses the freedesktop notification daemon.
func toast(title, body string) error {
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("notify-send not installed: %w", err)
	}
	// Separate argv entries, so a label containing quotes is inert.
	cmd := exec.Command(bin, "--app-name=Local Assistant", "--urgency=normal",
		truncate(title, 120), truncate(body, 400))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
