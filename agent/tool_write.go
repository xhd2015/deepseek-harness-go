package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (e *ToolExecutor) write(ctx context.Context, inv ToolInvocation, args map[string]any) (string, bool) {
	filePath, ok := stringArg(args, "file_path")
	if !ok || strings.TrimSpace(filePath) == "" {
		return "file_path must be a non-empty string", true
	}
	content, ok := stringArg(args, "content")
	if !ok {
		return "content must be a string", true
	}
	if err := ctx.Err(); err != nil {
		return err.Error(), true
	}
	target, display, err := e.resolvePath(inv, filePath, true)
	if err != nil {
		return fmt.Sprintf("cannot write %q: %s", display, pathErrorText(err)), true
	}
	if err := e.allowMutation(inv, target, display); err != nil {
		return err.Error(), true
	}

	// New files match DSH's atomic writer, which stages a 0600 temp file; an
	// existing file keeps its permission bits.
	mode := os.FileMode(0o600)
	operation := "create"
	if info, statErr := os.Stat(target); statErr == nil {
		if !info.Mode().IsRegular() {
			return fmt.Sprintf("cannot write %q: not a regular file", display), true
		}
		if err := e.requireObserved(inv, target, display); err != nil {
			return err.Error(), true
		}
		mode = info.Mode().Perm()
		operation = "update"
	} else if !os.IsNotExist(statErr) {
		return fmt.Sprintf("cannot write %q: %s", display, pathErrorText(statErr)), true
	}

	if err := writeFileAtomic(target, []byte(content), mode); err != nil {
		return fmt.Sprintf("cannot write %q: %s", display, pathErrorText(err)), true
	}
	e.markObserved(inv, target)
	verb := "Created"
	if operation == "update" {
		verb = "Updated"
	}
	return fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s file\n</content>", filepath.ToSlash(target), verb), false
}

// writeFileAtomic stages content beside the target, syncs it, and renames it
// into place so readers never observe a partially written file.
func writeFileAtomic(target string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".dsh-write-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(name)
	}
	if err := temp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, target); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
