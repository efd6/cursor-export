// Command cursor-export reads Cursor chat history from its SQLite
// databases and writes it as JSON or Markdown.
package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
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

	"github.com/efd6/cursor-export/agentpb"
	"google.golang.org/protobuf/proto"
)

func main() {
	log.SetFlags(0)

	cursorDir := flag.String("cursor-dir", defaultCursorDir(), "path to Cursor's User directory (contains workspaceStorage/ and globalStorage/)")
	ws := flag.String("workspace", "", "export only this workspace (matched against name or folder path)")
	chatFilter := flag.String("chat", "", "export only chats whose name contains this string (case-insensitive)")
	list := flag.Bool("list", false, "list all chats and exit")
	extra := flag.Bool("extra", false, "include thinking blocks, tool calls, and model info")
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
		switch *format {
		case "json":
			err = listChatsJSON(workspaces, *ws, *chatFilter)
			if err != nil {
				log.Fatal(err)
			}
		default:
			listChats(workspaces, *ws, *chatFilter)
		}
		return
	}

	gdb, err := openDB(globalDB)
	if err != nil {
		log.Fatalf("opening global database: %v", err)
	}
	defer gdb.Close()

	var exports []workspaceExport
	for _, w := range workspaces {
		if *ws != "" && !matchWorkspace(w, *ws) {
			continue
		}
		export, err := exportWorkspace(w, gdb, *chatFilter, *extra)
		if err != nil {
			log.Printf("warning: workspace %s (%s): %v", w.Name, w.Hash, err)
			continue
		}
		if len(export.Chats) > 0 {
			exports = append(exports, export)
		}
	}

	if *format == "md" && len(exports) == 0 {
		return
	}

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
				created = time.UnixMilli(c.CreatedAt).Format(time.RFC3339)
			}
			mode := c.UnifiedMode
			if mode == "" {
				mode = "-"
			}
			lines = append(lines, fmt.Sprintf("  %s\t%s\t%s\t%s\n", name, mode, c.ComposerID, created))
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

func listChatsJSON(workspaces []workspace, wsFilter, chatFilter string) error {
	type chat struct {
		Workspace string `json:"workspace"`
		composerMeta
	}
	var chats []chat
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
		for _, c := range composers {
			if !matchChat(c, chatFilter) {
				continue
			}
			chats = append(chats, chat{Workspace: ws.Name, composerMeta: c})
		}
	}
	b, err := json.Marshal(chats)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", b)
	return nil
}

type message struct {
	Role      string       `json:"role"`
	CreatedAt time.Time    `json:"createdAt"`
	Text      string       `json:"text,omitempty"`
	Model     string       `json:"model,omitempty"`
	Thinking  string       `json:"thinking,omitempty"`
	ToolCall  *toolCallMsg `json:"toolCall,omitempty"`
}

type toolCallMsg struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`

	// Structured fields from protobuf decoding.
	Input  any `json:"input,omitempty"`
	Output any `json:"output,omitempty"`

	// Fallback: raw JSON strings when proto decoding fails or
	// the tool type is not covered by our proto definitions.
	RawParams string `json:"rawParams,omitempty"`
	RawResult string `json:"rawResult,omitempty"`
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
	pattern = strings.ToLower(pattern)
	return strings.Contains(strings.ToLower(name), pattern) || strings.Contains(strings.ToLower(c.ComposerID), pattern)
}

func exportWorkspace(ws workspace, gdb *sql.DB, chatFilter string, extra bool) (workspaceExport, error) {
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
			if b == nil {
				continue
			}

			msg, ok := bubbleToMessage(b, extra)
			if !ok {
				continue
			}
			ch.Messages = append(ch.Messages, msg)
		}

		if len(ch.Messages) > 0 {
			export.Chats = append(export.Chats, ch)
		}
	}

	return export, nil
}

func bubbleToMessage(b *bubble, extra bool) (message, bool) {
	role := "assistant"
	if b.Type == 1 {
		role = "user"
	}
	ts, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)

	switch {
	case b.CapabilityType == 15 && b.ToolFormerData != nil && extra:
		tc := &toolCallMsg{
			Name:   b.ToolFormerData.Name,
			Status: b.ToolFormerData.Status,
		}
		if !decodeToolCallBinary(b.ToolFormerData.ToolCallBinary, tc) {
			tc.RawParams = b.ToolFormerData.Params
			tc.RawResult = b.ToolFormerData.Result
		}
		return message{Role: "tool", CreatedAt: ts, ToolCall: tc}, true

	case b.CapabilityType == 30 && b.Thinking != nil && extra:
		msg := message{
			Role:      role,
			CreatedAt: ts,
			Thinking:  b.Thinking.Text,
		}
		return msg, msg.Thinking != ""

	default:
		if b.Text == "" {
			return message{}, false
		}
		msg := message{
			Role:      role,
			CreatedAt: ts,
			Text:      b.Text,
		}
		if extra && b.ModelInfo != nil {
			msg.Model = b.ModelInfo.ModelName
		}
		if extra && b.Thinking != nil && b.Thinking.Text != "" {
			msg.Thinking = b.Thinking.Text
		}
		return msg, true
	}
}

// decodeToolCallBinary decodes a base64-encoded agent.v1.ToolCall
// protobuf into structured input/output fields on tc. Returns true
// if decoding succeeded and populated at least one field.
func decodeToolCallBinary(b64 string, tc *toolCallMsg) bool {
	if b64 == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false
	}
	var pb agentpb.ToolCall
	if err := proto.Unmarshal(raw, &pb); err != nil {
		return false
	}
	switch t := pb.Tool.(type) {
	case *agentpb.ToolCall_ShellToolCall:
		return decodeShell(t.ShellToolCall, tc)
	case *agentpb.ToolCall_ReadToolCall:
		return decodeRead(t.ReadToolCall, tc)
	case *agentpb.ToolCall_EditToolCall:
		return decodeEdit(t.EditToolCall, tc)
	case *agentpb.ToolCall_GlobToolCall:
		return decodeGlob(t.GlobToolCall, tc)
	case *agentpb.ToolCall_GrepToolCall:
		return decodeGrep(t.GrepToolCall, tc)
	case *agentpb.ToolCall_DeleteToolCall:
		return decodeDelete(t.DeleteToolCall, tc)
	case *agentpb.ToolCall_FetchToolCall:
		return decodeFetch(t.FetchToolCall, tc)
	case *agentpb.ToolCall_LsToolCall:
		return decodeLs(t.LsToolCall, tc)
	case *agentpb.ToolCall_TaskToolCall:
		return decodeTask(t.TaskToolCall, tc)
	default:
		return false
	}
}

type shellInput struct {
	Command      string `json:"command"`
	WorkingDir   string `json:"workingDirectory,omitempty"`
	Description  string `json:"description,omitempty"`
	TimeoutMs    int32  `json:"timeoutMs,omitempty"`
	IsBackground bool   `json:"isBackground,omitempty"`
}

type shellOutput struct {
	ExitCode        int32  `json:"exitCode"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	ExecutionTimeMs int32  `json:"executionTimeMs,omitempty"`
	Status          string `json:"status"`
}

func decodeShell(tc *agentpb.ShellToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = shellInput{
			Command:      a.Command,
			WorkingDir:   a.WorkingDirectory,
			Description:  tc.Description,
			TimeoutMs:    a.Timeout,
			IsBackground: a.IsBackground,
		}
	}
	if r := tc.Result; r != nil {
		so := shellOutput{}
		switch v := r.Result.(type) {
		case *agentpb.ShellResult_Success:
			so.Status = "success"
			so.ExitCode = v.Success.ExitCode
			so.Stdout = v.Success.Stdout
			so.Stderr = v.Success.Stderr
			so.ExecutionTimeMs = v.Success.ExecutionTime
			if so.Stdout == "" {
				so.Stdout = v.Success.InterleavedOutput
			}
		case *agentpb.ShellResult_Failure:
			so.Status = "failure"
			so.ExitCode = v.Failure.ExitCode
			so.Stdout = v.Failure.Stdout
			so.Stderr = v.Failure.Stderr
			so.ExecutionTimeMs = v.Failure.ExecutionTime
			if so.Stdout == "" {
				so.Stdout = v.Failure.InterleavedOutput
			}
		case *agentpb.ShellResult_Timeout:
			so.Status = "timeout"
		case *agentpb.ShellResult_Rejected:
			so.Status = "rejected"
		case *agentpb.ShellResult_SpawnError:
			so.Status = "spawn_error"
			so.Stderr = v.SpawnError.Error
		case *agentpb.ShellResult_PermissionDenied:
			so.Status = "permission_denied"
		}
		out.Output = so
	}
	return true
}

type readInput struct {
	Path      string `json:"path"`
	StartLine int32  `json:"startLine,omitempty"`
	EndLine   int32  `json:"endLine,omitempty"`
}

type readOutput struct {
	Content    string `json:"content,omitempty"`
	TotalLines uint32 `json:"totalLines,omitempty"`
	FileSize   uint32 `json:"fileSize,omitempty"`
	Error      string `json:"error,omitempty"`
}

func decodeRead(tc *agentpb.ReadToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = readInput{Path: a.FilePath, StartLine: a.StartLine, EndLine: a.EndLine}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.ReadToolResult_Success:
			ro := readOutput{TotalLines: v.Success.TotalLines, FileSize: v.Success.FileSize}
			if c, ok := v.Success.Output.(*agentpb.ReadToolSuccess_Content); ok {
				ro.Content = c.Content
			}
			out.Output = ro
		case *agentpb.ReadToolResult_Error:
			out.Output = readOutput{Error: v.Error.ErrorMessage}
		}
	}
	return true
}

type editInput struct {
	Path       string `json:"path"`
	OldString  string `json:"oldString,omitempty"`
	NewString  string `json:"newString,omitempty"`
	CreateFile bool   `json:"createFile,omitempty"`
	ReplaceAll bool   `json:"replaceAll,omitempty"`
}

type editOutput struct {
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

func decodeEdit(tc *agentpb.EditToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = editInput{
			Path: a.FilePath, OldString: a.OldString, NewString: a.NewString,
			CreateFile: a.CreateFile, ReplaceAll: a.ReplaceAll,
		}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.EditResult_Success:
			out.Output = editOutput{Path: v.Success.FilePath}
		case *agentpb.EditResult_Error:
			out.Output = editOutput{Error: v.Error.Message}
		case *agentpb.EditResult_FileNotFound:
			out.Output = editOutput{Error: "file not found: " + v.FileNotFound.Path}
		case *agentpb.EditResult_Rejected:
			out.Output = editOutput{Error: "rejected: " + v.Rejected.Reason}
		}
	}
	return true
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

type globOutput struct {
	Files      []string `json:"files,omitempty"`
	TotalFiles int32    `json:"totalFiles,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func decodeGlob(tc *agentpb.GlobToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = globInput{Pattern: a.Pattern, Path: a.Path}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.GlobToolResult_Success:
			out.Output = globOutput{Files: v.Success.Files, TotalFiles: v.Success.TotalFiles}
		case *agentpb.GlobToolResult_Error:
			out.Output = globOutput{Error: v.Error.Error}
		}
	}
	return true
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	OutputMode string `json:"outputMode,omitempty"`
}

type grepOutput struct {
	Truncated bool   `json:"truncated,omitempty"`
	Content   string `json:"content,omitempty"`
	Error     string `json:"error,omitempty"`
}

func decodeGrep(tc *agentpb.GrepToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = grepInput{Pattern: a.Pattern, Path: a.Path, Glob: a.Glob, OutputMode: a.OutputMode}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.GrepResult_Success:
			go_ := grepOutput{Truncated: v.Success.Truncated}
			if u := v.Success.UnionResult; u != nil {
				if c, ok := u.Result.(*agentpb.GrepUnionResult_Content); ok {
					var lines []string
					for _, m := range c.Content.Matches {
						lines = append(lines, fmt.Sprintf("%s:%d:%s", m.Path, m.LineNumber, m.Content))
					}
					go_.Content = strings.Join(lines, "\n")
				}
			}
			out.Output = go_
		case *agentpb.GrepResult_Error:
			out.Output = grepOutput{Error: v.Error.Error}
		}
	}
	return true
}

func decodeDelete(tc *agentpb.DeleteToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = map[string]string{"path": a.FilePath}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.DeleteResult_Success:
			out.Output = map[string]string{"path": v.Success.FilePath}
		case *agentpb.DeleteResult_Error:
			out.Output = map[string]string{"error": v.Error.Message}
		}
	}
	return true
}

func decodeFetch(tc *agentpb.FetchToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = map[string]string{"url": a.Url}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.FetchResult_Success:
			out.Output = map[string]string{"url": v.Success.Url, "content": v.Success.Content}
		case *agentpb.FetchResult_Error:
			out.Output = map[string]string{"error": v.Error.Message}
		}
	}
	return true
}

func decodeLs(tc *agentpb.LsToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = map[string]any{"path": a.Path, "maxDepth": a.MaxDepth}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.LsResult_Success:
			out.Output = map[string]string{"output": v.Success.Output}
		case *agentpb.LsResult_Error:
			out.Output = map[string]string{"error": v.Error.Error}
		}
	}
	return true
}

func decodeTask(tc *agentpb.TaskToolCall, out *toolCallMsg) bool {
	if tc == nil {
		return false
	}
	if a := tc.Args; a != nil {
		out.Input = map[string]string{"prompt": a.Prompt}
	}
	if r := tc.Result; r != nil {
		switch v := r.Result.(type) {
		case *agentpb.TaskResult_Success:
			out.Output = map[string]string{"result": v.Success.Result}
		case *agentpb.TaskResult_Error:
			out.Output = map[string]string{"error": v.Error.Message}
		}
	}
	return true
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
	var date time.Time
	for _, ws := range exports {
		for _, ch := range ws.Chats {
			if date.IsZero() || ch.CreatedAt.Before(date) {
				date = ch.CreatedAt
			}
		}
	}
	if !date.IsZero() {
		fmt.Fprintf(w, `---
date: %s
tags:
  - cursor_chat
---

`, date.Format(time.DateOnly))
	}

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
				writeMarkdownMessage(w, msg)
			}
			fmt.Fprintln(w, "---")
		}
	}
}

func writeMarkdownMessage(w io.Writer, msg message) {
	heading := msg.Role
	if msg.Model != "" {
		heading += " (" + msg.Model + ")"
	}
	if !msg.CreatedAt.IsZero() {
		heading += " — " + msg.CreatedAt.Format(time.RFC3339)
	}

	if msg.ToolCall != nil {
		toolHeading := "tool: " + msg.ToolCall.Name
		if !msg.CreatedAt.IsZero() {
			toolHeading += " — " + msg.CreatedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "### %s\n\n", toolHeading)
		writeToolCallMarkdown(w, msg.ToolCall)
		return
	}

	fmt.Fprintf(w, "### %s\n\n", heading)
	if msg.Thinking != "" {
		fmt.Fprintf(w, "<details><summary>thinking</summary>\n\n%s\n\n</details>\n\n", msg.Thinking)
	}
	if msg.Text != "" {
		fmt.Fprintf(w, "%s\n\n", msg.Text)
	}
}

func writeToolCallMarkdown(w io.Writer, tc *toolCallMsg) {
	switch in := tc.Input.(type) {
	case shellInput:
		fmt.Fprintf(w, "```bash\n%s\n```\n\n", in.Command)
		if in.WorkingDir != "" {
			fmt.Fprintf(w, "cwd: `%s`\n\n", in.WorkingDir)
		}
	case readInput:
		s := fmt.Sprintf("`%s`", in.Path)
		if in.StartLine > 0 || in.EndLine > 0 {
			s += fmt.Sprintf(" (lines %d–%d)", in.StartLine, in.EndLine)
		}
		fmt.Fprintf(w, "%s\n\n", s)
	case editInput:
		fmt.Fprintf(w, "`%s`\n\n", in.Path)
	case globInput:
		fmt.Fprintf(w, "pattern: `%s`", in.Pattern)
		if in.Path != "" {
			fmt.Fprintf(w, " in `%s`", in.Path)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w)
	case grepInput:
		fmt.Fprintf(w, "pattern: `%s`", in.Pattern)
		if in.Path != "" {
			fmt.Fprintf(w, " in `%s`", in.Path)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w)
	default:
		if tc.RawParams != "" {
			var buf bytes.Buffer
			if json.Indent(&buf, []byte(tc.RawParams), "", "    ") != nil {
				fmt.Fprintf(w, "**Params:**\n```json\n%s\n```\n\n", tc.RawParams)
			} else {
				fmt.Fprintf(w, "**Params:**\n```json\n%s\n```\n\n", &buf)
			}
		}
	}

	switch out := tc.Output.(type) {
	case shellOutput:
		if out.Stdout != "" {
			fmt.Fprintf(w, "```\n%s\n```\n\n", out.Stdout)
		} else if out.Stderr != "" {
			fmt.Fprintf(w, "stderr:\n```\n%s\n```\n\n", out.Stderr)
		}
		if out.Status != "success" && out.Status != "" {
			fmt.Fprintf(w, "exit %d (%s)\n\n", out.ExitCode, out.Status)
		}
	case readOutput:
		if out.Error != "" {
			fmt.Fprintf(w, "error: %s\n\n", out.Error)
		} else if out.Content != "" {
			content := out.Content
			if len(content) > 2000 {
				content = content[:2000] + "\n... (truncated)"
			}
			fmt.Fprintf(w, "```\n%s\n```\n\n", content)
		}
	case editOutput:
		if out.Error != "" {
			fmt.Fprintf(w, "error: %s\n\n", out.Error)
		}
	case globOutput:
		if out.Error != "" {
			fmt.Fprintf(w, "error: %s\n\n", out.Error)
		} else {
			for _, f := range out.Files {
				fmt.Fprintf(w, "- `%s`\n", f)
			}
			if out.TotalFiles > int32(len(out.Files)) {
				fmt.Fprintf(w, "- ... (%d total)\n", out.TotalFiles)
			}
			fmt.Fprintln(w)
		}
	case grepOutput:
		if out.Error != "" {
			fmt.Fprintf(w, "error: %s\n\n", out.Error)
		} else if out.Content != "" {
			fmt.Fprintf(w, "```\n%s\n```\n\n", out.Content)
		}
	default:
		if tc.RawResult != "" {
			var buf bytes.Buffer
			if json.Indent(&buf, []byte(tc.RawResult), "", "    ") != nil {
				fmt.Fprintf(w, "**Result:**\n```json\n%s\n```\n\n", tc.RawResult)
			} else {
				fmt.Fprintf(w, "**Result:**\n```json\n%s\n```\n\n", &buf)
			}
		}
	}
}
