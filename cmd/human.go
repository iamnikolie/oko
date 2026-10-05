package cmd

import (
	"math"
	"math/rand"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Human-like input: the mouse travels along a curved path with uneven speed
// before it clicks a random point inside the target, and the page scrolls by
// wheel ticks with reading pauses instead of jumping. It matters on sites
// that watch behaviour; it also makes lazy-loaded content load as it would
// for a person.

var humanFlag bool

// human reports whether this command should use human-like input: --human,
// $OKO_HUMAN, or the profile's remembered default.
func (s *session) human() bool {
	return humanFlag || s.prof.State.Human
}

func jitter(min, max time.Duration) time.Duration {
	return min + time.Duration(rand.Int63n(int64(max-min)+1))
}

func pause(min, max time.Duration) { time.Sleep(jitter(min, max)) }

// mousePos returns where oko last left the mouse in this tab, or a plausible
// resting point when it does not know.
func (s *session) mousePos(p *rod.Page) (float64, float64) {
	if m, ok := s.prof.State.Mouse[string(p.TargetID)]; ok {
		return m[0], m[1]
	}
	vw, vh := viewportSize(p)
	return vw * (0.3 + rand.Float64()*0.4), vh * (0.4 + rand.Float64()*0.4)
}

func (s *session) setMousePos(p *rod.Page, x, y float64) {
	if s.prof.State.Mouse == nil {
		s.prof.State.Mouse = map[string][2]float64{}
	}
	s.prof.State.Mouse[string(p.TargetID)] = [2]float64{x, y}
	_ = s.prof.Save()
}

func viewportSize(p *rod.Page) (float64, float64) {
	res, err := p.Eval(`() => [innerWidth, innerHeight]`)
	if err != nil {
		return 1200, 800
	}
	a := res.Value.Arr()
	return a[0].Num(), a[1].Num()
}

// ensureVisible activates a hidden tab before human input. A hidden tab draws
// no frames, and Chrome acknowledges a stream of mouse-moved or wheel events
// only on the next frame, so Input.dispatchMouseEvent blocks until the command
// times out. A single plain move (non-human) still gets through, which is why
// only --human hung. Activating raises Chrome, so only do it when hidden.
func (s *session) ensureVisible(p *rod.Page) {
	if r, err := p.Eval(`() => document.visibilityState`); err == nil && r.Value.Str() == "hidden" {
		_ = proto.TargetActivateTarget{TargetID: p.TargetID}.Call(s.b)
		for i := 0; i < 20; i++ {
			if r, err := p.Eval(`() => document.visibilityState`); err != nil || r.Value.Str() != "hidden" {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// humanMove moves the mouse along a cubic Bézier curve with eased timing.
func (s *session) humanMove(p *rod.Page, tx, ty float64) error {
	s.ensureVisible(p)
	fx, fy := s.mousePos(p)
	dx, dy := tx-fx, ty-fy
	dist := math.Hypot(dx, dy)
	if dist < 2 {
		return nil
	}
	// Control points pushed off the straight line, on the same side, so the
	// path bows like a wrist movement.
	nx, ny := -dy/dist, dx/dist
	bow := dist * (0.08 + rand.Float64()*0.22)
	if rand.Intn(2) == 0 {
		bow = -bow
	}
	c1x, c1y := fx+dx*0.3+nx*bow, fy+dy*0.3+ny*bow
	c2x, c2y := fx+dx*0.7+nx*bow*0.6, fy+dy*0.7+ny*bow*0.6
	steps := int(math.Max(12, math.Min(45, dist/18)))
	total := time.Duration(float64(250+rand.Intn(350))*math.Min(1.6, 0.6+dist/900)) * time.Millisecond
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		e := t * t * (3 - 2*t) // ease in-out
		u := 1 - e
		x := u*u*u*fx + 3*u*u*e*c1x + 3*u*e*e*c2x + e*e*e*tx
		y := u*u*u*fy + 3*u*u*e*c1y + 3*u*e*e*c2y + e*e*e*ty
		if i < steps {
			x += rand.Float64()*1.2 - 0.6
			y += rand.Float64()*1.2 - 0.6
		}
		if err := (proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseMoved, X: x, Y: y}).Call(p); err != nil {
			return err
		}
		time.Sleep(total / time.Duration(steps))
	}
	s.setMousePos(p, tx, ty)
	return nil
}

// elementBox returns the element's viewport box.
func elementBox(el *rod.Element) (x, y, w, h float64, err error) {
	res, err := el.Eval(`function () { const r = this.getBoundingClientRect(); return [r.left, r.top, r.width, r.height] }`)
	if err != nil {
		return
	}
	a := res.Value.Arr()
	off := frameOffsets[el] // zero for top-page elements
	return a[0].Num() + off[0], a[1].Num() + off[1], a[2].Num(), a[3].Num(), nil
}

// humanScrollTo wheels until the element sits comfortably in the viewport.
func (s *session) humanScrollTo(p *rod.Page, el *rod.Element) error {
	if _, inFrame := frameOffsets[el]; inFrame {
		// Wheel events over a frame scroll the frame; bring it into view
		// directly instead, then re-read the frame offset via the box.
		_, _ = el.Eval(`function () { this.scrollIntoView({block: 'center'}) }`)
		return nil
	}
	scrollY := func() float64 {
		r, err := p.Eval(`() => scrollY`)
		if err != nil {
			return -1
		}
		return r.Value.Num()
	}
	for i := 0; i < 12; i++ {
		_, y, _, h, err := elementBox(el)
		if err != nil {
			return err
		}
		_, vh := viewportSize(p)
		if y >= 0 && y+h <= vh {
			return nil // fully visible: a person would not scroll
		}
		before := scrollY()
		if err := s.humanWheel(p, y-vh*(0.3+rand.Float64()*0.2)); err != nil {
			return err
		}
		pause(120*time.Millisecond, 300*time.Millisecond)
		if scrollY() == before {
			return nil // page cannot scroll further (or element is in a fixed layer)
		}
	}
	return nil
}

// humanWheel scrolls by dy pixels in uneven wheel ticks with short pauses.
func (s *session) humanWheel(p *rod.Page, dy float64) error {
	s.ensureVisible(p)
	x, y := s.mousePos(p)
	dir := 1.0
	if dy < 0 {
		dir = -1
	}
	left := math.Abs(dy)
	n := 0
	for left > 0 {
		step := math.Min(left, float64(60+rand.Intn(90)))
		if err := (proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseWheel, X: x, Y: y, DeltaX: 0, DeltaY: dir * step}).Call(p); err != nil {
			return err
		}
		left -= step
		n++
		if n%(4+rand.Intn(4)) == 0 {
			pause(150*time.Millisecond, 600*time.Millisecond) // reading
		} else {
			pause(25*time.Millisecond, 90*time.Millisecond)
		}
	}
	return nil
}

// humanClick scrolls the element into view, moves there, and clicks a random
// point in its inner area with a human press duration.
func (s *session) humanClick(p *rod.Page, el *rod.Element, button proto.InputMouseButton, count int) error {
	return s.humanClickAt(p, el, button, count, nil)
}

// humanClickAt is humanClick at a fixed point inside the element (fractions of
// its box); nil picks a random point in the inner area.
func (s *session) humanClickAt(p *rod.Page, el *rod.Element, button proto.InputMouseButton, count int, at *[2]float64) error {
	if err := s.humanScrollTo(p, el); err != nil {
		return err
	}
	pause(80*time.Millisecond, 250*time.Millisecond)
	x, y, w, h, err := elementBox(el)
	if err != nil {
		return err
	}
	tx := x + w*(0.25+rand.Float64()*0.5)
	ty := y + h*(0.3+rand.Float64()*0.4)
	if at != nil {
		tx, ty = x+w*at[0], y+h*at[1]
	}
	if err := s.humanMove(p, tx, ty); err != nil {
		return err
	}
	pause(60*time.Millisecond, 200*time.Millisecond)
	for i := 1; i <= count; i++ {
		if err := (proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMousePressed, X: tx, Y: ty, Button: button, ClickCount: i}).Call(p); err != nil {
			return err
		}
		pause(50*time.Millisecond, 130*time.Millisecond)
		if err := (proto.InputDispatchMouseEvent{Type: proto.InputDispatchMouseEventTypeMouseReleased, X: tx, Y: ty, Button: button, ClickCount: i}).Call(p); err != nil {
			return err
		}
		if i < count {
			pause(60*time.Millisecond, 120*time.Millisecond)
		}
	}
	return nil
}

func (s *session) humanHover(p *rod.Page, el *rod.Element) error {
	if err := s.humanScrollTo(p, el); err != nil {
		return err
	}
	x, y, w, h, err := elementBox(el)
	if err != nil {
		return err
	}
	return s.humanMove(p, x+w*(0.25+rand.Float64()*0.5), y+h*(0.3+rand.Float64()*0.4))
}

// humanType types text key by key with uneven rhythm.
func humanType(p *rod.Page, text string) error {
	for _, r := range text {
		if err := p.InsertText(string(r)); err != nil {
			return err
		}
		d := jitter(40*time.Millisecond, 160*time.Millisecond)
		if r == ' ' && rand.Intn(4) == 0 {
			d += jitter(100*time.Millisecond, 300*time.Millisecond)
		}
		time.Sleep(d)
	}
	return nil
}
