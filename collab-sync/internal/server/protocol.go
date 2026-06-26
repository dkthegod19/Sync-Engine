package server

import "collabsync/internal/crdt"

// Cursor is a client's selection in visible-position coordinates.
type Cursor struct {
	Pos    int `json:"pos"`
	Anchor int `json:"anchor"`
}

// UserInfo is the presence record for one connected client.
type UserInfo struct {
	SiteID string  `json:"siteId"`
	Name   string  `json:"name"`
	Color  string  `json:"color"`
	Online bool    `json:"online"`
	Cursor *Cursor `json:"cursor,omitempty"`
}

// SeqOp is an operation stamped with the server's global sequence number and a
// wall-clock timestamp. The op log is the document's full edit history.
type SeqOp struct {
	Seq  uint64   `json:"seq"`
	Op   crdt.Op  `json:"op"`
	Time int64    `json:"time"` // unix millis
}

// ClientMsg is any message a browser sends to the hub.
type ClientMsg struct {
	Type    string   `json:"type"`           // hello | op | presence | preview | revert
	SiteID  string   `json:"siteId,omitempty"`
	Name    string   `json:"name,omitempty"`
	Color   string   `json:"color,omitempty"`
	Since   uint64   `json:"since,omitempty"` // hello: last serverSeq the client has
	Op      *crdt.Op `json:"op,omitempty"`
	Cursor  *Cursor  `json:"cursor,omitempty"`
	Version uint64   `json:"version,omitempty"` // preview/revert target
}

// ServerMsg is any message the hub sends to a browser.
type ServerMsg struct {
	Type      string     `json:"type"` // welcome | op | presence | users | preview | reset
	ServerSeq uint64     `json:"serverSeq,omitempty"`
	Ops       []SeqOp    `json:"ops,omitempty"` // welcome snapshot / reset replay
	Seq       uint64     `json:"seq,omitempty"` // single broadcast op
	Op        *crdt.Op   `json:"op,omitempty"`
	Time      int64      `json:"time,omitempty"`
	Users     []UserInfo `json:"users,omitempty"`
	User      *UserInfo  `json:"user,omitempty"`
	Version   uint64     `json:"version,omitempty"`
	Text      string     `json:"text,omitempty"`
	You       *UserInfo  `json:"you,omitempty"`
}
