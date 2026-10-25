package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	imageMaxBytes     = 20 * 1024 * 1024
	imageMaxPixels    = 64_000_000
	imageMaxDimension = 8192
)

var imageExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

func imageMediaTypeForPath(path string) string {
	return imageExtensions[strings.ToLower(filepath.Ext(path))]
}

func sniffImageMediaType(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// readImage decodes a supported image, normalizes it within the deployment's
// pixel/dimension/byte budgets, stores it content-addressed, and returns both a
// text envelope and the durable image block the model/UI consumes.
func (e *ToolExecutor) readImage(ctx context.Context, inv ToolInvocation, args map[string]any) ToolOutcome {
	filePath, ok := stringArg(args, "file_path")
	if !ok || strings.TrimSpace(filePath) == "" {
		return ToolOutcome{Text: "file_path must be a non-empty string", IsError: true}
	}
	extension := strings.ToLower(filepath.Ext(filePath))
	declared := imageMediaTypeForPath(filePath)
	if declared == "" && extension != "" {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: the %s extension does not declare a supported image format; read_image accepts PNG/JPEG/WebP/GIF files, including extension-less files in those formats", filePath, extension), IsError: true}
	}
	target, display, err := e.resolvePath(inv, filePath, false)
	if err != nil {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: %s", display, pathErrorText(err)), IsError: true}
	}
	info, err := os.Stat(target)
	if err != nil {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: %s", display, pathErrorText(err)), IsError: true}
	}
	if !info.Mode().IsRegular() {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: not a regular file", display), IsError: true}
	}
	if info.Size() > imageMaxBytes {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: the image cannot be stored within the deployment's byte limits; downscale the image and read the smaller copy", display), IsError: true}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: %s", display, pathErrorText(err)), IsError: true}
	}
	if err := ctx.Err(); err != nil {
		return ToolOutcome{Text: "cancelled", IsError: true}
	}
	mediaType := declared
	if mediaType == "" {
		mediaType = sniffImageMediaType(data)
		if mediaType == "" {
			return ToolOutcome{Text: fmt.Sprintf("cannot read %q: the file content is not a supported image format; read_image accepts PNG/JPEG/WebP/GIF", display), IsError: true}
		}
	}
	if sniff := sniffImageMediaType(data); sniff == "" {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt", display), IsError: true}
	} else if sniff != mediaType {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: the %s extension declares %s, but the bytes use a different image format; rename the file to match its actual format if it is PNG/JPEG/WebP/GIF, or convert it to one of those formats", display, extension, mediaType), IsError: true}
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt", display), IsError: true}
	}
	width, height := cfg.Width, cfg.Height
	if width <= 0 || height <= 0 {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: unsupported or malformed image data", display), IsError: true}
	}
	originalWidth, originalHeight := width, height
	limitWidth, limitHeight := imageFitDimensions(width, height, imageMaxPixels, imageMaxDimension)
	needsScale := limitWidth != width || limitHeight != height
	var normalized []byte
	outType := mediaType
	outWidth, outHeight := width, height
	if needsScale {
		src, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return ToolOutcome{Text: fmt.Sprintf("cannot read %q: the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt", display), IsError: true}
		}
		scaled := scaleImage(src, limitWidth, limitHeight)
		outWidth, outHeight = scaled.Bounds().Dx(), scaled.Bounds().Dy()
		normalized, outType, err = encodeImageWithinBudget(scaled, mediaType)
		if err != nil {
			return ToolOutcome{Text: fmt.Sprintf("cannot read %q: %v", display, err), IsError: true}
		}
	} else {
		normalized = data
	}
	attachmentID, err := e.storeAttachment(normalized)
	if err != nil {
		return ToolOutcome{Text: fmt.Sprintf("cannot read %q: %v", display, err), IsError: true}
	}
	name := filepath.Base(display)
	ref := map[string]any{
		"attachmentId": attachmentID,
		"mediaType":    outType,
		"bytes":        len(normalized),
		"width":        outWidth,
		"height":       outHeight,
		"name":         name,
	}
	if needsScale {
		ref["originalDimensions"] = map[string]any{"width": originalWidth, "height": originalHeight}
	}
	var scaled string
	if needsScale {
		x := fmt.Sprintf("%.2f", float64(originalWidth)/float64(outWidth))
		y := fmt.Sprintf("%.2f", float64(originalHeight)/float64(outHeight))
		advice := "multiply coordinates by " + x
		if x != y {
			advice = "multiply x coordinates by " + x + " and y coordinates by " + y
		}
		scaled = fmt.Sprintf(" (downscaled from %dx%d px; %s to locate features in the original file)", originalWidth, originalHeight, advice)
	}
	envelope := fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\n%s image, %dx%d px, %d bytes%s\n</content>",
		filepath.ToSlash(target), outType, outWidth, outHeight, len(normalized), scaled)
	e.markObserved(inv, target)
	return ToolOutcome{
		Text: envelope,
		Content: []any{
			map[string]any{"type": "text", "text": envelope},
			map[string]any{"type": "image", "attachment": ref},
		},
		Meta:    map[string]any{"path": filepath.ToSlash(target)},
		Present: map[string]any{"card": "generic", "title": "Read image " + filePath, "kind": "read", "locations": []any{map[string]any{"path": filePath}}},
	}
}

func imageFitDimensions(width, height, maxPixels, maxDimension int) (int, int) {
	w, h := float64(width), float64(height)
	if float64(width)*float64(height) > float64(maxPixels) {
		scale := sqrt(float64(maxPixels) / (float64(width) * float64(height)))
		w, h = w*scale, h*scale
	}
	longEdge := w
	if h > longEdge {
		longEdge = h
	}
	if longEdge > float64(maxDimension) {
		scale := float64(maxDimension) / longEdge
		w, h = w*scale, h*scale
	}
	nw, nh := int(w), int(h)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

func sqrt(value float64) float64 {
	if value <= 0 {
		return 0
	}
	x := value
	for i := 0; i < 40; i++ {
		x = (x + value/x) / 2
	}
	return x
}

func scaleImage(src image.Image, width, height int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

func encodeImageWithinBudget(img image.Image, sourceType string) ([]byte, string, error) {
	opaque := imageIsOpaque(img)
	for attempt := 0; ; attempt++ {
		if !opaque && (sourceType == "image/png" || sourceType == "image/gif" || sourceType == "image/webp") {
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err == nil && buf.Len() <= imageMaxBytes {
				return buf.Bytes(), "image/png", nil
			}
		}
		for _, quality := range []int{85, 75, 60} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err == nil && buf.Len() <= imageMaxBytes {
				return buf.Bytes(), "image/jpeg", nil
			}
		}
		if attempt >= 4 {
			return nil, "", fmt.Errorf("the image cannot be stored within the deployment's byte limits; downscale the image and read the smaller copy")
		}
		bounds := img.Bounds()
		width := bounds.Dx() * 3 / 4
		height := bounds.Dy() * 3 / 4
		if width < 1 {
			width = 1
		}
		if height < 1 {
			height = 1
		}
		img = scaleImage(img, width, height)
	}
}

func imageIsOpaque(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y += 3 {
		for x := bounds.Min.X; x < bounds.Max.X; x += 3 {
			_, _, _, alpha := img.At(x, y).RGBA()
			if alpha != 0xffff {
				return false
			}
		}
	}
	return true
}

func (e *ToolExecutor) storeAttachment(data []byte) (string, error) {
	home := e.dshHome
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".dsh-go")
	}
	digest := sha256.Sum256(data)
	encoded := hex.EncodeToString(digest[:])
	dir := filepath.Join(home, "attachments", "v1", "objects", encoded[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, encoded)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return "sha256:" + encoded, nil
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return "sha256:" + encoded, nil
}

// loadAttachment reads one stored attachment from this executor's DSH home.
func (e *ToolExecutor) loadAttachment(attachmentID string) ([]byte, error) {
	home := e.dshHome
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".dsh-go")
	}
	return loadAttachment(home, attachmentID)
}

// LoadStoredAttachment reads one attachment from a DSH home. It is exported
// for the attachment RPC, which serves the same content-addressed store.
func LoadStoredAttachment(home, attachmentID string) ([]byte, error) {
	return loadAttachment(home, attachmentID)
}

func loadAttachment(home, attachmentID string) ([]byte, error) {
	digest := strings.TrimPrefix(attachmentID, "sha256:")
	if len(digest) != 64 {
		return nil, fmt.Errorf("invalid attachment id")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, fmt.Errorf("invalid attachment id")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".dsh-go")
	}
	return os.ReadFile(filepath.Join(home, "attachments", "v1", "objects", digest[:2], digest))
}

// imageConfig reads only the dimensions/type needed to answer attachment RPCs.
func imageConfig(data []byte) (string, int, int) {
	mediaType := sniffImageMediaType(data)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return mediaType, 0, 0
	}
	return mediaType, cfg.Width, cfg.Height
}
