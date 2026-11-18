package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/codexrelay"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
)

const codexRelayKey = "codex.relay"

// codexRelayPending remembers the local endpoint magpie removed, apart
// from its current wiring. Processes from before that removal may still
// hold it after a Disconnect, a quit's restore, or another magpie start.
type codexRelayPending struct {
	Addr   string `json:"addr"`
	Before int64  `json:"before"`
}

func readCodexRelay(m map[string]string) (codexRelayPending, error) {
	var pending codexRelayPending
	if raw := m[codexRelayKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &pending); err != nil {
			return pending, fmt.Errorf("read Codex relay recovery: %w", err)
		}
		if pending.Addr == "" || pending.Before <= 0 {
			return pending, fmt.Errorf("read Codex relay recovery: missing endpoint or removal time")
		}
	}
	return pending, nil
}

// codexRelayAddress is the gateway this process can leave a relay at:
// loopback only, as the relay's own request guard requires.
func codexRelayAddress() (string, error) {
	u, err := url.Parse(gateway.URL())
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(u.Hostname()); !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", nil
	}
	return u.Host, nil
}

// rememberCodexRelay runs only after a successful removal. A distro or a
// config kept on a remote/previous gateway is no listener to take here.
// The relay has one port and one handover record, so a later removal of
// this gateway deliberately supersedes a record of another endpoint.
func rememberCodexRelay(at place, base string) error {
	if at.spell != nil || strings.TrimSuffix(base, "/") != gateway.URL()+gateway.CodexPath {
		return nil
	}
	addr, err := codexRelayAddress()
	if err != nil || addr == "" {
		return err
	}
	stashMu.Lock()
	defer stashMu.Unlock()
	m, err := stashLoadChecked()
	if err != nil {
		return err
	}
	pending, err := readCodexRelay(m)
	if err != nil {
		return err
	}
	before := time.Now().UnixNano()
	if pending.Addr == addr && pending.Before >= before {
		return nil
	}
	b, err := json.Marshal(codexRelayPending{Addr: addr, Before: before})
	if err != nil {
		return err
	}
	m[codexRelayKey] = string(b)
	return stashWrite(m)
}

// RelayOpenCodex leaves the existing relay for processes that may still
// hold an endpoint magpie removed. It is called after the quit's restore,
// even when failover ended at an earlier Disconnect. The record survives
// magpie restarting; a known list of only newer processes retires it.
func RelayOpenCodex() error {
	addr, err := codexRelayAddress()
	if err != nil || addr == "" {
		return err
	}
	stashMu.Lock()
	m, err := stashLoadChecked()
	stashMu.Unlock()
	if err != nil {
		return err
	}
	pending, err := readCodexRelay(m)
	if err != nil || pending.Addr != addr {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pids, err := codexPIDs(ctx)
	if err != nil {
		return fmt.Errorf("list Codex processes for relay recovery: %w", err)
	}
	var old []int
	for _, pid := range pids {
		since, err := codexProcessStarted(pid)
		if err != nil || !since.After(time.Unix(0, pending.Before)) {
			old = append(old, pid)
		}
	}
	if len(old) > 0 {
		return startRelay(addr, old)
	}
	// A removal while the processes were listed must not be forgotten.
	stashMu.Lock()
	defer stashMu.Unlock()
	now, err := stashLoadChecked()
	if err != nil {
		return err
	}
	if now[codexRelayKey] != m[codexRelayKey] {
		return nil
	}
	delete(now, codexRelayKey)
	return stashWrite(now)
}

// Hooks keep tests away from the real Codex processes and relay.
var (
	startRelay          = codexrelay.Start
	codexPIDs           = provider.CodexPIDs
	codexProcessStarted = proc.Started
)
