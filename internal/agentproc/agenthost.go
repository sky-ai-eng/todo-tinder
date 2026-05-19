package agentproc

// sandboxTFBinary is the canonical in-sandbox path the host TF binary
// is bind-mounted at. Picked once and used everywhere — BuildAllowedTools
// embeds this path in its `Bash(<selfBin> exec *)` pattern, and the agent
// invokes it under the same path. Anything outside this constant would
// break the per-tool path check Claude Code applies before exec.
//
// The agenthost socket lives at /run/tf.sock; that constant is owned by
// cmd/exec/agenthost (DefaultSocketPath) so the agent-side AutoDetect
// and the spawner-side mount registration agree by construction. The
// binary path is owned here because it's an agentproc-internal
// concern — the allowlist patterns that name it are constructed in
// agentproc.BuildAllowedTools.
//
// Defined without a build tag so the constant is reachable from
// agentproc.Run's sandbox-branch code on every platform's build —
// shouldSandbox gates the runtime use to Linux, but the package still
// has to compile on Darwin dev boxes.
const sandboxTFBinary = "/usr/local/bin/triagefactory"
