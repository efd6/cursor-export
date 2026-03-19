package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// composerMeta is the per-composer metadata stored in the workspace
// database's allComposers array.
type composerMeta struct {
	ComposerID  string `json:"composerId"`
	Name        string `json:"name,omitempty"`
	CreatedAt   int64  `json:"createdAt,omitempty"`
	UpdatedAt   int64  `json:"lastUpdatedAt,omitempty"`
	UnifiedMode string `json:"unifiedMode,omitempty"`
	Subtitle    string `json:"subtitle,omitempty"`
	IsArchived  bool   `json:"isArchived,omitempty"`
}

// composerList reads the composer.composerData value from a workspace
// database's ItemTable and returns the allComposers array.
func composerList(db *sql.DB) ([]composerMeta, error) {
	var raw []byte
	err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'composer.composerData'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying composer list: %w", err)
	}
	var data struct {
		AllComposers []composerMeta `json:"allComposers"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("unmarshal composerData: %w", err)
	}
	return data.AllComposers, nil
}

// bubbleHeader is a reference to a bubble in conversation order.
type bubbleHeader struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"` // 1 = user, 2 = assistant
}

// composerConversationOrder reads the ordered bubble list for a composer
// from the global database's cursorDiskKV table.
func composerConversationOrder(db *sql.DB, composerID string) ([]bubbleHeader, error) {
	key := "composerData:" + composerID
	var raw []byte
	err := db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying conversation order for %s: %w", composerID, err)
	}
	var data struct {
		Headers []bubbleHeader `json:"fullConversationHeadersOnly"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("unmarshal composerData:%s: %w", composerID, err)
	}
	return data.Headers, nil
}

// bubble is the content of a single message.
type bubble struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"`
	Text     string `json:"text"`
}

// readBubble reads a single bubble's content from the global database.
func readBubble(db *sql.DB, composerID, bubbleID string) (*bubble, error) {
	key := fmt.Sprintf("bubbleId:%s:%s", composerID, bubbleID)
	var raw []byte
	err := db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying bubble %s: %w", key, err)
	}
	var b bubble
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("unmarshal bubble %s: %w", key, err)
	}
	return &b, nil
}
