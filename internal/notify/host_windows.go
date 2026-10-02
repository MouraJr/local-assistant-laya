//go:build windows

package notify

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sound plays a short alert using the Windows system sound. SystemSounds has
// no external dependency, unlike Beep, which needs a console handle this
// process does not have when run as a service.
func sound() error {
	const script = `
$ErrorActionPreference = 'Stop'
[System.Reflection.Assembly]::LoadWithPartialName('System.Windows.Forms') | Out-Null
[System.Media.SystemSounds]::Exclamation.Play()
Start-Sleep -Milliseconds 400
[System.Media.SystemSounds]::Exclamation.Play()
`
	return runPowerShell(script)
}

// toast shows a Windows toast notification.
//
// Title and body arrive base64-encoded and are inserted as XML text nodes
// rather than interpolated into a template. Task labels are user-controlled
// strings, so building the toast by string concatenation would be an
// injection vector; this is not.
func toast(title, body string) error {
	const script = `
$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null

$title = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($args[0]))
$body  = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($args[1]))

$xml = '<toast><visual><binding template="ToastGeneric">' +
       '<text id="title"></text><text id="body"></text>' +
       '</binding></visual></toast>'

$doc = New-Object Windows.Data.Xml.Dom.XmlDocument
$doc.LoadXml($xml)

$titleNode = $doc.SelectSingleNode('//text[@id="title"]')
$titleNode.AppendChild($doc.CreateTextNode($title)) | Out-Null
$bodyNode = $doc.SelectSingleNode('//text[@id="body"]')
$bodyNode.AppendChild($doc.CreateTextNode($body)) | Out-Null

$toast = New-Object Windows.UI.Notifications.ToastNotification $doc
$appId = [Diagnostics.Process]::GetCurrentProcess().ProcessName
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($appId).Show($toast)
`

	// The script is written to a temp file so that no user text ever reaches
	// the PowerShell command line.
	path, err := writeTempScript(script)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", path,
		base64.StdEncoding.EncodeToString([]byte(title)),
		base64.StdEncoding.EncodeToString([]byte(body)),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("windows toast: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runPowerShell(script string) error {
	path, err := writeTempScript(script)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("powershell: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeTempScript(script string) (string, error) {
	f, err := os.CreateTemp("", "localassistant-*.ps1")
	if err != nil {
		return "", fmt.Errorf("create temp script: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(script); err != nil {
		return "", fmt.Errorf("write temp script: %w", err)
	}
	return filepath.Clean(f.Name()), nil
}
