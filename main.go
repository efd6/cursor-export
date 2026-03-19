// Command cursor-export reads Cursor chat history from its SQLite
// databases and writes it as JSON or Markdown.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

func main() {
	log.SetFlags(0)

	cursorDir := flag.String("cursor-dir", defaultCursorDir(), "path to Cursor's User directory (contains workspaceStorage/ and globalStorage/)")
	ws := flag.String("workspace", "", "export only this workspace (matched against name or folder path)")
	chatFilter := flag.String("chat", "", "export only chats whose name contains this string (case-insensitive)")
	list := flag.Bool("list", false, "list all chats and exit")
	format := flag.String("format", "json", "output format: json or md")
	outFile := flag.String("o", "", "output file (default: stdout)")
	flag.Parse()

	wsRoot := filepath.Join(*cursorDir, "workspaceStorage")
	globalDB := filepath.Join(*cursorDir, "globalStorage", "state.vscdb")

	workspaces, err := discoverWorkspaces(wsRoot)
	if err != nil {
		log.Fatalf("discovering workspaces: %v", err)
	}
	if len(workspaces) == 0 {
		log.Fatal("no workspaces found")
	}

	if *list {
		listChats(workspaces, *ws, *chatFilter)
		return
	}

	gdb, err := openDB(globalDB)
	if err != nil {
		log.Fatalf("opening global database: %v", err)
	}
	defer gdb.Close()

	var out io.Writer = os.Stdout
	if *outFile != "" {
		f, err := os.Create(*outFile)
		if err != nil {
			log.Fatalf("creating output file: %v", err)
		}
		defer func() {
			if err := f.Close(); err != nil {
				log.Fatalf("closing output file: %v", err)
			}
		}()
		out = f
	}

	var exports []workspaceExport
	for _, w := range workspaces {
		if *ws != "" && !matchWorkspace(w, *ws) {
			continue
		}
		export, err := exportWorkspace(w, gdb, *chatFilter)
		if err != nil {
			log.Printf("warning: workspace %s (%s): %v", w.Name, w.Hash, err)
			continue
		}
		if len(export.Chats) > 0 {
			exports = append(exports, export)
		}
	}

	switch *format {
	case "json":
		writeJSON(out, exports)
	case "md":
		writeMarkdown(out, exports)
	default:
		log.Fatalf("unknown format %q", *format)
	}
}

func defaultCursorDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "Cursor", "User")
}

func listChats(workspaces []workspace, wsFilter, chatFilter string) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, ws := range workspaces {
		if wsFilter != "" && !matchWorkspace(ws, wsFilter) {
			continue
		}
		wdb, err := openDB(ws.DBPath)
		if err != nil {
			log.Printf("warning: workspace %s: %v", ws.Name, err)
			continue
		}
		composers, err := composerList(wdb)
		wdb.Close()
		if err != nil {
			log.Printf("warning: workspace %s: %v", ws.Name, err)
			continue
		}
		if len(composers) == 0 {
			continue
		}
		var lines []string
		for _, c := range composers {
			name := c.Name
			if name == "" {
				name = c.ComposerID
			}
			if !matchChat(c, chatFilter) {
				continue
			}
			created := ""
			if c.CreatedAt > 0 {
				created = time.UnixMilli(c.CreatedAt).Format("2006-01-02")
			}
			mode := c.UnifiedMode
			if mode == "" {
				mode = "-"
			}
			lines = append(lines, fmt.Sprintf("  %s\t%s\t%s\n", name, mode, created))
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(tw, "%s\t(%d chats)\n", ws.Name, len(lines))
		for _, line := range lines {
			fmt.Fprint(tw, line)
		}
	}
	tw.Flush()
}

type message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type chat struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Messages  []message `json:"messages"`
}

type workspaceExport struct {
	Name   string `json:"name"`
	Folder string `json:"folder,omitempty"`
	Chats  []chat `json:"chats"`
}

// matchWorkspace reports whether ws matches pattern. Name is compared
// exactly (case-insensitive); folder is substring-matched so that
// partial paths like "elastic/integrations" work.
func matchWorkspace(ws workspace, pattern string) bool {
	p := strings.ToLower(pattern)
	return strings.ToLower(ws.Name) == p ||
		strings.Contains(strings.ToLower(ws.Folder), p)
}

// matchChat reports whether c matches pattern by case-insensitive
// substring on the chat name.
func matchChat(c composerMeta, pattern string) bool {
	if pattern == "" {
		return true
	}
	name := c.Name
	if name == "" {
		name = c.ComposerID
	}
	return strings.Contains(strings.ToLower(name), strings.ToLower(pattern))
}

func exportWorkspace(ws workspace, gdb *sql.DB, chatFilter string) (workspaceExport, error) {
	wdb, err := openDB(ws.DBPath)
	if err != nil {
		return workspaceExport{}, err
	}
	defer wdb.Close()

	composers, err := composerList(wdb)
	if err != nil {
		return workspaceExport{}, fmt.Errorf("reading composer list: %w", err)
	}

	export := workspaceExport{
		Name:   ws.Name,
		Folder: ws.Folder,
	}

	for _, c := range composers {
		if !matchChat(c, chatFilter) {
			continue
		}
		ch := chat{
			ID:   c.ComposerID,
			Name: c.Name,
			Mode: c.UnifiedMode,
		}
		if c.CreatedAt > 0 {
			ch.CreatedAt = time.UnixMilli(c.CreatedAt)
		}

		headers, err := composerConversationOrder(gdb, c.ComposerID)
		if err != nil {
			log.Printf("warning: composer %s: conversation order: %v", c.ComposerID, err)
			continue
		}

		for _, h := range headers {
			b, err := readBubble(gdb, c.ComposerID, h.BubbleID)
			if err != nil {
				log.Printf("warning: bubble %s/%s: %v", c.ComposerID, h.BubbleID, err)
				continue
			}
			if b == nil || b.Text == "" {
				continue
			}
			role := "assistant"
			if b.Type == 1 {
				role = "user"
			}
			ch.Messages = append(ch.Messages, message{
				Role: role,
				Text: b.Text,
			})
		}

		if len(ch.Messages) > 0 {
			export.Chats = append(export.Chats, ch)
		}
	}

	return export, nil
}

func writeJSON(w io.Writer, exports []workspaceExport) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(exports); err != nil {
		log.Fatalf("writing JSON: %v", err)
	}
}

func writeMarkdown(w io.Writer, exports []workspaceExport) {
	for _, ws := range exports {
		fmt.Fprintf(w, "# %s\n\n", ws.Name)
		if ws.Folder != "" {
			fmt.Fprintf(w, "Workspace: `%s`\n\n", ws.Folder)
		}
		for _, ch := range ws.Chats {
			title := ch.Name
			if title == "" {
				title = ch.ID
			}
			fmt.Fprintf(w, "## %s\n\n", title)
			fmt.Fprintf(w, "- Mode: %s\n", ch.Mode)
			fmt.Fprintf(w, "- Created: %s\n\n", ch.CreatedAt.Format(time.RFC3339))
			for _, msg := range ch.Messages {
				fmt.Fprintf(w, "### %s\n\n%s\n\n", msg.Role, msg.Text)
			}
			fmt.Fprintln(w, "---")
		}
	}
}
