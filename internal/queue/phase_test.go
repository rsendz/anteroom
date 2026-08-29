package queue

import (
	"encoding/json"
	"testing"
)

func TestPhaseCrossesTheAPIAsAName(t *testing.T) {
	// The dashboard and the position stream both match on the name, so a
	// number here would silently never match.
	for _, tc := range []struct {
		phase Phase
		want  string
	}{
		{PhaseQueueing, `"queueing"`},
		{PhaseBefore, `"before"`},
		{PhaseDraw, `"draw"`},
		{PhaseClosed, `"closed"`},
	} {
		got, err := json.Marshal(tc.phase)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("Marshal(%v) = %s, want %s", tc.phase, got, tc.want)
		}

		var back Phase
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatal(err)
		}
		if back != tc.phase {
			t.Errorf("round trip gave %v, want %v", back, tc.phase)
		}
	}
}

func TestSnapshotJSONCarriesThePhaseName(t *testing.T) {
	body, err := json.Marshal(Snapshot{Room: "drop", Phase: PhaseDraw, Lottery: true})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["phase"] != "draw" {
		t.Errorf("phase = %v, want the string \"draw\"", out["phase"])
	}
}
