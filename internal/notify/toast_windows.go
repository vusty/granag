// Package notify shows a Windows toast notification.
//
// The toast is built as XML and handed to WinRT through PowerShell rather than
// through Go bindings: WinRT projections for Go are a large dependency, and a
// reminder fires at most a few times per conversation, so the couple of hundred
// milliseconds a PowerShell process costs is invisible here. The script travels
// as -EncodedCommand so nothing has to survive shell quoting.
package notify

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

// PowerShellAppID attributes the toast to Windows PowerShell.
//
// A toast needs an AppUserModelID that Windows knows, and PowerShell's is
// always present. The cost is the attribution line: the notification says
// "Windows PowerShell" rather than the tool's own name. Fixing that means
// registering an AUMID of our own through a Start Menu shortcut, which is also
// what buttons inside the toast would require.
const PowerShellAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

// Toast is one notification.
type Toast struct {
	// AppID is the AppUserModelID the toast is attributed to. Empty means
	// PowerShellAppID.
	AppID string
	Title string
	Body  string
}

const script = `$ErrorActionPreference = 'Stop'
[void][Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
[void][Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType = WindowsRuntime]
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml(@'
<toast activationType="foreground">
  <visual>
    <binding template="ToastGeneric">
      <text>%s</text>
      <text>%s</text>
    </binding>
  </visual>
</toast>
'@)
$toast = New-Object Windows.UI.Notifications.ToastNotification $xml
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('%s').Show($toast)
`

// Show displays the toast. It blocks until PowerShell has handed the
// notification to Windows, normally a few hundred milliseconds.
func (t Toast) Show() error {
	appID := t.AppID
	if appID == "" {
		appID = PowerShellAppID
	}

	ps := fmt.Sprintf(script,
		escapeXML(t.Title),
		escapeXML(t.Body),
		strings.ReplaceAll(appID, "'", "''"),
	)

	cmd := exec.Command("powershell.exe",
		"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden",
		"-EncodedCommand", encodeCommand(ps),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("toast: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// encodeCommand renders a script the way -EncodedCommand expects it: UTF-16
// little-endian, base64.
func encodeCommand(s string) string {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// escapeXML escapes the five XML entities. The text is ours, but a device or
// application name can reach it, and an unescaped ampersand makes the toast
// vanish with no error anywhere.
func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
