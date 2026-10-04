package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

// Recording runs in a detached `oko _record` process: it keeps a CDP
// session on the tab, receives screencast frames (Chrome sends one whenever
// the page repaints), stores them as JPEGs with their timestamps, and on
// SIGTERM turns them into a GIF or MP4. It also injects a small overlay that
// draws the pointer and click ripples, so actions are visible in the video.

var (
	recOut   string
	recWidth int
	recFPS   int
)

// cursorJS draws the pointer and click ripples. Pages do not render the
// system cursor into screencast frames, so without it clicks are invisible.
const cursorJS = `(() => {
  if (window.__okoCursor) return;
  window.__okoCursor = true;
  const add = () => {
    const c = document.createElement('div');
    c.id = '__oko_cursor';
    c.style.cssText = 'position:fixed;left:-50px;top:-50px;width:18px;height:18px;border-radius:50%;' +
      'background:rgba(255,80,60,.85);border:2px solid #fff;box-shadow:0 0 6px rgba(0,0,0,.45);' +
      'z-index:2147483647;pointer-events:none;transform:translate(-50%,-50%);transition:transform .08s';
    document.documentElement.appendChild(c);
    addEventListener('mousemove', (e) => { c.style.left = e.clientX + 'px'; c.style.top = e.clientY + 'px'; }, true);
    addEventListener('mousedown', (e) => {
      c.style.transform = 'translate(-50%,-50%) scale(.7)';
      const r = document.createElement('div');
      r.style.cssText = 'position:fixed;left:' + e.clientX + 'px;top:' + e.clientY + 'px;width:16px;height:16px;' +
        'border-radius:50%;border:3px solid rgba(255,80,60,.9);z-index:2147483647;pointer-events:none;' +
        'transform:translate(-50%,-50%);transition:all .45s ease-out;opacity:1';
      document.documentElement.appendChild(r);
      requestAnimationFrame(() => { r.style.width = '56px'; r.style.height = '56px'; r.style.opacity = '0'; });
      setTimeout(() => r.remove(), 500);
    }, true);
    addEventListener('mouseup', () => { c.style.transform = 'translate(-50%,-50%)'; }, true);
  };
  if (document.documentElement) add(); else addEventListener('DOMContentLoaded', add);
})()`

func recordDir(prof *chrome.Profile, tab proto.TargetTargetID) string {
	return filepath.Join(prof.Dir, "recording-"+shortID(tab))
}

var recordCmd = &cobra.Command{
	Use:   "record <start|stop|status>",
	Short: "Record the current tab to a GIF (or MP4): start, act, stop",
	Long: "start launches a background recorder on the current tab; every oko command\n" +
		"you run meanwhile is captured, with the pointer and clicks drawn in. stop\n" +
		"writes the file (-o name.gif or name.mp4; default ~/.oko/recordings/<time>.gif)\n" +
		"and prints its path. Frames arrive only when the page repaints, so idle time\n" +
		"costs nothing.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "start":
			return run(recordStart)
		case "stop":
			return run(recordStop)
		case "status":
			return run(func(s *session) error {
				p, err := s.page()
				if err != nil {
					return err
				}
				pid := s.prof.State.Recorders[string(p.TargetID)]
				if pid == 0 || syscall.Kill(pid, 0) != nil {
					fmt.Fprintln(stdout, "not recording")
					return nil
				}
				n, _ := filepath.Glob(filepath.Join(recordDir(s.prof, p.TargetID), "*.jpg"))
				fmt.Fprintf(stdout, "recording tab %s (pid %d), %d frames so far\n", shortID(p.TargetID), pid, len(n))
				return nil
			})
		}
		return errors.New("use start, stop or status")
	},
}

func recordStart(s *session) error {
	p, err := s.page()
	if err != nil {
		return err
	}
	tab := string(p.TargetID)
	if pid := s.prof.State.Recorders[tab]; pid != 0 && syscall.Kill(pid, 0) == nil {
		return fmt.Errorf("already recording this tab (pid %d); 'oko record stop' first", pid)
	}
	dir := recordDir(s.prof, p.TargetID)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(self, "_record", "--profile", s.prof.Name, tab, dir, strconv.Itoa(recWidth))
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logf, _ := os.Create(filepath.Join(dir, "recorder.log"))
	c.Stdout, c.Stderr = logf, logf
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	// Wait for the recorder to report it is capturing.
	ready := filepath.Join(dir, "ready")
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		_ = syscall.Kill(c.Process.Pid, syscall.SIGTERM)
		return fmt.Errorf("recorder did not start; see %s", filepath.Join(dir, "recorder.log"))
	}
	if s.prof.State.Recorders == nil {
		s.prof.State.Recorders = map[string]int{}
	}
	s.prof.State.Recorders[tab] = c.Process.Pid
	_ = s.prof.Save()
	fmt.Fprintf(stdout, "recording tab %s — act with oko, then 'oko record stop -o demo.gif'\n", shortID(p.TargetID))
	return nil
}

func recordStop(s *session) error {
	p, err := s.page()
	if err != nil {
		return err
	}
	tab := string(p.TargetID)
	pid := s.prof.State.Recorders[tab]
	if pid == 0 {
		return errors.New("not recording this tab")
	}
	dir := recordDir(s.prof, p.TargetID)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	delete(s.prof.State.Recorders, tab)
	_ = s.prof.Save()

	out := recOut
	if out == "" {
		d := filepath.Join(chrome.Home(), "recordings")
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		out = filepath.Join(d, time.Now().Format("20060102-150405")+".gif")
	}
	out, _ = filepath.Abs(out)
	n, dur, err := encodeRecording(dir, out, recFPS)
	if err != nil {
		return err
	}
	st, _ := os.Stat(out)
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	fmt.Fprintf(stdout, "%s\n%d frames, %.1fs, %.1f MB\n", out, n, dur.Seconds(), float64(size)/1e6)
	_ = os.RemoveAll(dir)
	return nil
}

// recorderCmd is the background process behind 'record start'.
var recorderCmd = &cobra.Command{
	Use:    "_record <tab> <dir> <width>",
	Hidden: true,
	Args:   cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		tab, dir := args[0], args[1]
		width, _ := strconv.Atoi(args[2])
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := connect(ctx, false)
		if err != nil {
			return err
		}
		tabFlag = tab
		p, err := s.page()
		if err != nil {
			return err
		}
		// Pointer overlay: on this page now and on every page it navigates to.
		_, _ = proto.PageAddScriptToEvaluateOnNewDocument{Source: cursorJS}.Call(p)
		_, _ = proto.RuntimeEvaluate{Expression: cursorJS}.Call(p)

		events := s.b.Event()
		frames := 0
		done := make(chan struct{})
		go func() {
			defer close(done)
			for msg := range events {
				if msg.SessionID != p.SessionID {
					continue
				}
				switch msg.Method {
				case "Page.screencastFrame":
					var e proto.PageScreencastFrame
					if !msg.Load(&e) {
						continue
					}
					_ = proto.PageScreencastFrameAck{SessionID: e.SessionID}.Call(p)
					ts := time.Now()
					if e.Metadata != nil && !e.Metadata.Timestamp.Time().IsZero() {
						ts = e.Metadata.Timestamp.Time()
					}
					name := fmt.Sprintf("%020d.jpg", ts.UnixNano())
					_ = os.WriteFile(filepath.Join(dir, name), e.Data, 0o600)
					frames++
				case "Page.frameNavigated":
					// Some pages drop init scripts on bfcache restores; re-inject.
					_, _ = proto.RuntimeEvaluate{Expression: cursorJS}.Call(p)
				}
			}
		}()
		q := 80
		maxW := width * 2 // device pixels on HiDPI; encoder scales down
		if err := (proto.PageStartScreencast{Format: proto.PageStartScreencastFormatJpeg, Quality: &q, MaxWidth: &maxW}).Call(p); err != nil {
			return err
		}
		_ = os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600)

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-sig:
		case <-time.After(30 * time.Minute): // safety cap
		}
		_ = proto.PageStopScreencast{}.Call(p)
		time.Sleep(200 * time.Millisecond)
		// One last frame: the final state, even if nothing repainted.
		if img, err := p.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatJpeg, Quality: &q}); err == nil {
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d.jpg", time.Now().UnixNano())), img, 0o600)
		}
		return nil
	},
}

type frame struct {
	path string
	at   time.Time
}

func listFrames(dir string) ([]frame, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []frame
	for _, f := range files {
		ns, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(f), ".jpg"), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, frame{f, time.Unix(0, ns)})
	}
	if len(out) == 0 {
		return nil, errors.New("no frames were captured (was the tab visible and did anything change?)")
	}
	return out, nil
}

// frameDurations converts capture times into display durations: a frame is
// shown until the next one arrived; long idle stretches are shortened so the
// video stays watchable, and the last frame is held briefly.
func frameDurations(fr []frame) []time.Duration {
	d := make([]time.Duration, len(fr))
	for i := range fr {
		if i == len(fr)-1 {
			d[i] = 1500 * time.Millisecond
			continue
		}
		g := fr[i+1].at.Sub(fr[i].at)
		if g > 2*time.Second {
			g = 2 * time.Second
		}
		if g < 20*time.Millisecond {
			g = 20 * time.Millisecond
		}
		d[i] = g
	}
	return d
}

func encodeRecording(dir, out string, fps int) (int, time.Duration, error) {
	fr, err := listFrames(dir)
	if err != nil {
		return 0, 0, err
	}
	if err := normalizeFrames(fr); err != nil {
		return 0, 0, err
	}
	durs := frameDurations(fr)
	var total time.Duration
	for _, d := range durs {
		total += d
	}
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		return len(fr), total, encodeFFmpeg(dir, fr, durs, out, fps)
	}
	if strings.HasSuffix(strings.ToLower(out), ".mp4") {
		return 0, 0, errors.New("MP4 output needs ffmpeg")
	}
	return len(fr), total, encodeGoGIF(fr, durs, out)
}

func encodeFFmpeg(dir string, fr []frame, durs []time.Duration, out string, fps int) error {
	var list bytes.Buffer
	for i, f := range fr {
		fmt.Fprintf(&list, "file '%s'\nduration %.3f\n", f.path, durs[i].Seconds())
	}
	// concat demuxer needs the last file repeated for its duration to apply
	fmt.Fprintf(&list, "file '%s'\n", fr[len(fr)-1].path)
	listPath := filepath.Join(dir, "frames.txt")
	if err := os.WriteFile(listPath, list.Bytes(), 0o600); err != nil {
		return err
	}
	scale := fmt.Sprintf("fps=%d,scale=%d:-2:flags=lanczos", fps, recWidth)
	var args []string
	if strings.HasSuffix(strings.ToLower(out), ".mp4") {
		args = []string{"-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", listPath,
			"-vf", scale + ",format=yuv420p", "-c:v", "libx264", "-crf", "23", "-movflags", "+faststart", out}
	} else {
		args = []string{"-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", listPath,
			"-vf", scale + ",split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle",
			"-loop", "0", out}
	}
	c := exec.Command("ffmpeg", args...)
	var stderrBuf bytes.Buffer
	c.Stderr = &stderrBuf
	if err := c.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(stderrBuf.String()))
	}
	return nil
}

// normalizeFrames rescales any frame whose size differs from the first one
// (the final screenshot is captured at device resolution; the window may
// also be resized mid-recording). ffmpeg's palette filters reset on a size
// change and freeze the GIF on its first frame otherwise.
func normalizeFrames(fr []frame) error {
	read := func(p string) (image.Config, error) {
		f, err := os.Open(p)
		if err != nil {
			return image.Config{}, err
		}
		defer f.Close()
		return jpeg.DecodeConfig(f)
	}
	first, err := read(fr[0].path)
	if err != nil {
		return err
	}
	for _, f := range fr[1:] {
		c, err := read(f.path)
		if err != nil || (c.Width == first.Width && c.Height == first.Height) {
			continue
		}
		b, err := os.ReadFile(f.path)
		if err != nil {
			return err
		}
		src, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			continue
		}
		sb := src.Bounds()
		dst := image.NewRGBA(image.Rect(0, 0, first.Width, first.Height))
		for y := 0; y < first.Height; y++ {
			for x := 0; x < first.Width; x++ {
				dst.Set(x, y, src.At(sb.Min.X+x*sb.Dx()/first.Width, sb.Min.Y+y*sb.Dy()/first.Height))
			}
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
			return err
		}
		if err := os.WriteFile(f.path, buf.Bytes(), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// encodeGoGIF is the dependency-free fallback: Plan 9 palette with
// Floyd–Steinberg dithering, frames scaled to recWidth.
func encodeGoGIF(fr []frame, durs []time.Duration, out string) error {
	g := &gif.GIF{}
	for i, f := range fr {
		b, err := os.ReadFile(f.path)
		if err != nil {
			return err
		}
		src, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			continue
		}
		sb := src.Bounds()
		w := recWidth
		if sb.Dx() < w {
			w = sb.Dx()
		}
		h := sb.Dy() * w / sb.Dx()
		scaled := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				scaled.Set(x, y, src.At(sb.Min.X+x*sb.Dx()/w, sb.Min.Y+y*sb.Dy()/h))
			}
		}
		pal := image.NewPaletted(scaled.Bounds(), palette.Plan9)
		draw.FloydSteinberg.Draw(pal, pal.Bounds(), scaled, image.Point{})
		g.Image = append(g.Image, pal)
		g.Delay = append(g.Delay, int(durs[i]/(10*time.Millisecond)))
	}
	fo, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fo.Close()
	return gif.EncodeAll(fo, g)
}

func init() {
	recordCmd.Flags().StringVarP(&recOut, "out", "o", "", "output file, .gif or .mp4 (default ~/.oko/recordings/<time>.gif)")
	recordCmd.Flags().IntVar(&recWidth, "width", 960, "output width in pixels")
	recordCmd.Flags().IntVar(&recFPS, "fps", 12, "output frame rate (ffmpeg)")
	rootCmd.AddCommand(recordCmd, recorderCmd)
}
