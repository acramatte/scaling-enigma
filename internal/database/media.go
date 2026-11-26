package database

import (
	"mime"
	"path"
	"strings"
)

var imageExtensions = map[string]struct{}{
	".jpg":  {},
	".jpeg": {},
	".png":  {},
	".gif":  {},
	".webp": {},
	".bmp":  {},
	".tif":  {},
	".tiff": {},
	".avif": {},
	".heic": {},
	".heif": {},
}

var videoExtensions = map[string]struct{}{
	".mp4":  {},
	".mov":  {},
	".m4v":  {},
	".avi":  {},
	".mkv":  {},
	".webm": {},
	".mpeg": {},
	".mpg":  {},
	".ts":   {},
	".mts":  {},
	".m2ts": {},
	".wmv":  {},
	".flv":  {},
	".3gp":  {},
	".ogv":  {},
}

// ClassifyMediaMetadata maps S3 notification metadata (content type and object
// key) to an ingestion media class. Explicit non-generic content types take
// precedence over extensions; generic binary types fall back to the extension.
func ClassifyMediaMetadata(contentType, objectKey string) string {
	mediaType := normalizeContentType(contentType)
	if mediaType != "" && !isGenericContentType(mediaType) {
		return classifyNormalizedMediaType(mediaType)
	}

	ext := strings.ToLower(path.Ext(strings.TrimSpace(objectKey)))
	if ext == "" {
		return IngestionMediaClassUnsupported
	}
	if _, ok := imageExtensions[ext]; ok {
		return IngestionMediaClassImage
	}
	if _, ok := videoExtensions[ext]; ok {
		return IngestionMediaClassVideo
	}
	if detected := normalizeContentType(mime.TypeByExtension(ext)); detected != "" {
		return classifyNormalizedMediaType(detected)
	}
	return IngestionMediaClassUnsupported
}

func IsValidMediaClass(mediaClass string) bool {
	switch mediaClass {
	case IngestionMediaClassImage, IngestionMediaClassVideo, IngestionMediaClassUnsupported:
		return true
	default:
		return false
	}
}

func IsValidFailureKind(failureKind string) bool {
	switch failureKind {
	case IngestionFailureRetryable, IngestionFailurePermanentMedia, IngestionFailurePermanentLimit:
		return true
	default:
		return false
	}
}

func normalizeContentType(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if before, _, found := strings.Cut(trimmed, ";"); found {
		trimmed = strings.TrimSpace(before)
	}
	return strings.ToLower(trimmed)
}

func isGenericContentType(mediaType string) bool {
	return mediaType == "application/octet-stream" || mediaType == "binary/octet-stream"
}

func classifyNormalizedMediaType(mediaType string) string {
	switch {
	case mediaType == "image/svg+xml":
		return IngestionMediaClassUnsupported
	case strings.HasPrefix(mediaType, "image/"):
		return IngestionMediaClassImage
	case strings.HasPrefix(mediaType, "video/"):
		return IngestionMediaClassVideo
	default:
		return IngestionMediaClassUnsupported
	}
}
