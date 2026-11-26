// Package files puts the files a task brings into its working folder.
//
// Runtimes read PDF, text and image files with their own tools. Office files
// are zip archives that a runtime's file tool cannot open and that reading
// would need a command for, so rbridge writes a text copy beside each Word
// file (<name>.docx.txt, with a --- Page N --- line before each page) and
// tells the runtime about both.
package files

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Prepared describes one file in the working folder.
type Prepared struct {
	Name string // the file's name in the folder
	Text string // name of the text copy rbridge wrote, if any
}

// Place copies each source file into dir, writes text copies for Word files,
// and returns what is there. Two files with the same name are refused.
func Place(dir string, sources []string) ([]Prepared, error) {
	var out []Prepared
	seen := map[string]bool{}
	for _, src := range sources {
		name := filepath.Base(src)
		if seen[name] {
			return nil, fmt.Errorf("two files are named %s", name)
		}
		seen[name] = true
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("file %s: %w", src, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", src)
		}
		dest := filepath.Join(dir, name)
		if err := copyFile(src, dest); err != nil {
			return nil, err
		}
		p := Prepared{Name: name}
		if strings.EqualFold(filepath.Ext(name), ".docx") {
			text, err := DocxText(dest)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			p.Text = name + ".txt"
			if err := os.WriteFile(filepath.Join(dir, p.Text), []byte(text), 0o600); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Note is the line a prompt gets so the runtime knows what it was given.
func Note(placed []Prepared) string {
	var parts []string
	for _, p := range placed {
		if p.Text != "" {
			parts = append(parts, fmt.Sprintf("%s (a Word file: read its text in %s, which marks each page with a --- Page N --- line)", p.Name, p.Text))
		} else {
			parts = append(parts, p.Name)
		}
	}
	return "Files for this task, in your working folder: " + strings.Join(parts, "; ") + "."
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// DocxText returns a Word document's text: paragraphs as lines, tables as
// | separated rows, and a --- Page N --- line before each page the document
// breaks to.
func DocxText(path string) (string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("not a readable Word document: %w", err)
	}
	defer z.Close()
	var doc *zip.File
	for _, f := range z.File {
		if f.Name == "word/document.xml" {
			doc = f
		}
	}
	if doc == nil {
		return "", fmt.Errorf("not a Word document (no word/document.xml)")
	}
	r, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()

	pages := [][]string{nil}
	newPage := func() {
		if len(pages[len(pages)-1]) > 0 {
			pages = append(pages, nil)
		}
	}
	add := func(line string) {
		if strings.TrimSpace(line) != "" {
			pages[len(pages)-1] = append(pages[len(pages)-1], line)
		}
	}

	dec := xml.NewDecoder(io.LimitReader(r, 64<<20))
	var (
		depth      int // table nesting
		para       strings.Builder
		inPara     bool
		breakAfter bool
		cell       strings.Builder
		row        []string
		inText     bool
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading the document: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tbl":
				depth++
			case "tr":
				row = nil
			case "tc":
				cell.Reset()
			case "p":
				inPara, breakAfter = true, false
				para.Reset()
			case "pageBreakBefore":
				if depth == 0 {
					newPage()
				}
			case "br":
				page := false
				for _, a := range t.Attr {
					if a.Name.Local == "type" && a.Value == "page" {
						page = true
					}
				}
				if page {
					breakAfter = true
				} else if inPara {
					para.WriteString("\n")
				}
			case "tab":
				if inPara {
					para.WriteString("\t")
				}
			case "t":
				inText = true
			}
		case xml.CharData:
			if inText && inPara {
				para.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				inPara = false
				if depth > 0 {
					if cell.Len() > 0 {
						cell.WriteString(" ")
					}
					cell.WriteString(para.String())
				} else {
					add(strings.TrimSpace(para.String()))
					if breakAfter {
						newPage()
					}
				}
			case "tc":
				row = append(row, strings.Join(strings.Fields(cell.String()), " "))
			case "tr":
				if depth == 1 && strings.Join(row, "") != "" {
					add("| " + strings.Join(row, " | ") + " |")
				}
			case "tbl":
				depth--
			}
		}
	}
	var b strings.Builder
	n := 0
	for _, p := range pages {
		if len(p) == 0 {
			continue
		}
		n++
		fmt.Fprintf(&b, "--- Page %d ---\n%s\n\n", n, strings.Join(p, "\n"))
	}
	if n == 0 {
		return "", fmt.Errorf("the Word document has no text")
	}
	return b.String(), nil
}
