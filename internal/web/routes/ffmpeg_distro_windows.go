package routes

// suggestFFmpegInstall is unused on Windows where the Chocolatey/winget
// install endpoints handle FFmpeg setup. Kept as a stub so the
// /api/ffmpeg/install-suggestion endpoint can be wired uniformly.
func suggestFFmpegInstall() string {
	return ""
}
