// Package panel serves the LAN-only control panel: roles with model pinning,
// agent cards, active tasks, quota, and a live view of each run's
// transcript (Go templates, htmx, xterm.js; everything embedded).
//
// Milestone M3b in the architecture roadmap. Interactive terminals (Take
// control), ad-hoc sessions and needs-input detection come next: they need
// runs started inside a pseudo-terminal.
package panel
