package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
)

type workspace struct {
	Hash   string
	Name   string
	Folder string
	DBPath string
}

// discoverWorkspaces finds all workspace directories under root that
// contain a state.vscdb file, and resolves the workspace folder path
// from workspace.json.
func discoverWorkspaces(root string) ([]workspace, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read workspace storage dir: %w", err)
	}
	var workspaces []workspace
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		hash := e.Name()
		dbPath := filepath.Join(root, hash, "state.vscdb")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		ws := workspace{
			Hash:   hash,
			DBPath: dbPath,
		}
		ws.Folder, ws.Name = readWorkspaceJSON(filepath.Join(root, hash, "workspace.json"))
		if ws.Name == "" {
			ws.Name = hash
		}
		workspaces = append(workspaces, ws)
	}
	sort.Slice(workspaces, func(i, j int) bool {
		return workspaces[i].Name < workspaces[j].Name
	})
	return workspaces, nil
}

func readWorkspaceJSON(path string) (folder, name string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var ws struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(data, &ws); err != nil {
		return "", ""
	}
	folder = ws.Folder
	if u, err := url.Parse(folder); err == nil && u.Scheme == "file" {
		folder = u.Path
	}
	name = filepath.Base(folder)
	return folder, name
}
