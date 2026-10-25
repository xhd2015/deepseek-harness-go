package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func (e *ToolExecutor) edit(ctx context.Context, inv ToolInvocation, args map[string]any) (string, bool) {
	filePath, ok := stringArg(args, "file_path")
	if !ok || strings.TrimSpace(filePath) == "" {
		return "file_path must be a non-empty string", true
	}
	oldString, ok := stringArg(args, "old_string")
	if !ok || oldString == "" {
		return "old_string must be a non-empty string", true
	}
	newString, ok := stringArg(args, "new_string")
	if !ok {
		return "new_string must be a string", true
	}
	if oldString == newString {
		return "old_string and new_string must differ", true
	}
	if err := ctx.Err(); err != nil {
		return err.Error(), true
	}
	target, display, err := e.resolvePath(inv, filePath, false)
	if err != nil {
		return fmt.Sprintf("cannot edit %q: %s", display, pathErrorText(err)), true
	}
	if err := e.allowMutation(inv, target, display); err != nil {
		return err.Error(), true
	}
	if err := e.requireObserved(inv, target, display); err != nil {
		return err.Error(), true
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Sprintf("cannot edit %q: %s", display, pathErrorText(err)), true
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("cannot edit %q: not a regular file", display), true
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return fmt.Sprintf("cannot edit %q: %v", display, err), true
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return fmt.Sprintf("cannot edit %q: binary file", display), true
	}
	if !utf8.Valid(raw) {
		return fmt.Sprintf("cannot edit %q: invalid UTF-8 text", display), true
	}
	lineEndings := detectLineEndings(string(raw))
	content := normalizeLineEndings(string(raw))
	oldNorm := normalizeLineEndings(oldString)
	replacements := strings.Count(content, oldNorm)
	if replacements == 0 {
		return fmt.Sprintf("old_string was not found in %q", display), true
	}
	replaceAll := boolArg(args, "replace_all")
	if !replaceAll && replacements > 1 {
		return fmt.Sprintf("old_string matched %d times in %q; provide a more specific old_string or set replace_all to true", replacements, display), true
	}
	updated := strings.Replace(content, oldNorm, normalizeLineEndings(newString), 1)
	if replaceAll {
		updated = strings.ReplaceAll(content, oldNorm, normalizeLineEndings(newString))
	}
	if err := writeFileAtomic(target, []byte(restoreLineEndings(updated, lineEndings)), info.Mode().Perm()); err != nil {
		return fmt.Sprintf("cannot edit %q: %v", display, err), true
	}
	e.markObserved(inv, target)
	if replaceAll {
		return fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", filepath.ToSlash(target)), false
	}
	return fmt.Sprintf("The file %s has been updated successfully.", filepath.ToSlash(target)), false
}

func normalizeLineEndings(text string) string {
	return strings.ReplaceAll(text, "\r\n", "\n")
}

func detectLineEndings(text string) string {
	sample := text
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	crlf := strings.Count(sample, "\r\n")
	lf := strings.Count(sample, "\n") - crlf
	if crlf > lf {
		return "CRLF"
	}
	return "LF"
}

func restoreLineEndings(text, style string) string {
	if style != "CRLF" {
		return text
	}
	return strings.ReplaceAll(normalizeLineEndings(text), "\n", "\r\n")
}
