package main

import (
	"fmt"
	"os/exec"
)

const ksChain = "VPNCTL_KS"

type KillSwitch struct {
	state *AppState
	armed bool
}

func NewKillSwitch(state *AppState) *KillSwitch {
	return &KillSwitch{state: state}
}

func (ks *KillSwitch) IsArmed() bool {
	return ks.armed
}

func (ks *KillSwitch) Arm(allowLAN bool) error {
	// Flush or create chain
	exec.Command("iptables", "-N", ksChain).Run()
	exec.Command("iptables", "-F", ksChain).Run()

	rules := [][]string{
		{"-A", ksChain, "-o", "lo", "-j", "ACCEPT"},
		{"-A", ksChain, "-i", "lo", "-j", "ACCEPT"},
		{"-A", ksChain, "-o", "tun+", "-j", "ACCEPT"},
		{"-A", ksChain, "-i", "tun+", "-j", "ACCEPT"},
		{"-A", ksChain, "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	}

	if allowLAN {
		rules = append(rules,
			[]string{"-A", ksChain, "-d", "192.168.0.0/16", "-j", "ACCEPT"},
			[]string{"-A", ksChain, "-d", "10.0.0.0/8", "-j", "ACCEPT"},
			[]string{"-A", ksChain, "-d", "172.16.0.0/12", "-j", "ACCEPT"},
			[]string{"-A", ksChain, "-s", "192.168.0.0/16", "-j", "ACCEPT"},
			[]string{"-A", ksChain, "-s", "10.0.0.0/8", "-j", "ACCEPT"},
			[]string{"-A", ksChain, "-s", "172.16.0.0/12", "-j", "ACCEPT"},
		)
	}

	rules = append(rules, []string{"-A", ksChain, "-j", "DROP"})

	for _, rule := range rules {
		if out, err := exec.Command("iptables", rule...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v: %v — %s", rule, err, out)
		}
	}

	// Insert chain into INPUT/OUTPUT/FORWARD
	for _, hook := range []string{"INPUT", "OUTPUT", "FORWARD"} {
		// Remove existing reference first to avoid duplicates
		exec.Command("iptables", "-D", hook, "-j", ksChain).Run()
		exec.Command("iptables", "-I", hook, "-j", ksChain).Run()
	}

	ks.armed = true
	ks.state.AddLog("warn", "[killswitch] ARMED — traffic blocked if all tunnels down")
	return nil
}

func (ks *KillSwitch) Disarm() error {
	for _, hook := range []string{"INPUT", "OUTPUT", "FORWARD"} {
		exec.Command("iptables", "-D", hook, "-j", ksChain).Run()
	}
	exec.Command("iptables", "-F", ksChain).Run()
	exec.Command("iptables", "-X", ksChain).Run()

	ks.armed = false
	ks.state.AddLog("ok", "[killswitch] disarmed")
	return nil
}
