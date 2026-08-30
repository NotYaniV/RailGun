package main

import "testing"

// Arms must alternate rather than run in blocks. If all five NGINX runs happen
// before all five Railgun runs, any drift in the rig over those minutes —
// a noisy neighbour arriving, thermal behaviour, a network change — is
// perfectly confounded with the arm being tested.
func TestOrderAlternatesArms(t *testing.T) {
	got := runOrder([]string{"null", "nginx"}, []string{"W1"}, 3)

	if len(got) != 6 {
		t.Fatalf("runOrder() produced %d steps, want 6", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Arm == got[i-1].Arm {
			t.Errorf("steps %d and %d are both arm %q; arms must alternate", i-1, i, got[i].Arm)
		}
	}
}

func TestOrderCoversEveryCombination(t *testing.T) {
	got := runOrder([]string{"null", "nginx"}, []string{"W1", "W2"}, 5)
	if len(got) != 2*2*5 {
		t.Fatalf("runOrder() produced %d steps, want 20", len(got))
	}
	seen := map[string]int{}
	for _, s := range got {
		seen[s.Arm+"/"+s.Workload]++
	}
	for _, k := range []string{"null/W1", "null/W2", "nginx/W1", "nginx/W2"} {
		if seen[k] != 5 {
			t.Errorf("%s ran %d times, want 5", k, seen[k])
		}
	}
}

func TestOrderRepeatNumbersAreSequentialPerCombination(t *testing.T) {
	got := runOrder([]string{"nginx"}, []string{"W1"}, 3)
	for i, s := range got {
		if s.Repeat != i+1 {
			t.Errorf("step %d has Repeat = %d, want %d", i, s.Repeat, i+1)
		}
	}
}
