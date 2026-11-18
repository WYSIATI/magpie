package gui

import (
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/agent"
	"github.com/yetone/magpie/internal/settings"
)

// Exercise the actual helper OnShutdown calls. Ending no failover must
// still consider an earlier Disconnect, and a restore made at this quit
// must happen before the pending-runtime relay is asked for.
func TestQuitRelaysCodexAfterDisconnectAndRestore(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "already-disconnected", true: "restore-at-quit"}[restore], func(t *testing.T) {
			sandboxHome(t)
			oldServed, oldDetect, oldRestore := servedHere, quitDetect, quitRestore
			oldEnd, oldPort, oldRelay := endCodexAtQuit, portCodexAtQuit, relayCodexAtQuit
			t.Cleanup(func() {
				servedHere, quitDetect, quitRestore = oldServed, oldDetect, oldRestore
				endCodexAtQuit, portCodexAtQuit, relayCodexAtQuit = oldEnd, oldPort, oldRelay
			})
			if err := settings.Save(settings.Settings{RestoreOnQuit: restore}); err != nil {
				t.Fatal(err)
			}
			var calls []string
			pending := !restore
			servedHere = func() bool { return true }
			endCodexAtQuit = func() (bool, error) { calls = append(calls, "end"); return false, nil }
			portCodexAtQuit = func(time.Duration) { calls = append(calls, "port") }
			quitDetect = func() []*agent.Agent { return nil }
			quitRestore = func([]*agent.Agent) ([]string, error) {
				calls = append(calls, "restore")
				pending = true
				return nil, nil
			}
			relayCodexAtQuit = func() error {
				calls = append(calls, "relay")
				if !pending {
					t.Fatal("relay checked before the quit restored Codex's endpoint")
				}
				return nil
			}
			finishAgentRoutesOnQuit()
			want := []string{"end", "port", "relay"}
			if restore {
				want = []string{"end", "port", "restore", "relay"}
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("Codex left with no relay after failover ended false: calls %v, want %v", calls, want)
			}
			// An app showing another gateway must leave its listener and
			// agents to that serving magpie, as before this change.
			servedHere = func() bool { return false }
			calls = nil
			finishAgentRoutesOnQuit()
			if !slices.Equal(calls, []string{"port"}) {
				t.Fatalf("a non-serving app changed the gateway's routes: %v", calls)
			}
		})
	}
}
