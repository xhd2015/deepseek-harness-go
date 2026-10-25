package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type readLine struct {
	number int
	text   string
}

func (e *ToolExecutor) read(ctx context.Context, inv ToolInvocation, args map[string]any) (string, bool) {
	filePath, ok := stringArg(args, "file_path")
	if !ok || strings.TrimSpace(filePath) == "" {
		return "file_path must be a non-empty string", true
	}
	offset, err := positiveIntArg(args, "offset", 1)
	if err != nil {
		return err.Error(), true
	}
	limit, err := positiveIntArg(args, "limit", readLineLimit)
	if err != nil {
		return err.Error(), true
	}
	if limit > readLineLimit {
		return fmt.Sprintf("limit must be less than or equal to %d", readLineLimit), true
	}
	target, display, err := e.resolvePath(inv, filePath, false)
	if err != nil {
		return fmt.Sprintf("cannot read %q: %s", display, pathErrorText(err)), true
	}
	file, err := os.Open(target)
	if err != nil {
		return fmt.Sprintf("cannot read %q: %s", display, pathErrorText(err)), true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Sprintf("cannot read %q: %s", display, pathErrorText(err)), true
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("cannot read %q: not a regular file", display), true
	}
	if err := validateUTF8(file); err != nil {
		return fmt.Sprintf("cannot read %q: %v", display, err), true
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Sprintf("cannot read %q: %v", display, err), true
	}

	window, total, truncatedByBytes, err := readWindow(ctx, file, offset, limit)
	if err != nil {
		return fmt.Sprintf("cannot read %q: %v", display, err), true
	}
	if !truncatedByBytes && offset > total && !(total == 0 && offset == 1) {
		return fmt.Sprintf("offset %d is out of range for %q (%d lines)", offset, display, total), true
	}
	endLine := offset - 1
	if len(window) > 0 {
		endLine = window[len(window)-1].number
	}
	var footer string
	switch {
	case truncatedByBytes:
		footer = fmt.Sprintf("(Output capped. Showing lines %d-%d. Use offset=%d to continue.)", offset, endLine, endLine+1)
	case endLine < total:
		footer = fmt.Sprintf("(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, endLine, total, endLine+1)
	default:
		footer = fmt.Sprintf("(End of file - total %d lines)", total)
	}
	var body strings.Builder
	for i, line := range window {
		if i > 0 {
			body.WriteByte('\n')
		}
		fmt.Fprintf(&body, "%d: %s", line.number, line.text)
	}
	if body.Len() > 0 {
		body.WriteString("\n\n")
	}
	body.WriteString(footer)
	e.markObserved(inv, target)
	return fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s\n</content>", filepath.ToSlash(target), body.String()), false
}

// readWindow scans the whole file for an exact line count while retaining only
// the requested window. A resolved window is capped at 2000 lines, 2000
// characters per line, and 50 KiB of selected text.
func readWindow(ctx context.Context, reader io.Reader, offset, limit int) ([]readLine, int, bool, error) {
	buffered := bufio.NewReaderSize(reader, 64*1024)
	lines := make([]readLine, 0, min(limit, 256))
	total := 0
	outputBytes := 0
	truncatedByBytes := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, total, false, err
		}
		raw, present, err := nextLine(buffered)
		if err != nil {
			return nil, total, false, err
		}
		if !present {
			break
		}
		total++
		if truncatedByBytes || total < offset || len(lines) >= limit {
			continue
		}
		text := truncateJS(raw, readMaxLineLength)
		size := len(text)
		if len(lines) > 0 {
			size++
		}
		if outputBytes+size > readMaxBytes {
			truncatedByBytes = true
			continue
		}
		outputBytes += size
		lines = append(lines, readLine{number: total, text: text})
	}
	return lines, total, truncatedByBytes, nil
}

// nextLine reads one LF-delimited line while retaining at most enough bytes to
// reconstruct the 2000-character truncation point. It keeps draining an
// arbitrarily long line so the total line count stays exact.
func nextLine(reader *bufio.Reader) (string, bool, error) {
	const bufferCap = readMaxLineLength*utf8.UTFMax + utf8.UTFMax
	var line strings.Builder
	present := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			present = true
			if line.Len() < bufferCap {
				remaining := bufferCap - line.Len()
				if len(fragment) > remaining {
					fragment = fragment[:remaining]
				}
				line.Write(fragment)
			}
		}
		switch err {
		case bufio.ErrBufferFull:
			continue
		case nil:
			return strings.TrimSuffix(strings.TrimSuffix(line.String(), "\n"), "\r"), present, nil
		case io.EOF:
			if !present {
				return "", false, nil
			}
			return strings.TrimSuffix(line.String(), "\r"), true, nil
		default:
			return "", false, err
		}
	}
}

// truncateJS truncates text to limit UTF-16 code units, matching the
// JavaScript string semantics the Node implementation uses for its per-line cap.
func truncateJS(text string, limit int) string {
	units := 0
	for index, char := range text {
		width := 1
		if char > 0xFFFF {
			width = 2
		}
		if units+width > limit {
			return text[:index] + fmt.Sprintf("... (line truncated to %d chars)", limit)
		}
		units += width
	}
	return text
}

// validateUTF8 rejects a file whose contents are not valid UTF-8 before any
// window is rendered, matching the Node reader's whole-file decode.
func validateUTF8(reader io.Reader) error {
	buffered := bufio.NewReaderSize(reader, 64*1024)
	var pending []byte
	chunk := make([]byte, 64*1024)
	for {
		count, err := buffered.Read(chunk)
		window := chunk[:count]
		if len(pending) > 0 {
			window = append(pending, window...)
		}
		for len(window) > 0 {
			if !utf8.FullRune(window) {
				break
			}
			char, size := utf8.DecodeRune(window)
			if char == utf8.RuneError && size == 1 {
				return fmt.Errorf("invalid UTF-8 text")
			}
			window = window[size:]
		}
		pending = append(pending[:0], window...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("invalid UTF-8 text")
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
