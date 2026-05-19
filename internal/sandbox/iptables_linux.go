//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

// applyMasquerade adds the NAT POSTROUTING rule that lets the
// sandbox reach the public internet through the upstream interface.
// Matches precns-test.sh line 33:
//
//	iptables -t nat -A POSTROUTING -s <subnet> -o <upstreamIF> -j MASQUERADE
//
// Also enables net.ipv4.ip_forward (idempotent — checks current
// value first so we don't toggle on every run).
//
// Returns the rule's literal args so teardownIptables can mirror
// them in a -D call.
func applyMasquerade(ctx context.Context, subnet, upstreamIF string) (iptablesRule, error) {
	if err := ensureIPForward(); err != nil {
		return iptablesRule{}, fmt.Errorf("iptables: ip_forward: %w", err)
	}

	rule := iptablesRule{
		table: "nat",
		chain: "POSTROUTING",
		args: []string{
			"-s", subnet,
			"-o", upstreamIF,
			"-j", "MASQUERADE",
		},
	}
	args := append([]string{"-t", rule.table, "-A", rule.chain}, rule.args...)
	if err := runIptables(ctx, args...); err != nil {
		return iptablesRule{}, err
	}
	return rule, nil
}

// teardownIptables removes the masquerade rule. Idempotent — a
// missing rule errors with exit code 1 from iptables, which we
// silence.
func teardownIptables(ctx context.Context, rule iptablesRule) error {
	if len(rule.args) == 0 {
		return nil
	}
	args := append([]string{"-t", rule.table, "-D", rule.chain}, rule.args...)
	cmd := exec.CommandContext(ctx, "iptables", args...)
	_ = cmd.Run() // best-effort
	return nil
}

// applyEgressPolicy is the SKY-335 hook point. Called with an empty
// allowlist in SKY-254 — does nothing, leaves MASQUERADE wide open
// for the test/dev path. SKY-335 will pass actual proxy IPs here
// and this function will install FORWARD-chain rules that DROP
// everything except those.
//
// Kept here (rather than as a no-op in agentproc.Run) so the SKY-335
// PR has an obvious single place to wire its logic without touching
// run.go.
func applyEgressPolicy(_ context.Context, _ string, allowed []netip.Prefix) error {
	if len(allowed) == 0 {
		// SKY-254 path: no allowlist enforced. MASQUERADE alone lets
		// sandbox traffic out. SKY-335 will tighten this.
		return nil
	}
	// SKY-335 implementation goes here. The signature is locked so
	// the future PR is a pure addition inside this function.
	return nil
}

// reapIptablesForSubnet removes every POSTROUTING MASQUERADE rule
// matching the given subnet. Used by the orphan reaper at startup
// when we know a netns was leaked (because we couldn't see its Close
// run) and want to also clean the MASQUERADE rule it installed.
//
// We don't need to know the upstream interface that was used — match
// by subnet alone. Since `10.42.0.0/16` is the sandbox's private
// allocation pool, anything we find there is unambiguously ours.
//
// Best-effort: on iptables exec error, skip and move on. Returning
// errors would just turn into log spam at startup; the operator can
// `iptables -t nat -F POSTROUTING` if reaper falls behind.
func reapIptablesForSubnet(ctx context.Context, subnet string) {
	out, err := exec.CommandContext(ctx, "iptables", "-t", "nat", "-S", "POSTROUTING").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Match shape: `-A POSTROUTING -s 10.42.N.0/24 -o eth0 -j MASQUERADE`.
		if !strings.HasPrefix(line, "-A POSTROUTING ") {
			continue
		}
		if !strings.Contains(line, "-s "+subnet) {
			continue
		}
		if !strings.Contains(line, "-j MASQUERADE") {
			continue
		}
		// Swap -A for -D, exec.
		delRule := strings.Replace(line, "-A POSTROUTING", "-D POSTROUTING", 1)
		args := append([]string{"-t", "nat"}, strings.Fields(delRule)...)
		_ = exec.CommandContext(ctx, "iptables", args...).Run()
	}
}

// runIptables wraps `iptables` with context + stderr capture.
func runIptables(ctx context.Context, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "iptables", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("iptables %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ensureIPForward reads /proc/sys/net/ipv4/ip_forward and writes "1"
// only if it's not already 1. Avoids fighting with other workloads
// on the host that might toggle it (Docker does too, idempotently —
// we mirror its behaviour).
func ensureIPForward() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	cur, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(cur)) == "1" {
		return nil
	}
	return os.WriteFile(path, []byte("1"), 0o644)
}
