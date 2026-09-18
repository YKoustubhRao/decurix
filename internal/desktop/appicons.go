package desktop

import "strings"

// appSVGIcons maps app slugs/identifiers to clean, compact vector SVGs.
// These are written directly into the user's .local/share/icons/hicolor/scalable/apps/
// inside the migration tar archive so XFCE displays official, crisp logos instantly
// without missing icon warnings or recursive theme directory searches.
var appSVGIcons = map[string]string{
	"microsoft-edge": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <defs>
    <radialGradient id="e1" cx="38.5" cy="11.5" r="28" gradientUnits="userSpaceOnUse">
      <stop offset="0%" stop-color="#0078d7"/>
      <stop offset="100%" stop-color="#002050"/>
    </radialGradient>
    <linearGradient id="e2" x1="12" y1="8" x2="36" y2="44" gradientUnits="userSpaceOnUse">
      <stop offset="0%" stop-color="#22d3ee"/>
      <stop offset="50%" stop-color="#0284c7"/>
      <stop offset="100%" stop-color="#0369a1"/>
    </linearGradient>
    <linearGradient id="e3" x1="8" y1="20" x2="28" y2="40" gradientUnits="userSpaceOnUse">
      <stop offset="0%" stop-color="#34d399"/>
      <stop offset="100%" stop-color="#059669"/>
    </linearGradient>
  </defs>
  <path fill="url(#e1)" d="M42 22.5c0 10.77-8.73 19.5-19.5 19.5-6.52 0-12.3-3.2-15.84-8.15 3.37 1.34 7.21 2.08 11.23 2.08 8.64 0 15.65-7.01 15.65-15.65 0-3.35-1.05-6.45-2.85-9 6.8 2.39 11.31 7.9 11.31 11.22z"/>
  <path fill="url(#e2)" d="M22.5 6C13.39 6 6 13.39 6 22.5c0 3.84 1.31 7.37 3.51 10.18C12.3 27.67 17.5 24 23.5 24c7.45 0 13.5 6.05 13.5 13.5 0 .9-.09 1.78-.26 2.63C40.67 36.33 43 31.69 43 26.5 43 15.18 33.82 6 22.5 6z"/>
  <path fill="url(#e3)" d="M6 24c0 9.94 8.06 18 18 18 3.5 0 6.77-1 9.55-2.72-2.12-.86-4.47-1.35-6.94-1.35-7.24 0-13.11-5.87-13.11-13.11 0-2.8.88-5.39 2.38-7.53C10.5 19.16 6 21.2 6 24z"/>
</svg>`,

	"youtube": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <rect width="44" height="32" x="2" y="8" rx="8" ry="8" fill="#ff0000"/>
  <polygon points="19,16 33,24 19,32" fill="#ffffff"/>
</svg>`,

	"google-chrome": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <circle cx="24" cy="24" r="21" fill="#ffffff"/>
  <path fill="#ea4335" d="M24 3a21 21 0 0 1 18.18 10.5H24v10.5h-9.09L8.43 11.83A20.9 20.9 0 0 1 24 3z"/>
  <path fill="#fbbc05" d="M3.78 18.78A21 21 0 0 1 24 3l6.48 10.5H14.91L5.82 32.55a21.1 21.1 0 0 1-2.04-13.77z"/>
  <path fill="#34a853" d="M24 45a21 21 0 0 1-18.18-10.5L14.91 24l5.24 9.07L24 45z"/>
  <path fill="#4285f4" d="M42.18 13.5A21 21 0 0 1 24 45l6.48-11.23L42.18 13.5z"/>
  <circle cx="24" cy="24" r="9.5" fill="#ffffff"/>
  <circle cx="24" cy="24" r="7.5" fill="#1a73e8"/>
</svg>`,

	"firefox": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <circle cx="24" cy="24" r="21" fill="#ff7139"/>
  <circle cx="24" cy="24" r="14" fill="#6c31e3"/>
  <path fill="#ffb400" d="M24 5c10.5 0 19 8.5 19 19 0 7-3.8 13.1-9.5 16.4C37 36 39 30.5 39 25c0-8.3-6.7-15-15-15-2.5 0-4.9.6-7 1.7C19.2 8.7 21.5 5 24 5z"/>
  <circle cx="24" cy="24" r="7" fill="#00ffff" opacity=".8"/>
</svg>`,

	"vscode": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <path fill="#0065a9" d="M36.5 3.5 25.8 13.7 13.4 4.2 3.8 8.8v30.4l9.6 4.6 12.4-9.5 10.7 10.2 9.5-4.6V8.1z"/>
  <path fill="#007acc" d="M36.5 3.5 24.5 15.5l5.5 5.2 6.5-6.2z"/>
  <path fill="#1f9cf0" d="M36.5 44.5 24.5 32.5l5.5-5.2 6.5 6.2z"/>
  <path fill="#0065a9" d="m3.8 8.8 20.7 15.2L3.8 39.2z"/>
</svg>`,

	"claude": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <rect width="44" height="44" x="2" y="2" rx="10" ry="10" fill="#d97757"/>
  <path fill="#ffffff" d="M24 10a14 14 0 1 0 14 14A14 14 0 0 0 24 10zm-2 22-5-5 2.1-2.1L22 27.8l6.9-6.9 2.1 2.1z"/>
</svg>`,

	"chatgpt": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <rect width="44" height="44" x="2" y="2" rx="10" ry="10" fill="#10a37f"/>
  <circle cx="24" cy="24" r="13" fill="none" stroke="#ffffff" stroke-width="4"/>
  <circle cx="24" cy="24" r="5" fill="#ffffff"/>
</svg>`,

	"github": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <circle cx="24" cy="24" r="21" fill="#24292e"/>
  <path fill="#ffffff" d="M24 9.5a14.5 14.5 0 0 0-4.58 28.26c.72.13 1-.31 1-.7v-2.47c-4.04.88-4.9-1.95-4.9-1.95-.66-1.68-1.61-2.13-1.61-2.13-1.32-.9.1-.88.1-.88 1.46.1 2.23 1.5 2.23 1.5 1.3 2.22 3.4 1.58 4.23 1.21.13-.94.51-1.58.93-1.94-3.23-.37-6.62-1.61-6.62-7.18 0-1.59.57-2.89 1.5-3.91-.15-.37-.65-1.85.14-3.85 0 0 1.22-.39 4 .1.5-.07 1-.1 1.5-.1s1 .03 1.5.1c2.78-.49 4-.1 4-.1.79 2 .29 3.48.14 3.85.93 1.02 1.5 2.32 1.5 3.91 0 5.58-3.4 6.81-6.64 7.17.52.45.99 1.34.99 2.7v4.02c0 .39.26.84 1 .68A14.5 14.5 0 0 0 24 9.5z"/>
</svg>`,

	"slack": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <rect width="44" height="44" x="2" y="2" rx="10" ry="10" fill="#4a154b"/>
  <circle cx="16" cy="18" r="4" fill="#36c5f0"/>
  <circle cx="30" cy="16" r="4" fill="#2eb67d"/>
  <circle cx="32" cy="30" r="4" fill="#e01e5a"/>
  <circle cx="18" cy="32" r="4" fill="#ecb22e"/>
</svg>`,

	"discord": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <circle cx="24" cy="24" r="21" fill="#5865f2"/>
  <path fill="#ffffff" d="M33.8 15.5a18.7 18.7 0 0 0-4.6-1.4.1.1 0 0 0-.1.05c-.2.36-.43.83-.59 1.2a17.3 17.3 0 0 0-5.2 0c-.16-.38-.4-.84-.6-1.2a.1.1 0 0 0-.1-.05 18.7 18.7 0 0 0-4.6 1.4.1.1 0 0 0-.05.04C13.2 23.3 12 30.8 12.6 38.3a.1.1 0 0 0 .04.07 18.8 18.8 0 0 0 5.7 2.9.1.1 0 0 0 .1-.04c.45-.61.85-1.26 1.2-1.94a.1.1 0 0 0-.06-.14 12.3 12.3 0 0 1-1.75-.84.1.1 0 0 1 0-.17c.12-.09.24-.18.35-.27a.1.1 0 0 1 .1 0 13.4 13.4 0 0 0 11.5 0 .1.1 0 0 1 .1 0c.11.09.23.18.35.27a.1.1 0 0 1 0 .17 12.5 12.5 0 0 1-1.76.84.1.1 0 0 0-.05.14c.36.68.76 1.33 1.2 1.94a.1.1 0 0 0 .1.04 18.8 18.8 0 0 0 5.7-2.9.1.1 0 0 0 .04-.07c.72-8.7-1.23-16.14-5.32-22.75a.1.1 0 0 0-.05-.04zM20.2 30.5c-1.3 0-2.4-1.2-2.4-2.6 0-1.5 1.1-2.6 2.4-2.6 1.4 0 2.4 1.2 2.4 2.6 0 1.5-1 2.6-2.4 2.6zm7.6 0c-1.3 0-2.4-1.2-2.4-2.6 0-1.5 1.1-2.6 2.4-2.6 1.4 0 2.4 1.2 2.4 2.6 0 1.5-1 2.6-2.4 2.6z"/>
</svg>`,

	"spotify": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <circle cx="24" cy="24" r="21" fill="#1ed760"/>
  <path fill="#ffffff" d="M33.2 29.8a1.2 1.2 0 0 1-1.7.4c-4.6-2.8-10.4-3.4-17.2-1.9a1.2 1.2 0 1 1-.5-2.4c7.4-1.7 13.8-1 19 2.2.6.4.8 1.1.4 1.7zm2.5-5.5a1.5 1.5 0 0 1-2.1.5c-5.3-3.2-13.4-4.2-19.6-2.3a1.5 1.5 0 0 1-.9-2.9c7.2-2.2 16.1-1.1 22.1 2.6.7.4.9 1.4.5 2.1zm.2-5.7c-6.3-3.8-16.8-4.1-22.9-2.3a1.8 1.8 0 1 1-1-3.5c7.2-2.2 18.8-1.8 26.1 2.5a1.8 1.8 0 0 1-2.2 3.3z"/>
</svg>`,

	"utilities-terminal": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">
  <rect width="44" height="34" x="2" y="7" rx="6" ry="6" fill="#1e1e24"/>
  <path fill="#22c55e" d="m11 16 7 6-7 6-2-2 4.7-4L9 18z"/>
  <rect width="10" height="3" x="21" y="27" fill="#f8fafc" rx="1.5"/>
</svg>`,
}

// getAppSVGIcon returns the embedded SVG icon for an app by slug or name, or empty string.
func getAppSVGIcon(appNameOrSlug string) (iconSlug string, svgContent string) {
	lower := strings.ToLower(appNameOrSlug)
	switch {
	case strings.Contains(lower, "edge"):
		return "microsoft-edge", appSVGIcons["microsoft-edge"]
	case strings.Contains(lower, "youtube"):
		return "youtube", appSVGIcons["youtube"]
	case strings.Contains(lower, "chrome"):
		return "google-chrome", appSVGIcons["google-chrome"]
	case strings.Contains(lower, "firefox"):
		return "firefox", appSVGIcons["firefox"]
	case strings.Contains(lower, "code") || strings.Contains(lower, "studio"):
		return "vscode", appSVGIcons["vscode"]
	case strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic"):
		return "claude", appSVGIcons["claude"]
	case strings.Contains(lower, "chatgpt") || strings.Contains(lower, "openai"):
		return "chatgpt", appSVGIcons["chatgpt"]
	case strings.Contains(lower, "github"):
		return "github", appSVGIcons["github"]
	case strings.Contains(lower, "slack"):
		return "slack", appSVGIcons["slack"]
	case strings.Contains(lower, "discord"):
		return "discord", appSVGIcons["discord"]
	case strings.Contains(lower, "spotify"):
		return "spotify", appSVGIcons["spotify"]
	case strings.Contains(lower, "terminal") || strings.Contains(lower, "bash"):
		return "utilities-terminal", appSVGIcons["utilities-terminal"]
	default:
		return "", ""
	}
}
