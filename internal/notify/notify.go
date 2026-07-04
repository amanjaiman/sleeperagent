// Package notify delivers best-effort alerts on SleeperAgent state changes: a
// desktop notification (via the OS's native tool) and/or an optional webhook
// POST. Every delivery is fire-and-forget and never blocks or fails the loop.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

// Event is a single notification.
type Event struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Time  string `json:"time"`
}

// Notifier delivers an Event.
type Notifier interface {
	Notify(Event)
}

// Multi fans an Event out to several notifiers.
type Multi []Notifier

func (m Multi) Notify(e Event) {
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	for _, n := range m {
		if n != nil {
			n.Notify(e)
		}
	}
}

// Desktop posts a native OS notification. Unsupported platforms/tools are a
// silent no-op.
type Desktop struct{}

func (Desktop) Notify(e Event) {
	go func() {
		switch runtime.GOOS {
		case "darwin":
			script := `display notification ` + quoteAS(e.Body) + ` with title ` + quoteAS(e.Title)
			_ = exec.Command("osascript", "-e", script).Run()
		case "linux":
			_ = exec.Command("notify-send", e.Title, e.Body).Run()
		case "windows":
			// PowerShell balloon tip; best effort, no error surfaced.
			ps := `[reflection.assembly]::LoadWithPartialName('System.Windows.Forms') > $null;` +
				`$n = New-Object System.Windows.Forms.NotifyIcon;` +
				`$n.Icon = [System.Drawing.SystemIcons]::Information;` +
				`$n.Visible = $true;` +
				`$n.ShowBalloonTip(5000, ` + quotePS(e.Title) + `, ` + quotePS(e.Body) + `, 'Info')`
			_ = exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
		}
	}()
}

// Webhook POSTs the Event as JSON to URL.
type Webhook struct {
	URL  string
	HTTP *http.Client
}

func (w Webhook) Notify(e Event) {
	if w.URL == "" {
		return
	}
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	go func() {
		body, err := json.Marshal(e)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
}

// quoteAS quotes a string for AppleScript (double quotes, escape backslash/quote).
func quoteAS(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	out = append(out, '"')
	return string(out)
}

// quotePS single-quotes a string for PowerShell (double any single quote).
func quotePS(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '\'')
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'')
		}
		out = append(out, r)
	}
	out = append(out, '\'')
	return string(out)
}
