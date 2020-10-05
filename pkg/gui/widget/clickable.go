package widget

import (
	"image"
	"time"

	"gioui.org/f32"
	"gioui.org/gesture"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
)

type ClickEvents struct {
	Click, Cancel, Press func()
}

// Clickable represents a clickable area.
type Clickable struct {
	click  gesture.Click
	clicks []Click
	// prevClicks is the index into clicks that marks the clicks from the most recent Fn call. prevClicks is used to
	// keep clicks bounded.
	prevClicks int
	history    []Press
	Events     ClickEvents
}

func NewClickable() (c *Clickable) {
	c = &Clickable{
		click:      gesture.Click{},
		clicks:     nil,
		prevClicks: 0,
		history:    nil,
		Events: ClickEvents{
			Click:  func() {},
			Cancel: func() {},
			Press:  func() {},
		},
	}
	return
}

func (c *Clickable) SetClick(fn func()) *Clickable {
	if c.Events.Click == nil {
		c.Events.Click = func(){}
	}
	c.Events.Click = fn
	return c
}

func (c *Clickable) SetCancel(fn func()) *Clickable {
	if c.Events.Cancel == nil {
		c.Events.Cancel = func(){}
	}
	c.Events.Cancel = fn
	return c
}

func (c *Clickable) SetPress(fn func()) *Clickable {
	if c.Events.Press == nil {
		c.Events.Press = func(){}
	}
		c.Events.Press = fn

	return c
}

// Click represents a Click.
type Click struct {
	Modifiers key.Modifiers
	NumClicks int
}

// Press represents a past pointer Press.
type Press struct {
	// Position of the Press.
	Position f32.Point
	// Start is when the Press began.
	Start time.Time
	// End is when the Press was ended by a release or Cancel. A zero End means it hasn't ended yet.
	End time.Time
	// Cancelled is true for cancelled presses.
	Cancelled bool
}

// Clicked reports whether there are pending clicks as would be reported by Clicks. If so, Clicked removes the earliest
// Click.
func (b *Clickable) Clicked() bool {
	if len(b.clicks) == 0 {
		return false
	}
	n := copy(b.clicks, b.clicks[1:])
	b.clicks = b.clicks[:n]
	if b.prevClicks > 0 {
		b.prevClicks--
	}
	return true
}

// Clicks returns and clear the clicks since the last call to Clicks.
func (b *Clickable) Clicks() []Click {
	clicks := b.clicks
	b.clicks = nil
	b.prevClicks = 0
	return clicks
}

// History is the past pointer presses useful for drawing markers. History is retained for a short duration (about a
// second).
func (b *Clickable) History() []Press {
	return b.history
}

func (b *Clickable) Fn(gtx layout.Context) layout.Dimensions {
	b.update(gtx)
	stack := op.Push(gtx.Ops)
	pointer.Rect(image.Rectangle{Max: gtx.Constraints.Min}).Add(gtx.Ops)
	b.click.Add(gtx.Ops)
	stack.Pop()
	for len(b.history) > 0 {
		c := b.history[0]
		if c.End.IsZero() || gtx.Now.Sub(c.End) < 1*time.Second {
			break
		}
		n := copy(b.history, b.history[1:])
		b.history = b.history[:n]
	}
	return layout.Dimensions{Size: gtx.Constraints.Min}
}

// update the button state by processing ClickEvents.
func (b *Clickable) update(gtx layout.Context) {
	// if this is used by old code these functions have to be empty as they are called, not nil (which will panic)
	if b.Events.Click == nil {
		b.Events.Click = func(){}
	}
	if b.Events.Cancel == nil {
		b.Events.Cancel = func(){}
	}
	if b.Events.Press == nil {
		b.Events.Press = func(){}
	}
	// Flush clicks from before the last update.
	n := copy(b.clicks, b.clicks[b.prevClicks:])
	b.clicks = b.clicks[:n]
	b.prevClicks = n

	for _, e := range b.click.Events(gtx) {
		switch e.Type {
		case gesture.TypeClick:
			click := Click{
				Modifiers: e.Modifiers,
				NumClicks: e.NumClicks,
			}
			b.clicks = append(b.clicks, click)
			if l := len(b.history); l > 0 {
				b.history[l-1].End = gtx.Now
			}
			b.Events.Click()
		case gesture.TypeCancel:
			for i := range b.history {
				b.history[i].Cancelled = true
				if b.history[i].End.IsZero() {
					b.history[i].End = gtx.Now
				}
			}
			b.Events.Cancel()
		case gesture.TypePress:
			b.history = append(b.history, Press{
				Position: e.Position,
				Start:    gtx.Now,
			})
			b.Events.Press()
		}
	}
}
