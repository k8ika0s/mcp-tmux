package server

import (
	"context"
	"testing"
	"time"
)

type dummyReq struct {
	Target *dummyTarget
}

type dummyTarget struct {
	host    string
	session string
	window  string
	pane    string
}

func (d dummyReq) GetTarget() *dummyTarget { return d.Target }

func TestAuditLogNoPanic(t *testing.T) {
	cfg := auditConfig{color: false}
	cfg.log("/mcp/Stream", dummyReq{}, time.Now(), nil, false)
	cfg.log("/mcp/Stream", dummyReq{}, time.Now(), context.Canceled, true)
}
