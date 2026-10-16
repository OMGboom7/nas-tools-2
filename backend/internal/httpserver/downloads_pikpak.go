package httpserver

import "github.com/0xforee/nas-tools/backend/internal/pikpak"

// SavePath is the existing PikPak parent folder ID, not a NAS filesystem path.
// Unsupported settings must never be silently dropped before a cloud upload.
func pikpakDownloadOptionsSupported(options downloadAddOptions) bool {
	parent := options.SavePath
	options.SavePath = ""
	return options.empty() && !options.ApplyTransmissionLimits && (parent == "" || pikpak.ValidID(parent))
}
