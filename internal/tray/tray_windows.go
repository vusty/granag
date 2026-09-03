// Package tray puts granag in the notification area, which is the whole user
// interface: an icon that says whether reminders are on, and a menu to turn
// them off while gaming.
package tray

import (
	_ "embed"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

//go:embed icons/listening.ico
var iconListening []byte

//go:embed icons/paused.ico
var iconPaused []byte

// PauseFor is how long "pause for an hour" lasts before reminders come back on
// their own. Forgetting to unpause is the failure mode a plain toggle has.
const PauseFor = time.Hour

// Options wires the tray to the rest of the tool.
type Options struct {
	// OnPause is called whenever reminders are suppressed or resumed,
	// including when a timed pause expires by itself.
	OnPause func(paused bool)
	// OnOpenGranola brings Granola up, from the menu item that saves finding
	// it in the taskbar.
	OnOpenGranola func() error
	// OnQuit runs after the tray closes, to stop the rest of the process.
	OnQuit func()
}

var mu sync.Mutex

// Run shows the icon and blocks until the user quits. It must be called on the
// main goroutine: the tray owns a window message loop.
func Run(o Options) {
	systray.Run(func() { ready(o) }, o.OnQuit)
}

// SetStatus replaces the icon's tooltip. Safe to call from any goroutine.
func SetStatus(s string) {
	mu.Lock()
	defer mu.Unlock()
	systray.SetTooltip("granag — " + s)
}

func ready(o Options) {
	systray.SetIcon(iconListening)
	systray.SetTitle("granag")
	SetStatus("слежу за разговорами")

	open := systray.AddMenuItem("Открыть Granola", "")
	systray.AddSeparator()
	pause := systray.AddMenuItemCheckbox("Пауза", "", false)
	pauseHour := systray.AddMenuItem("Пауза на час", "")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Выход", "")

	go func() {
		// A timed pause fires on this channel; a nil channel blocks forever,
		// which is what an untimed pause wants.
		var expiry <-chan time.Time
		var timer *time.Timer

		cancelTimer := func() {
			if timer != nil {
				timer.Stop()
				timer, expiry = nil, nil
			}
		}
		setPaused := func(p bool) {
			if p {
				pause.Check()
				systray.SetIcon(iconPaused)
				SetStatus("пауза")
			} else {
				pause.Uncheck()
				systray.SetIcon(iconListening)
				SetStatus("слежу за разговорами")
			}
			o.OnPause(p)
		}

		for {
			select {
			case <-open.ClickedCh:
				if o.OnOpenGranola != nil {
					if err := o.OnOpenGranola(); err != nil {
						SetStatus("не смог открыть Granola")
					}
				}

			case <-pause.ClickedCh:
				cancelTimer()
				setPaused(!pause.Checked())

			case <-pauseHour.ClickedCh:
				cancelTimer()
				timer = time.NewTimer(PauseFor)
				expiry = timer.C
				setPaused(true)
				SetStatus("пауза на час")

			case <-expiry:
				cancelTimer()
				setPaused(false)

			case <-quit.ClickedCh:
				cancelTimer()
				systray.Quit()
				return
			}
		}
	}()
}
