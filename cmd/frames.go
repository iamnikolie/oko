package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
)

// Cross-origin iframes (payment widgets, embedded forms, captchas) are not
// reachable from the page's JavaScript, so the in-page snapshot cannot see
// them. oko lists them from the top page, attaches to each frame through CDP
// and snapshots it separately; their refs carry the frame number: f1e3.

// framesJS registers visible cross-origin frames in window.__okoFrames (the
// order defines f1, f2, …) and describes them.
const framesJS = `function () {
  const out = [], list = [];
  for (const f of document.querySelectorAll('iframe, frame')) {
    let same = false;
    try { same = !!f.contentDocument; } catch (e) {}
    if (same) continue; // same-origin frames are inlined by the snapshot itself
    const r = f.getBoundingClientRect();
    if (r.width < 40 || r.height < 40) continue;
    const st = getComputedStyle(f);
    if (st.display === 'none' || st.visibility === 'hidden') continue;
    list.push(f);
    let origin = '';
    try { origin = new URL(f.src, location.href).origin; } catch (e) {}
    out.push({ title: f.title || f.name || '', origin });
  }
  window.__okoFrames = list;
  return out;
}`

type frameInfo struct {
	Title  string `json:"title"`
	Origin string `json:"origin"`
}

func crossFrames(p *rod.Page) ([]frameInfo, error) {
	raw, err := evalJSON(p, framesJS)
	if err != nil {
		return nil, err
	}
	var fs []frameInfo
	err = json.Unmarshal(raw, &fs)
	return fs, err
}

// framePage attaches to frame n (1-based, as registered by the last
// snapshot or read of the top page) and returns it with its offset in the
// top page's viewport.
func framePage(p *rod.Page, n int) (*rod.Page, [2]float64, error) {
	var off [2]float64
	pn := p.Sleeper(rod.NotFoundSleeper)
	el, err := pn.ElementByJS(rod.Eval(`(i) => (window.__okoFrames || [])[i] || null`, n-1))
	if isNotFound(err) {
		return nil, off, fmt.Errorf("frame f%d is not on the page any more; run 'oko snap' again", n)
	}
	if err != nil {
		return nil, off, err
	}
	res, err := el.Eval(`function () { const r = this.getBoundingClientRect(); return [r.left + this.clientLeft, r.top + this.clientTop] }`)
	if err == nil {
		a := res.Value.Arr()
		off = [2]float64{a[0].Num(), a[1].Num()}
	}
	node, err := el.Describe(1, false)
	if err != nil {
		return nil, off, err
	}
	var fp *rod.Page
	if node.ContentDocument != nil {
		// Same renderer process (cross-origin but same site): rod can
		// address the frame's document directly.
		fp, err = el.Frame()
	} else {
		// Out-of-process frame (a different site): it is its own target,
		// whose id is the frame id; attach to it.
		fp, err = p.Browser().PageFromTarget(proto.TargetTargetID(node.FrameID))
	}
	if err != nil {
		return nil, off, fmt.Errorf("attach to frame f%d: %w", n, err)
	}
	return fp, off, nil
}

// frameOffsets remembers, for elements living in a cross-origin frame, where
// that frame sits in the top viewport — needed to move the real mouse there.
var frameOffsets = map[*rod.Element][2]float64{}

// frameSnapshots snapshots every cross-origin frame of p.
func frameSnapshots(p *rod.Page, o snapOpts) ([]frameInfo, []*snapResult) {
	fs, err := crossFrames(p)
	if err != nil || len(fs) == 0 {
		return nil, nil
	}
	prof, _ := chrome.Load(profileName)
	var out []*snapResult
	for i := range fs {
		fp, _, err := framePage(p, i+1)
		if err != nil {
			out = append(out, nil)
			continue
		}
		key := string(p.TargetID) + "#f" + strconv.Itoa(i+1)
		base := 0
		if prof != nil {
			base = prof.State.Seq[key]
		}
		raw, err := evalJSON(fp, chrome.SnapshotJS, map[string]interface{}{
			"text": o.text, "seqBase": base, "prefix": "f" + strconv.Itoa(i+1),
		})
		if err != nil {
			out = append(out, nil)
			continue
		}
		var r snapResult
		if json.Unmarshal(raw, &r) != nil {
			out = append(out, nil)
			continue
		}
		if prof != nil && r.Seq != base {
			if prof.State.Seq == nil {
				prof.State.Seq = map[string]int{}
			}
			prof.State.Seq[key] = r.Seq
			_ = prof.Save()
		}
		out = append(out, &r)
	}
	return fs, out
}
