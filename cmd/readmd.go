package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

var (
	readFull   bool
	readMax    int
	readRows   int
	readLinks  string
	readImages bool
)

type readResult struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Source   string `json:"source"`
	Markdown string `json:"markdown"`
}

var readCmd = &cobra.Command{
	Use:   "read [target]",
	Short: "The page's content as clean markdown: main content only, no nav/controls",
	Long: "Finds the main content (main/article, else the densest text block) and\n" +
		"renders it as markdown: headings, paragraphs, lists, links, code, tables\n" +
		"(including ARIA grids), field values. Site chrome, buttons, floating layers\n" +
		"and hidden text are dropped. --full renders the whole page; a target renders\n" +
		"one element. Deterministic, no model.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			o := map[string]interface{}{"full": readFull, "rows": readRows, "links": readLinks, "images": readImages}
			var res *proto.RuntimeRemoteObject
			if len(args) == 1 {
				el, err := element(p, args[0])
				if err != nil {
					return err
				}
				if res, err = el.Eval(chrome.ReadJS, o); err != nil {
					return err
				}
			} else if res, err = p.Eval(chrome.ReadJS, o); err != nil {
				return err
			}
			raw, _ := json.Marshal(res.Value)
			var r readResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			md := r.Markdown
			if len(args) == 0 {
				// Cross-origin frames: read each one and append it.
				if fs, err := crossFrames(p); err == nil {
					for i, f := range fs {
						fp, _, err := framePage(p, i+1)
						if err != nil {
							continue
						}
						fr, err := fp.Eval(chrome.ReadJS, o)
						if err != nil {
							continue
						}
						var fm readResult
						b, _ := json.Marshal(fr.Value)
						if json.Unmarshal(b, &fm) == nil && strings.TrimSpace(fm.Markdown) != "" {
							title := f.Title
							if title == "" {
								title = f.Origin
							}
							md += fmt.Sprintf("\n\n## Frame f%d: %s\n\n%s", i+1, title, fm.Markdown)
						}
					}
				}
			}
			cut := 0
			if readMax > 0 && len([]rune(md)) > readMax {
				rs := []rune(md)
				cut = len(rs) - readMax
				md = string(rs[:readMax])
			}
			if jsonOutput {
				r.Markdown = md
				return printJSON(map[string]interface{}{"page": r, "truncated_chars": cut})
			}
			fmt.Fprintf(stdout, "<!-- %s | %s | %s -->\n\n%s\n", r.Title, r.URL, r.Source, md)
			if cut > 0 {
				fmt.Fprintf(stdout, "\n… %d more chars (--max 0 for all, or 'oko read <target>' for one part)\n", cut)
			}
			return nil
		})
	},
}

func init() {
	readCmd.Flags().BoolVar(&readFull, "full", false, "whole page, including nav, header, footer and sidebars")
	readCmd.Flags().IntVar(&readMax, "max", 20000, "character limit (0 = no limit)")
	readCmd.Flags().IntVar(&readRows, "rows", 50, "rows per table (0 = all)")
	readCmd.Flags().StringVar(&readLinks, "links", "smart", "smart (external + title-like links), all, or none")
	readCmd.Flags().BoolVar(&readImages, "images", false, "include images with alt text as ![alt](src)")
	rootCmd.AddCommand(readCmd)
}
