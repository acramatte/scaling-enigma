package ingestion

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	appdb "semantic-search/internal/database"
)

// ClassifyMedia classifies an object from S3 event metadata before download.
func ClassifyMedia(contentType, objectKey string) string {
	return appdb.ClassifyMediaMetadata(contentType, objectKey)
}

// VerifyImageMedia confirms that the downloaded object's S3 content type and
// payload bytes represent a supported raster image.
func VerifyImageMedia(s3ContentType, objectKey string, payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", PermanentMediaError(errors.New("image object is empty"))
	}

	if normalized := normalizeMIME(s3ContentType); normalized != "" && !isGenericMIME(normalized) {
		declaredClass := appdb.ClassifyMediaMetadata(normalized, "")
		if declaredClass != appdb.IngestionMediaClassImage {
			return "", PermanentMediaError(fmt.Errorf(
				"media classification mismatch for %q: expected image, storage content type %q classified as %s",
				objectKey, normalized, declaredClass,
			))
		}
	}

	sniffedClass, sniffedMIME := SniffMedia(payload)
	if sniffedClass != appdb.IngestionMediaClassImage {
		return "", PermanentMediaError(fmt.Errorf(
			"media verification failed for %q: expected image, payload detected as %s (%s)",
			objectKey, sniffedClass, sniffedMIME,
		))
	}
	return sniffedMIME, nil
}

// VerifyVideoMedia confirms that an object's S3 content type and header bytes
// represent a supported video container.
func VerifyVideoMedia(s3ContentType, objectKey string, header []byte) (string, error) {
	if len(header) == 0 {
		return "", PermanentMediaError(errors.New("video object is empty"))
	}

	if normalized := normalizeMIME(s3ContentType); normalized != "" && !isGenericMIME(normalized) {
		declaredClass := appdb.ClassifyMediaMetadata(normalized, "")
		if declaredClass != appdb.IngestionMediaClassVideo {
			return "", PermanentMediaError(fmt.Errorf(
				"media classification mismatch for %q: expected video, storage content type %q classified as %s",
				objectKey, normalized, declaredClass,
			))
		}
	}

	sniffedClass, sniffedMIME := SniffMedia(header)
	if sniffedClass != appdb.IngestionMediaClassVideo {
		return "", PermanentMediaError(fmt.Errorf(
			"media verification failed for %q: expected video, payload detected as %s (%s)",
			objectKey, sniffedClass, sniffedMIME,
		))
	}
	return sniffedMIME, nil
}

// SniffMedia inspects magic bytes from the start of a payload to determine its
// actual media class and MIME type independent of object key or upload headers.
func SniffMedia(payload []byte) (mediaClass string, contentType string) {
	if len(payload) == 0 {
		return appdb.IngestionMediaClassUnsupported, "application/octet-stream"
	}

	if mimeType, class, ok := matchMagicSignature(payload); ok {
		return class, mimeType
	}

	detected := normalizeMIME(http.DetectContentType(payload))
	return appdb.ClassifyMediaMetadata(detected, ""), detected
}

func matchMagicSignature(b []byte) (mimeType string, mediaClass string, ok bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg", appdb.IngestionMediaClassImage, true
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png", appdb.IngestionMediaClassImage, true
	case len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a"))):
		return "image/gif", appdb.IngestionMediaClassImage, true
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp", appdb.IngestionMediaClassImage, true
	case len(b) >= 2 && b[0] == 'B' && b[1] == 'M':
		return "image/bmp", appdb.IngestionMediaClassImage, true
	case len(b) >= 4 && (bytes.Equal(b[:4], []byte{'I', 'I', 0x2A, 0x00}) || bytes.Equal(b[:4], []byte{'M', 'M', 0x00, 0x2A})):
		return "image/tiff", appdb.IngestionMediaClassImage, true
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("AVI ")):
		return "video/x-msvideo", appdb.IngestionMediaClassVideo, true
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "video/webm", appdb.IngestionMediaClassVideo, true
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{'F', 'L', 'V', 0x01}):
		return "video/x-flv", appdb.IngestionMediaClassVideo, true
	case len(b) >= 4 && (bytes.Equal(b[:4], []byte{0x00, 0x00, 0x01, 0xBA}) || bytes.Equal(b[:4], []byte{0x00, 0x00, 0x01, 0xB3})):
		return "video/mpeg", appdb.IngestionMediaClassVideo, true
	case len(b) >= 4 && bytes.Equal(b[:4], []byte("OggS")):
		return "video/ogg", appdb.IngestionMediaClassVideo, true
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")):
		brand := string(b[8:12])
		switch brand {
		case "avif", "avis":
			return "image/avif", appdb.IngestionMediaClassImage, true
		case "heic", "heix", "hevc", "hevx", "mif1", "msf1":
			return "image/heic", appdb.IngestionMediaClassImage, true
		case "qt  ":
			return "video/quicktime", appdb.IngestionMediaClassVideo, true
		case "isom", "iso2", "mp41", "mp42", "M4V ", "avc1", "mp71", "f4v ", "MSNV":
			return "video/mp4", appdb.IngestionMediaClassVideo, true
		default:
			if strings.HasPrefix(brand, "3gp") || strings.HasPrefix(brand, "3g2") {
				return "video/3gpp", appdb.IngestionMediaClassVideo, true
			}
		}
	}
	return "", "", false
}

func normalizeMIME(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if before, _, found := strings.Cut(trimmed, ";"); found {
		trimmed = strings.TrimSpace(before)
	}
	return strings.ToLower(trimmed)
}

func isGenericMIME(mimeType string) bool {
	return mimeType == "application/octet-stream" || mimeType == "binary/octet-stream"
}
