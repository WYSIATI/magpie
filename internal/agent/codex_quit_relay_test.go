package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

type relayQuitCall struct {
	addr string
	pids []int
}

type relayQuitProcesses struct {
	running  []int
	listed   int
	listErr  error
	starts   map[int]time.Time
	startErr error
	relays   []relayQuitCall
}

func fakeRelayQuitProcesses(t *testing.T) *relayQuitProcesses {
	t.Helper()
	wasStart, wasPIDs, wasStarted := startRelay, codexPIDs, codexProcessStarted
	t.Cleanup(func() { startRelay, codexPIDs, codexProcessStarted = wasStart, wasPIDs, wasStarted })
	p := &relayQuitProcesses{running: []int{41}, starts: map[int]time.Time{41: time.Now().Add(-time.Hour)}}
	startRelay = func(addr string, pids []int) error {
		p.relays = append(p.relays, relayQuitCall{addr, slices.Clone(pids)})
		return nil
	}
	codexPIDs = func(context.Context) ([]int, error) {
		p.listed++
		return p.running, p.listErr
	}
	codexProcessStarted = func(pid int) (time.Time, error) { return p.starts[pid], p.startErr }
	return p
}

func connectedNativeCodexForRelay(t *testing.T) (*Agent, func() string) {
	t.Helper()
	// The reported path: the Agents switch joins Codex's own ChatGPT
	// model, then the native gpt-6.1-sol is picked, before Disconnect.
	home, read := codexHome(t, `{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`, "model = \"gpt-6.1-sol\"\n")
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3499")
	cx := codex(home)
	if _, err := cx.ConnectHow(); err != nil {
		t.Fatal(err)
	}
	if err := cx.Apply("model", "gpt-6.1-sol"); err != nil {
		t.Fatal(err)
	}
	if !cx.Wired() || !strings.Contains(read(), "openai_base_url") {
		t.Fatalf("native Codex was not connected:\n%s", read())
	}
	return cx, read
}

func relayPendingForTest(t *testing.T) codexRelayPending {
	t.Helper()
	pending, err := readCodexRelay(stashLoad())
	if err != nil || pending.Addr == "" {
		t.Fatalf("no pending Codex endpoint: %+v, %v", pending, err)
	}
	return pending
}

func TestCodexDisconnectLeavesRelayForOldProcesses(t *testing.T) {
	cx, read := connectedNativeCodexForRelay(t)
	p := fakeRelayQuitProcesses(t)
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if cx.Wired() || strings.Contains(read(), "openai_base_url") {
		t.Fatalf("Disconnect left Codex routed:\n%s", read())
	}
	if ended, err := EndCodexFailover(); err != nil || ended {
		t.Fatalf("Disconnect left legacy failover on: %v, %v", ended, err)
	}
	before := stashLoad()[codexRelayKey]
	// Nothing to remove a second time: it must not extend which processes
	// are thought to hold the old endpoint.
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if got := stashLoad()[codexRelayKey]; got != before {
		t.Fatalf("a repeated Disconnect moved recovery from %s to %s", before, got)
	}
	// Recovery is on disk, not in this Agent or the old process's memo.
	filememo.DropKind("stash")
	reopened := codex(filepath.Dir(filepath.Dir(cx.Path)))
	if reopened.Wired() || !reopened.Disconnected() {
		t.Fatal("reconstructed Codex lost its disconnected state")
	}
	if err := RelayOpenCodex(); err != nil {
		t.Fatal(err)
	}
	if len(p.relays) != 1 || p.relays[0].addr != "127.0.0.1:3499" || !slices.Equal(p.relays[0].pids, []int{41}) {
		t.Fatalf("restored config left the old Codex with no relay: %+v", p.relays)
	}
	if stashLoad()[codexRelayKey] == "" {
		t.Fatal("starting a relay forgot the still-running old Codex")
	}
}

func TestCodexQuitRestoreLeavesRelayWithoutDisconnectedMark(t *testing.T) {
	cx, _ := connectedNativeCodexForRelay(t)
	p := fakeRelayQuitProcesses(t)
	if ids, err := RestoreOnQuit([]*Agent{cx}); err != nil || !slices.Equal(ids, []string{"codex"}) {
		t.Fatalf("restore on quit: %v, %v", ids, err)
	}
	if cx.Disconnected() || cx.Wired() {
		t.Fatal("quit restore did not leave Codex ready to rewire at start")
	}
	if ended, err := EndCodexFailover(); err != nil || ended {
		t.Fatalf("quit restore left legacy failover: %v, %v", ended, err)
	}
	if err := RelayOpenCodex(); err != nil || len(p.relays) != 1 {
		t.Fatalf("a quit's restore lost the open native thread: %v, %+v", err, p.relays)
	}
}

func TestCodexRelayRetiresOnlyKnownNewProcesses(t *testing.T) {
	cx, _ := connectedNativeCodexForRelay(t)
	p := fakeRelayQuitProcesses(t)
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	pending := relayPendingForTest(t)
	cutoff := time.Unix(0, pending.Before)
	p.running = []int{41, 42, 43}
	p.starts = map[int]time.Time{41: cutoff.Add(-time.Hour), 42: cutoff, 43: cutoff.Add(time.Hour)}
	if err := RelayOpenCodex(); err != nil {
		t.Fatal(err)
	}
	if len(p.relays) != 1 || !slices.Equal(p.relays[0].pids, []int{41, 42}) {
		t.Fatalf("old/equal/new processes relayed as %+v", p.relays)
	}
	p.running = []int{43}
	if err := RelayOpenCodex(); err != nil || len(p.relays) != 1 {
		t.Fatalf("a new Codex was relayed: %v, %+v", err, p.relays)
	}
	if stashLoad()[codexRelayKey] != "" {
		t.Fatal("recovery stayed after every old process ended")
	}
}

func TestCodexRelayKeepsUnknownProcessesAndFailedLists(t *testing.T) {
	cx, _ := connectedNativeCodexForRelay(t)
	p := fakeRelayQuitProcesses(t)
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	before := stashLoad()[codexRelayKey]
	p.listErr = errors.New("process list unavailable")
	if err := RelayOpenCodex(); !errors.Is(err, p.listErr) || len(p.relays) != 0 || stashLoad()[codexRelayKey] != before {
		t.Fatalf("failed list retired recovery: %v, %+v", err, p.relays)
	}
	p.listErr, p.startErr = nil, errors.New("start time unavailable")
	if err := RelayOpenCodex(); err != nil || len(p.relays) != 1 || stashLoad()[codexRelayKey] != before {
		t.Fatalf("unknown process start lost recovery: %v, %+v", err, p.relays)
	}
}

func TestCodexRelayLeavesNewerRecoveryRecord(t *testing.T) {
	cx, _ := connectedNativeCodexForRelay(t)
	p := fakeRelayQuitProcesses(t)
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	before := stashLoad()[codexRelayKey]
	var newer string
	codexPIDs = func(context.Context) ([]int, error) {
		// A real later route removal, while the list was being read.
		if _, err := cx.ConnectHow(); err != nil {
			t.Fatal(err)
		}
		if err := cx.Disconnect(); err != nil {
			t.Fatal(err)
		}
		newer = stashLoad()[codexRelayKey]
		return nil, nil
	}
	if err := RelayOpenCodex(); err != nil {
		t.Fatal(err)
	}
	if newer == before || stashLoad()[codexRelayKey] != newer || len(p.relays) != 0 {
		t.Fatal("a completed list forgot the route removed after it started")
	}
}

func TestCodexRelayNeedsAnEndpointMagpieRemoved(t *testing.T) {
	home, _ := codexHome(t, "", "model = \"gpt-6.1-sol\"\n")
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3499")
	p := fakeRelayQuitProcesses(t)
	if err := RelayOpenCodex(); err != nil || p.listed != 0 || len(p.relays) != 0 {
		t.Fatalf("Codex never connected got a relay: %v, %+v", err, p.relays)
	}
	// The user's handwritten endpoint is not legacy failover to remove.
	cx := codex(home)
	if err := edit.SetTOMLTop(cx.Path, edit.KV{Path: "openai_base_url", Value: gateway.URL() + gateway.CodexPath}); err != nil {
		t.Fatal(err)
	}
	if ended, err := EndCodexFailover(); err != nil || ended {
		t.Fatalf("handwritten endpoint was removed: %v, %v", ended, err)
	}
	if err := RelayOpenCodex(); err != nil || p.listed != 0 || len(p.relays) != 0 {
		t.Fatalf("handwritten endpoint got a relay: %v, %+v", err, p.relays)
	}
}

func TestCodexRelayKeepsRecoveryForItsOwnGateway(t *testing.T) {
	cx, _ := connectedNativeCodexForRelay(t)
	p := fakeRelayQuitProcesses(t)
	if err := cx.Disconnect(); err != nil {
		t.Fatal(err)
	}
	before := stashLoad()[codexRelayKey]
	for _, addr := range []string{"192.0.2.7:3499", "127.0.0.1:3498"} {
		t.Setenv("MAGPIE_ADDR", addr)
		if err := RelayOpenCodex(); err != nil || p.listed != 0 || len(p.relays) != 0 || stashLoad()[codexRelayKey] != before {
			t.Fatalf("another gateway changed recovery: %v, %+v", err, p.relays)
		}
	}
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3499")
	if err := RelayOpenCodex(); err != nil || len(p.relays) != 1 {
		t.Fatalf("own gateway lost its recovery: %v, %+v", err, p.relays)
	}
}

func TestCodexRelayDoesNotTakeRemoteOrDistroEndpoints(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "distro", true: "remote"}[remote], func(t *testing.T) {
			home, _ := codexHome(t, "", "")
			t.Setenv("MAGPIE_ADDR", "127.0.0.1:3499")
			at := here(home)
			base := gateway.URL() + gateway.CodexPath
			if remote {
				at.base = func() string { return "http://192.0.2.7:3499" }
				base = at.codexURL()
			} else {
				at.spell = func(path string) string { return path }
			}
			if err := rememberCodexRelay(at, base); err != nil {
				t.Fatal(err)
			}
			if stashLoad()[codexRelayKey] != "" {
				t.Fatal("recorded another machine's endpoint")
			}
		})
	}
}

func TestCodexRelayUnreadRecoveryKeepsEndpoint(t *testing.T) {
	cx, read := connectedNativeCodexForRelay(t)
	stash(map[string]string{codexRelayKey: "{cut off"})
	if err := cx.Disconnect(); err == nil {
		t.Fatal("unknown recovery was taken for none")
	}
	if !strings.Contains(read(), "openai_base_url") || stashLoad()[codexRelayKey] != "{cut off" {
		t.Fatal("failed recovery persistence left the endpoint removed")
	}
}

func TestCodexRelayUnreadStashIsNotAnEmptyRecovery(t *testing.T) {
	stashHome(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3499")
	p := fakeRelayQuitProcesses(t)
	if err := os.MkdirAll(filepath.Dir(stashPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"codex.relay":`, `[]`} {
		if err := os.WriteFile(stashPath(), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		filememo.Forget()
		if err := RelayOpenCodex(); err == nil || p.listed != 0 {
			t.Fatalf("unreadable stash retired recovery: %v", err)
		}
		b, _ := os.ReadFile(stashPath())
		if string(b) != raw {
			t.Fatal("unreadable stash was overwritten")
		}
	}
}

// EndFailover is called directly at quit, outside Unwire's atomic wrapper.
func TestCodexFailoverKeepsEndpointWhenRelayCannotBeRemembered(t *testing.T) {
	home, read := codexHome(t, "", "model = \"gpt-6.1-sol\"\n")
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3499")
	cx := codex(home)
	if err := edit.SetTOMLTop(cx.Path, edit.KV{Path: "openai_base_url", Value: gateway.URL() + gateway.CodexPath}); err != nil {
		t.Fatal(err)
	}
	stash(map[string]string{"codex.failover": "1", codexRelayKey: "{cut off"})
	wasKeep := keepCodexDaemon
	t.Cleanup(func() { keepCodexDaemon = wasKeep })
	keepCodexDaemon = func(context.Context, string, time.Time) provider.CodexDaemonCheck { return provider.CodexDaemonCheck{} }
	if ended, err := EndCodexFailover(); err == nil || ended {
		t.Fatalf("failed recovery was reported restored: %v, %v", ended, err)
	}
	if !strings.Contains(read(), "openai_base_url") || stashLoad()["codex.failover"] != "1" {
		t.Fatalf("failed recovery lost the endpoint or its failover mark:\n%s", read())
	}
}
