package main

import "testing"

func TestControllerCmdLeaderElectionDefaults(t *testing.T) {
	cmd := newControllerCmd()
	f := cmd.Flags()

	le, err := f.GetBool("leader-elect")
	if err != nil {
		t.Fatalf("leader-elect flag: %v", err)
	}
	if !le {
		t.Errorf("leader-elect default = false, want true (HA-safe default)")
	}

	ns, err := f.GetString("leader-elect-namespace")
	if err != nil {
		t.Fatalf("leader-elect-namespace flag: %v", err)
	}
	if ns != "" {
		t.Errorf("leader-elect-namespace default = %q, want empty (in-cluster namespace)", ns)
	}
}

func TestControllerCmdLeaderElectDisable(t *testing.T) {
	cmd := newControllerCmd()
	if err := cmd.Flags().Parse([]string{"--leader-elect=false"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	le, _ := cmd.Flags().GetBool("leader-elect")
	if le {
		t.Errorf("--leader-elect=false did not disable leader election")
	}
}
