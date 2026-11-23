package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Workspace-write confinement, as a verifiable refusal rather than an OS sandbox.
//
// This backend does not run an OS-level confinement runner (macOS sandbox-exec,
// Linux bwrap). What it does is refuse and say so, using the vocabulary DSH
// teaches the model, so a denied write is recognizable in both. The gap to real
// confinement is stated in the spec, not hidden: a shell command that reaches
// outside the workspace by a path this check cannot see (a symlink resolved
// inside the command, a network write) is not blocked.

// sandboxDenialMarker is the model-facing denial line, byte-identical to DSH's
// `sandboxDenialMarker`, so the model treats a refusal the same way on both hosts.
func sandboxDenialMarker(mode string) string {
	return fmt.Sprintf("[sandbox: file access denied under %s mode]", mode)
}

// escalationHintMarker is DSH's same-turn hint: the retry the model is allowed to
// make, stated where the denial happens rather than in the tool description only.
func escalationHintMarker(subject string) string {
	return fmt.Sprintf("[sandbox: escalation available — retry this exact %s once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]", subject)
}

// neverSentence and askSentence are DSH's model-facing policy statements.
const neverSentence = "Approval prompts are disabled in this session: actions that require approval are rejected automatically — do not request sandbox escalation (do not set `sandbox_permissions`)."

const askSentence = "Approval prompts are enabled in this session: operations that require approval may ask through the configured answerers; without an available answerer, the request fails closed."

// sandboxPolicy is the effective permission state one call runs under.
type sandboxPolicy struct {
	Mode     string
	Approval string
}

// readOnly reports whether writes are refused outright.
func (p sandboxPolicy) readOnly() bool { return p.Mode == "read-only" }

// workspaceWrite reports whether writes are confined to the workspace.
func (p sandboxPolicy) workspaceWrite() bool { return p.Mode == "workspace-write" }

// unrestricted reports whether no write check applies.
func (p sandboxPolicy) unrestricted() bool { return p.Mode == "danger-full-access" }

// temporaryRoots lists the directories a workspace-write session may also write.
//
// DSH grants the host `/tmp` and the platform temp dir, each also in its
// symlink-resolved spelling so `/tmp` and `/private/tmp` match alike.
func temporaryRoots() []string {
	roots := []string{os.TempDir(), "/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"}
	seen := map[string]bool{}
	out := make([]string, 0, len(roots)*2)
	for _, root := range roots {
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
		if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root && !seen[resolved] {
			seen[resolved] = true
			out = append(out, resolved)
		}
	}
	return out
}

// canonicalPath resolves a path through its nearest existing ancestor, so a file
// created through a directory symlink has one spelling before and after it exists.
func canonicalPath(path string) string {
	missing := []string{}
	head := path
	for {
		if resolved, err := filepath.EvalSymlinks(head); err == nil {
			parts := append([]string{resolved}, missing...)
			return filepath.Join(parts...)
		}
		parent := filepath.Dir(head)
		if parent == head || filepath.Dir(parent) == parent {
			return path
		}
		missing = append([]string{filepath.Base(head)}, missing...)
		head = parent
	}
}

// withinBoundary reports whether a target may be written under one mode.
//
// The workspace root and the temporary roots are both allowed; everything else is
// outside. Comparison is on canonical spellings so a symlink cannot be used to
// step outside while looking inside.
func withinBoundary(mode, base, target string) bool {
	if mode == "danger-full-access" {
		return true
	}
	if mode == "read-only" {
		return false
	}
	canonical := canonicalPath(target)
	candidates := []string{canonicalPath(base)}
	candidates = append(candidates, temporaryRoots()...)
	for _, root := range candidates {
		if root == "" {
			continue
		}
		if withinBase(root, canonical) {
			return true
		}
	}
	return false
}

// writeDenial explains why a mutation is refused, or returns "" when it is allowed.
//
// Two different refusals share one vocabulary on purpose: the model should not have
// to distinguish "read-only means no writes at all" from "this path is outside the
// workspace" to know what it may try next.
func writeDenial(policy sandboxPolicy, base, target, display string) string {
	if policy.unrestricted() {
		return ""
	}
	if withinBoundary(policy.Mode, base, target) {
		return ""
	}
	if policy.readOnly() {
		return "\ncannot write \"" + display + "\": " + sandboxDenialMarker(policy.Mode)
	}
	if policy.Approval == "ask" {
		return "\ncannot write \"" + display + "\": " + sandboxDenialMarker(policy.Mode) +
			"\n" + escalationHintMarker("operation")
	}
	return "\ncannot write \"" + display + "\": " + sandboxDenialMarker(policy.Mode) +
		"\n" + neverSentence
}

// Bash write detection.
//
// A shell command's effects cannot be known without running it, so this recognizes
// the shapes that write to an explicit absolute path. It is a heuristic and the
// spec says so: `sh -c` with a computed path, or a program that writes on its own,
// is not detected. The alternative — refusing every command because some might
// write — would make workspace-write useless.

// writeTargetPatterns match a redirection or an in-place edit to an absolute path.
var writeTargetPatterns = []*regexp.Regexp{
	// > /abs/path, >> /abs/path, 2> /abs/path
	regexp.MustCompile(`(?:^|[^>0-9])>{1,2}\s*("([^"]+)"|'([^']+)'|([^\s;&|<>]+))`),
	// tee /abs/path, dd of=/abs/path, sed -i ... /abs/path (covered below), cp/mv target
	regexp.MustCompile(`\btee\s+(?:-a\s+)?("([^"]+)"|'([^']+)'|([^\s;&|<>]+))`),
	regexp.MustCompile(`\bof=("([^"]+)"|'([^']+)'|([^\s;&|<>]+))`),
}

// shellWriteTargets extracts the absolute paths a command appears to write.
func shellWriteTargets(command string) []string {
	targets := []string{}
	for _, pattern := range writeTargetPatterns {
		for _, match := range pattern.FindAllStringSubmatch(command, -1) {
			for _, group := range match[1:] {
				candidate := strings.TrimSpace(group)
				if candidate == "" || !strings.HasPrefix(candidate, "/") {
					continue
				}
				// A redirection into /dev/null and friends is not a workspace write.
				if strings.HasPrefix(candidate, "/dev/") {
					continue
				}
				targets = append(targets, candidate)
			}
		}
	}
	// cp / mv / install / ln write their last argument.
	for _, verb := range []string{"cp ", "mv ", "install ", "ln "} {
		index := strings.Index(command, verb)
		if index < 0 {
			continue
		}
		fields := strings.Fields(command[index+len(verb):])
		if len(fields) == 0 {
			continue
		}
		last := fields[len(fields)-1]
		if strings.HasPrefix(last, "/") && !strings.HasPrefix(last, "/dev/") {
			targets = append(targets, last)
		}
	}
	return targets
}

// bashDenial decides whether one command may run, and explains a refusal.
//
// The order matters: an explicit `sandbox_permissions` is validated first, because
// a widened retry is the sanctioned response to a denial and must not itself be
// refused for the reason it exists to lift.
func (e *ToolExecutor) bashDenial(inv ToolInvocation, base, command string, args map[string]any) string {
	policy := e.policyFor(inv)
	requested, justification := escalationArgs(args)
	ignoredHint := ""
	if requested != "" {
		// An ask that cannot widen this call is ignored, not refused: the call
		// proceeds at its own mode. Refusing would make a harmless bit of extra
		// information fatal, and the model only learns the mode from the marker.
		if !isStrictlyWider(requested, policy.Mode) {
			// Purely informational: the call runs at its own mode and the marker says
			// so. DSH returns `ignored` before any pairing check, because a request
			// that cannot widen the call has nothing to validate.
			ignoredHint = escalationIgnoredMarker(requested, policy.Mode)
			requested = ""
		}
	}
	if requested != "" {
		// An argument error is reported as one: a missing justification is a
		// malformed retry, not a policy decision, and calling it denied would teach
		// the model the wrong lesson.
		if errText := validateEscalation(requested, justification, policy.Mode); errText != "" {
			return errText
		}
		if policy.Approval == "never" {
			// Deterministic refusal: the policy has already decided that no prompt
			// will be shown, so offering one would be a lie the model waits on.
			return "cannot escalate: " + sandboxDenialMarker(policy.Mode) + "\n" + neverSentence
		}
		outcome := ApprovalUnavailable
		if e.askApproval != nil {
			outcome = e.askApproval(inv, requested, policy.Mode, command)
		}
		if outcome == ApprovalAllowedOnce {
			// Allowed once: this call runs wider, and no standing permission is
			// recorded, so the next call is checked again.
			return ""
		}
		return "cannot escalate: " + sandboxDenialMarker(policy.Mode) +
			"\napproval was " + string(outcome)
	}
	if policy.unrestricted() {
		return ""
	}
	for _, target := range shellWriteTargets(command) {
		if !withinBoundary(policy.Mode, base, target) {
			return "cannot run this command: " + sandboxDenialMarker(policy.Mode) +
				"\nwrite to " + target + " is outside the session workspace" +
				"\n" + escalationHintOrNever(policy)
		}
	}
	return ignoredHint
}

// escalationIgnoredMarker reports an ask that could not widen the call, matching
// DSH's wording so the model sees the same fact on either host.
func escalationIgnoredMarker(requested, effective string) string {
	return fmt.Sprintf("[sandbox: escalation to %q ignored — this call ran at %q mode]", requested, effective)
}

// escalationHintOrNever states what the model may do next under this policy.
func escalationHintOrNever(policy sandboxPolicy) string {
	if policy.Approval == "ask" {
		return escalationHintMarker("command")
	}
	return neverSentence
}

// escalationArgs reads the two escalation fields a tool call may carry.
func escalationArgs(args map[string]any) (mode, justification string) {
	if raw, present := args["sandbox_permissions"]; present {
		mode, _ = raw.(string)
		mode = strings.TrimSpace(mode)
	}
	if raw, present := args["justification"]; present {
		justification, _ = raw.(string)
		justification = strings.TrimSpace(justification)
	}
	return mode, justification
}

// validateEscalation reproduces DSH's argument rules.
//
// A named mode that is not strictly wider than the current one is ignored rather
// than granted: the model asked for something it already had, and treating that as
// an escalation would spend a user prompt on nothing.
func validateEscalation(requested, justification, effective string) string {
	if !isStrictlyWider(requested, effective) {
		return "invalid escalation: " + sandboxDenialMarker(effective) +
			"\n" + requested + " is not wider than the current mode; retry with a wider mode or without sandbox_permissions"
	}
	if justification == "" {
		return "invalid escalation: sandbox_permissions requires a justification" +
			"\nstate, in one sentence, why this exact operation needs the wider access"
	}
	if requested != "" && sandboxRankOf(requested) < 0 {
		return "invalid escalation: unknown sandbox mode \"" + requested + "\""
	}
	return ""
}

// isStrictlyWider reports whether a request names a mode wider than the effective one.
func isStrictlyWider(requested, effective string) bool {
	return sandboxRankOf(requested) > sandboxRankOf(effective)
}

// sandboxRankOf orders the modes from narrowest to widest.
func sandboxRankOf(mode string) int {
	switch mode {
	case "read-only":
		return 0
	case "workspace-write":
		return 1
	case "danger-full-access":
		return 2
	default:
		return -1
	}
}
