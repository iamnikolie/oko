package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPerf(t *testing.T) {
	oko(t, "open", base+"/feed.html")
	must(t, oko(t, "perf"), "TTFB", "LCP", "CLS", "TBT", "requests", "DOM nodes")

	dir := t.TempDir()
	trace := filepath.Join(dir, "t.json")
	must(t, oko(t, "perf", "trace", "--duration", "1s", "-o", trace), "main thread busy")
	if st, err := os.Stat(trace); err != nil || st.Size() < 1000 {
		t.Fatalf("trace not written: %v", err)
	}
	heap := filepath.Join(dir, "h.heapsnapshot")
	must(t, oko(t, "perf", "heap", "-o", heap), "JS heap")
	if st, err := os.Stat(heap); err != nil || st.Size() < 10000 {
		t.Fatalf("heap snapshot not written: %v", err)
	}
	if os.Getenv("OKO_E2E_LIGHTHOUSE") != "" {
		must(t, oko(t, "perf", "lighthouse"), "Performance")
	}
}
