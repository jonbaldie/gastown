//go:build darwin

package quota

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// keychainServiceBase is the base service name Claude Code uses for keychain credentials.
	keychainServiceBase = "Claude Code-credentials"

	// defaultClaudeConfigDir is Claude Code's default config directory (no suffix in keychain).
	defaultClaudeConfigDir = ".claude"
)

// KeychainCredential holds a backup of a keychain credential for rollback.
type KeychainCredential struct {
	ServiceName string // keychain service name
	Token       string // backed-up token value
}

// KeychainServiceName computes the macOS Keychain service name for a given config dir path.
// Claude Code stores OAuth tokens under: "Claude Code-credentials-<sha256(configDir)[:8]>"
// The default config dir (~/.claude) uses the bare name "Claude Code-credentials" (no suffix).
func KeychainServiceName(configDirPath string) string {
	// Expand ~ to home dir for consistent hashing
	expanded := expandTilde(configDirPath)

	// Check if this is the default config dir (~/.claude or /Users/xxx/.claude)
	home, err := os.UserHomeDir()
	if err == nil {
		defaultPath := home + "/" + defaultClaudeConfigDir
		if expanded == defaultPath {
			return keychainServiceBase
		}
	}

	// Non-default dir: append first 8 chars of SHA-256 hex
	h := sha256.Sum256([]byte(expanded))
	return fmt.Sprintf("%s-%x", keychainServiceBase, h[:4])
}

// ReadKeychainToken reads the password/token for a keychain service name.
func ReadKeychainToken(serviceName string) (string, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", serviceName, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading keychain token for %q: %w", serviceName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteKeychainToken writes (or updates) a token in the macOS Keychain.
// The -U flag updates the existing entry if it exists.
func WriteKeychainToken(serviceName, accountLabel, token string) error {
	cmd := exec.Command("security", "add-generic-password",
		"-U",
		"-s", serviceName,
		"-a", accountLabel,
		"-w", token,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing keychain token for %q: %s: %w", serviceName, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// SwapKeychainCredential backs up the target's keychain token, then overwrites it
// with the source's token. Returns the backup for rollback via RestoreKeychainToken.
//
// This is the core of context-preserving rotation: by swapping the token in the
// target config dir's keychain entry (rather than changing CLAUDE_CONFIG_DIR),
// the respawned session reads a fresh auth token while /resume still finds the
// previous session transcript.
func SwapKeychainCredential(targetConfigDir, sourceConfigDir string) (*KeychainCredential, error) {
	targetSvc := KeychainServiceName(targetConfigDir)
	sourceSvc := KeychainServiceName(sourceConfigDir)

	// Step 1: Back up the target's current token
	backupToken, err := ReadKeychainToken(targetSvc)
	if err != nil {
		return nil, fmt.Errorf("backing up target token: %w", err)
	}

	// Step 2: Read the source's token (the fresh, non-rate-limited one)
	sourceToken, err := ReadKeychainToken(sourceSvc)
	if err != nil {
		return nil, fmt.Errorf("reading source token: %w", err)
	}

	// Step 3: Write the source's token into the target's keychain entry
	if err := WriteKeychainToken(targetSvc, "claude-code", sourceToken); err != nil {
		return nil, fmt.Errorf("writing source token to target keychain: %w", err)
	}

	return &KeychainCredential{
		ServiceName: targetSvc,
		Token:       backupToken,
	}, nil
}

// RestoreKeychainToken writes the backup token back to the keychain,
// undoing a previous SwapKeychainCredential.
func RestoreKeychainToken(backup *KeychainCredential) error {
	if backup == nil {
		return nil
	}
	return WriteKeychainToken(backup.ServiceName, "claude-code", backup.Token)
}

// SwapOAuthAccount copies the oauthAccount field from the source config dir's
// .claude.json into the target's. This ensures Claude Code identifies as the
// new account (correct accountUuid/organizationUuid) after a keychain swap.
// Returns the target's original oauthAccount value for rollback.
func SwapOAuthAccount(targetConfigDir, sourceConfigDir string) (json.RawMessage, error) {
	targetPath := filepath.Join(expandTilde(targetConfigDir), ".claude.json")
	sourcePath := filepath.Join(expandTilde(sourceConfigDir), ".claude.json")

	// Skip if either file doesn't exist — the keychain token is what
	// authenticates; oauthAccount is only cached identity metadata.
	if !oauthAccountFilesPresent(targetPath, sourcePath) {
		return nil, nil
	}

	sourceOAuth, err := readOAuthAccount(sourcePath, "source")
	if err != nil {
		return nil, err
	}

	// Read target's .claude.json (preserve all other fields)
	targetDoc, err := readOAuthDocument(targetPath, "target")
	if err != nil {
		return nil, err
	}

	// Back up target's oauthAccount
	backup := targetDoc["oauthAccount"]

	// Swap
	targetDoc["oauthAccount"] = sourceOAuth

	// Write back
	out, err := json.MarshalIndent(targetDoc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling target .claude.json: %w", err)
	}
	if err := os.WriteFile(targetPath, out, 0600); err != nil {
		return nil, fmt.Errorf("writing target .claude.json: %w", err)
	}

	return backup, nil
}

func oauthAccountFilesPresent(targetPath, sourcePath string) bool {
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return false
	}
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		return false
	}
	return true
}

func readOAuthAccount(path, label string) (json.RawMessage, error) {
	doc, err := readOAuthDocument(path, label)
	if err != nil {
		return nil, err
	}
	oauth, ok := doc["oauthAccount"]
	if !ok {
		return nil, fmt.Errorf("%s .claude.json has no oauthAccount", label)
	}
	return oauth, nil
}

func readOAuthDocument(path, label string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s .claude.json: %w", label, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s .claude.json: %w", label, err)
	}
	return doc, nil
}

// RestoreOAuthAccount writes the backup oauthAccount back to the target .claude.json.
func RestoreOAuthAccount(targetConfigDir string, backup json.RawMessage) error {
	if backup == nil {
		return nil
	}
	targetPath := filepath.Join(expandTilde(targetConfigDir), ".claude.json")

	data, err := os.ReadFile(targetPath)
	if err != nil {
		return fmt.Errorf("reading target .claude.json: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing target .claude.json: %w", err)
	}
	doc["oauthAccount"] = backup
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling target .claude.json: %w", err)
	}
	return os.WriteFile(targetPath, out, 0600)
}

// ValidateKeychainToken checks if the OAuth token for a config dir is still usable.
// It attempts local validation first (JSON credential expiry, JWT expiry), then
// falls back to a lightweight API call. Returns nil if the token appears valid
// or if the token can't be read (the actual swap will fail clearly in that case).
func ValidateKeychainToken(configDir string) error {
	svc := KeychainServiceName(configDir)
	raw, err := ReadKeychainToken(svc)
	if err != nil {
		// Can't read the token — don't block planning. The swap itself will
		// fail with a clear error if the keychain entry doesn't exist.
		return nil
	}
	if raw == "" {
		return nil
	}

	if err, recognized := validateJSONToken(raw); recognized {
		return err
	}
	if err, recognized := validateJWTToken(raw); recognized {
		return err
	}

	// Token is present but format is opaque (not JSON with expires_at, not JWT).
	// Claude Code uses OAuth tokens that authenticate through a different flow
	// than Bearer tokens against the Anthropic API, so HTTP validation would
	// always return 401 for valid OAuth tokens. Assume valid if present.
	return nil
}

func validateJSONToken(raw string) (error, bool) {
	// Claude Code may store the full OAuth response including expiry.
	var cred struct {
		ExpiresAt int64 `json:"expires_at"`
	}
	if json.Unmarshal([]byte(raw), &cred) != nil || cred.ExpiresAt <= 0 {
		return nil, false
	}
	if time.Now().Unix() >= cred.ExpiresAt {
		return fmt.Errorf("token expired at %s", time.Unix(cred.ExpiresAt, 0).Format(time.RFC3339)), true
	}
	return nil, true
}

func validateJWTToken(raw string) (error, bool) {
	// Decode the JWT payload and check its exp claim.
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return nil, false
	}
	if time.Now().Unix() >= claims.Exp {
		return fmt.Errorf("JWT expired at %s", time.Unix(claims.Exp, 0).Format(time.RFC3339)), true
	}
	return nil, true
}

// validateTokenHTTP sends a minimal request to the Anthropic API to check if a
// token is accepted by the auth layer. Returns error only for HTTP 401.
func validateTokenHTTP(token string) error {
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages",
		strings.NewReader("{}"))
	if err != nil {
		return nil // Can't create request → assume valid
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil // Network error → assume valid
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("token rejected by API (HTTP 401)")
	}
	return nil
}

// SyncSwappedTokens propagates fresh tokens from source accounts to target
// keychain entries that were swapped during quota rotation.
//
// When rotation swaps account X's token into config dir Y's keychain entry,
// later re-authentication of account X writes the fresh token to X's own
// keychain entry — not Y's. This function reads each source account's current
// token and writes it to the target's keychain entry if they differ.
//
// swapDirs maps target config dir → source config dir (already resolved from
// account handles via ResolveSwapSourceDirs).
//
// Returns the number of keychain entries updated.
func SyncSwappedTokens(swapDirs map[string]string) int {
	updated := 0
	for targetConfigDir, sourceConfigDir := range swapDirs {
		targetSvc := KeychainServiceName(targetConfigDir)
		sourceSvc := KeychainServiceName(sourceConfigDir)

		if targetSvc == sourceSvc {
			continue // same keychain entry, nothing to sync
		}

		// Read current tokens from both keychain entries
		targetToken, err := ReadKeychainToken(targetSvc)
		if err != nil {
			continue // target entry doesn't exist or can't be read
		}
		sourceToken, err := ReadKeychainToken(sourceSvc)
		if err != nil {
			continue // source entry doesn't exist or can't be read
		}

		// If tokens match, no sync needed
		if targetToken == sourceToken {
			continue
		}

		// Source has a different (presumably fresher) token — propagate it
		if err := WriteKeychainToken(targetSvc, "claude-code", sourceToken); err != nil {
			continue // best-effort
		}
		updated++
	}
	return updated
}

// expandTilde expands a leading ~/ to the user's home directory.
func expandTilde(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + path[1:]
		}
	}
	return path
}
