package ethertest

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestHeadV2ReplayRetainsBranchDependentRoots(t *testing.T) {
	for _, fork := range []struct {
		name  string
		epoch int64
	}{{"fulu", -1}, {"gloas", 0}} {
		for _, missed := range []bool{false, true} {
			name := fork.name + "/all_slots"
			if missed {
				name = fork.name + "/missed_dependency_slots"
			}
			t.Run(name, func(t *testing.T) {
				cfg := amsterdamTestConfig()
				cfg.Chain.Forks.AmsterdamEpoch = fork.epoch
				n := startRPCNode(t, cfg)
				ctx := t.Context()
				if err := n.PauseFinality(ctx); err != nil {
					t.Fatal(err)
				}
				if err := n.CreateBranch(ctx, "alternate", 0); err != nil {
					t.Fatal(err)
				}
				epochSlots := cfg.Chain.SlotsPerEpoch
				for slot := uint64(1); slot <= 2*epochSlots; slot++ {
					if missed && (slot == epochSlots-1 || slot == 2*epochSlots-1) {
						if _, err := n.MissSlots(ctx, 1); err != nil {
							t.Fatal(err)
						}
					} else if _, err := n.Mine(ctx, 1, true); err != nil {
						t.Fatal(err)
					}
				}
				events, err := n.EventsSince(0)
				if err != nil {
					t.Fatal(err)
				}
				var heads []Event
				var original [][]byte
				for _, event := range events {
					// Cover both genesis fallbacks and both epoch-two dependencies.
					if event.Type != "block" || (event.Slot != 1 && event.Slot != epochSlots && event.Slot != 2*epochSlots) {
						continue
					}
					messages := n.beaconEventMessages(event, map[string]bool{"head_v2": true})
					if len(messages) != 1 {
						t.Fatalf("slot %d: got %d head events", event.Slot, len(messages))
					}
					data := messages[0].data.(map[string]any)["data"].(map[string]any)
					current, next := uint64(0), uint64(0)
					switch event.Slot {
					case epochSlots:
						next = epochSlots - 1
					case 2 * epochSlots:
						current, next = epochSlots-1, 2*epochSlots-1
					}
					if missed {
						if current != 0 {
							current--
						}
						if next != 0 {
							next--
						}
					}
					for field, slot := range map[string]uint64{"current_epoch_dependent_root": current, "next_epoch_dependent_root": next} {
						root, err := n.beaconRoot(n.chain.blockAtOrBeforeSlot(slot))
						if err != nil {
							t.Fatal(err)
						}
						if data[field] != common.Hash(root).Hex() {
							t.Fatalf("head slot %d: %s = %v, want root at slot %d", event.Slot, field, data[field], slot)
						}
					}
					encoded, err := json.Marshal(messages[0].data)
					if err != nil {
						t.Fatal(err)
					}
					heads = append(heads, event)
					original = append(original, encoded)
				}
				if len(heads) != 3 {
					t.Fatalf("got %d head fixtures, want 3", len(heads))
				}
				if _, err := n.MineBranch(ctx, "alternate", 2*epochSlots); err != nil {
					t.Fatal(err)
				}
				if err := n.SwitchBranch(ctx, "alternate"); err != nil {
					t.Fatal(err)
				}
				for i, event := range heads {
					messages := n.beaconEventMessages(event, map[string]bool{"head_v2": true})
					if len(messages) != 1 {
						t.Fatalf("slot %d: replay returned %d head events", event.Slot, len(messages))
					}
					encoded, err := json.Marshal(messages[0].data)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(original[i], encoded) {
						t.Fatalf("head event at slot %d changed after reorg:\nbefore: %s\nafter: %s", event.Slot, original[i], encoded)
					}
				}
			})
		}
	}
}
