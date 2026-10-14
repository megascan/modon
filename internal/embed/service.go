package embed

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UnitName is the systemd user unit that keeps the embedding server up.
const UnitName = "modon-embed.service"

// UnitPath is where the user unit lives
// ($XDG_CONFIG_HOME/systemd/user/modon-embed.service).
func UnitPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "systemd", "user", UnitName), nil
}

// UnitInstalled reports whether the user unit file exists.
func UnitInstalled() bool {
	p, err := UnitPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// UnitOptions fills the unit template.
type UnitOptions struct {
	Exe   string // modon binary
	UVDir string // directory holding uv, added to PATH (systemd's PATH is minimal)
	Home  string // paths under it are written as %h so the unit stays portable
}

// Unit renders the systemd user unit. The server restarts whenever it
// stops; when another server already holds the lock it exits and systemd
// retries later, taking over once the other one is gone.
func Unit(o UnitOptions) string {
	h := func(p string) string {
		if o.Home != "" && (p == o.Home || strings.HasPrefix(p, o.Home+"/")) {
			return "%h" + strings.TrimPrefix(p, o.Home)
		}
		return p
	}
	path := "/usr/local/bin:/usr/bin:/bin"
	if o.UVDir != "" {
		path = h(o.UVDir) + ":" + path
	}
	return fmt.Sprintf(`[Unit]
Description=modon embedding server (EmbeddingGemma 2 for semantic 3D-asset search)
Documentation=https://github.com/megascan/modon
After=network.target

[Service]
Type=simple
ExecStart=%s embed-server
Environment=PATH=%s
Restart=always
RestartSec=30
TimeoutStopSec=30
# A background helper: yield CPU to games and editors.
Nice=5

[Install]
WantedBy=default.target
`, h(o.Exe), path)
}
