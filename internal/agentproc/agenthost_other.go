//go:build !linux

package agentproc

// rewriteAllowedToolsForSandbox is the non-Linux stub. shouldSandbox
// returns false off Linux, so this function is unreachable in
// practice — it exists to keep the package compileable on Darwin
// dev boxes.
func rewriteAllowedToolsForSandbox(allowedTools, _ string) string {
	return allowedTools
}
