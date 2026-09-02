package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsendz/anteroom/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoadConfigSingleRoomModeNeedsOnlyAnOrigin(t *testing.T) {
	// The whole promise of single-room mode: one flag is enough to start,
	// including the secrets, which are generated rather than demanded.
	cfg, err := loadConfig("", "http://localhost:3000", 12, 40, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	room, ok := cfg.Rooms["main"]
	if !ok {
		t.Fatalf("rooms = %v, want a room called main", cfg.Rooms)
	}
	if room.Origin != "http://localhost:3000" {
		t.Errorf("origin = %q, want http://localhost:3000", room.Origin)
	}
	if room.Rate != 12 || room.MaxActive != 40 {
		t.Errorf("rate=%v max_active=%d, want the flag values 12 and 40", room.Rate, room.MaxActive)
	}
	if cfg.CookieSecret == "" || cfg.AdminToken == "" {
		t.Error("secrets were not generated, so single-room mode would refuse to start")
	}
}

func TestLoadConfigRejectsConfigAndOriginTogether(t *testing.T) {
	// Silently preferring one would leave an operator convinced their flag
	// took effect when it did not.
	_, err := loadConfig("anteroom.yaml", "http://localhost:3000", 5, 100, discardLogger())
	if err == nil {
		t.Fatal("passing both --config and --origin was accepted")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %q, want it to say the two flags conflict", err)
	}
}

func TestLoadConfigWithNothingToProtectSaysHowToFixIt(t *testing.T) {
	_, err := loadConfig("", "", 5, 100, discardLogger())
	if err == nil {
		t.Fatal("running with no origin and no config was accepted")
	}
	if !strings.Contains(err.Error(), "--origin") || !strings.Contains(err.Error(), "--config") {
		t.Errorf("error = %q, want it to name both ways out", err)
	}
}

func TestLoadConfigRejectsANonAbsoluteOrigin(t *testing.T) {
	// Validation has to run in single-room mode too, not just for files.
	if _, err := loadConfig("", "localhost:3000", 5, 100, discardLogger()); err == nil {
		t.Fatal("an origin with no scheme was accepted")
	}
}

func TestLoadConfigReadsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anteroom.yaml")
	body := `
listen: ":9999"
cookie_secret: "0123456789abcdef0123456789abcdef"
admin_token: "0123456789abcdef"
rooms:
  shop:
    origin: "http://localhost:3000"
    rate: 25
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path, "", 5, 100, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen = %q, want :9999", cfg.Listen)
	}
	if got := cfg.Rooms["shop"].Rate; got != 25 {
		t.Errorf("rate = %v, want 25 from the file", got)
	}
}

func TestGeneratedSecretsAreFreshAndNotOverwritten(t *testing.T) {
	first := config.Default()
	if err := applyGeneratedSecrets(&first, discardLogger()); err != nil {
		t.Fatal(err)
	}
	second := config.Default()
	if err := applyGeneratedSecrets(&second, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if first.CookieSecret == second.CookieSecret || first.AdminToken == second.AdminToken {
		t.Error("two runs generated identical secrets, so they are not random")
	}

	// An operator who supplied their own must keep it: regenerating would send
	// every queued visitor to the back of the line on restart.
	supplied := config.Default()
	supplied.CookieSecret = "mine-and-stable"
	supplied.AdminToken = "also-mine"
	if err := applyGeneratedSecrets(&supplied, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if supplied.CookieSecret != "mine-and-stable" || supplied.AdminToken != "also-mine" {
		t.Error("a configured secret was overwritten by a generated one")
	}
}

// `anteroom init` is the documented way to start a real deployment, so its
// output has to be loadable without editing anything but the origin.
func TestGeneratedExampleConfigLoads(t *testing.T) {
	secret, err := randomHex(32)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "anteroom.yaml")
	body := fmt.Sprintf(config.Example, secret, adminToken)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the config `anteroom init` writes does not load: %v", err)
	}
	if cfg.CookieSecret != secret || cfg.AdminToken != adminToken {
		t.Error("the generated secrets did not survive the round trip")
	}
	if len(cfg.Rooms) == 0 {
		t.Error("the example config defines no rooms, so it would not start")
	}
}

// The checked-in example and the one `anteroom init` generates are two copies
// of the same document; this catches an edit to one and not the other.
func TestCheckedInExampleMatchesTheGeneratedOne(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "anteroom.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	generated := fmt.Sprintf(config.Example, "PLACEHOLDER-SECRET", "PLACEHOLDER-TOKEN")

	if normaliseSecrets(string(onDisk)) != normaliseSecrets(generated) {
		t.Error("anteroom.example.yaml and config.Example have drifted apart; " +
			"update both so `anteroom init` and the checked-in sample agree")
	}
}

// normaliseSecrets blanks the two lines that legitimately differ: the file on
// disk carries a CHANGE-ME placeholder where the generated one has a real key.
func normaliseSecrets(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		for _, field := range []string{"cookie_secret:", "admin_token:"} {
			if strings.HasPrefix(strings.TrimSpace(line), field) {
				lines[i] = field
			}
		}
	}
	return strings.Join(lines, "\n")
}
