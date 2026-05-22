package hosts

// Host module P1c.4 — coder credential vault.
//
// Each host_skills row of kind=coder can carry a set of named
// credentials (e.g. "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
// "MOONSHOT_API_KEY") that polar-agent will inject as env vars when
// it spawns the CLI for a skill.start call. Encryption-at-rest is
// AES-256-GCM keyed off POLAR_CREDENTIAL_KEY (parallel to
// IOSDIST_RESOURCE_KEY; separate key by design).
//
// Only the dispatch path (lands in P1c.3) ever decrypts. The REST
// endpoints in host_skill_credentials_handlers.go return masked
// values + the encrypted-state bit so admins can audit without
// exposing secrets.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HostSkillCredential mirrors a row in host_skill_credentials. Raw
// secret values are NEVER attached to this struct — callers that
// need the decrypted value go through decryptHostSkillCredential
// at injection time.
type HostSkillCredential struct {
	ID          int64      `json:"id"`
	HostSkillID int64      `json:"host_skill_id"`
	Key         string     `json:"key"`
	Encrypted   bool       `json:"encrypted"`
	MaskedValue string     `json:"masked_value"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// maskCredentialValue produces a UI-safe display string:
//   - <= 4 chars: "****"
//   - 5–8 chars: "<first>****<last>"
//   - > 8 chars: "<first 4>****<last 4>"
//
// The DB never returns the raw value to any API surface — only the
// dispatch path's decrypt helper does.
func maskCredentialValue(raw string) string {
	n := len(raw)
	if n == 0 {
		return ""
	}
	if n <= 4 {
		return "****"
	}
	if n <= 8 {
		return string(raw[0]) + "****" + string(raw[n-1])
	}
	return raw[:4] + "****" + raw[n-4:]
}

// encryptCoderCredential seals plaintext with AES-256-GCM. Returns
// (cipher_b64, true, nil) when polarCredentialKey is configured;
// ("", false, nil) when not — caller stores plaintext + sets
// encrypted=false. Same shape as encryptIOSDistSecret to keep the
// audit pattern uniform across credential domains.
func (p *Plugin) encryptCoderCredential(plaintext string) (string, bool, error) {
	if len(p.polarCredentialKey) != iosdistResourceKeyBytes {
		return "", false, nil
	}
	if plaintext == "" {
		return "", true, nil
	}
	block, err := aes.NewCipher(p.polarCredentialKey)
	if err != nil {
		return "", false, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", false, err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), true, nil
}

// decryptCoderCredential reverses encryptCoderCredential. Returns
// the plaintext or an error; callers that store plaintext (encrypted
// == false) should skip the decrypt step entirely.
func (p *Plugin) decryptCoderCredential(blob string) (string, error) {
	if len(p.polarCredentialKey) != iosdistResourceKeyBytes {
		return "", errors.New("polar credential key not configured")
	}
	if blob == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(p.polarCredentialKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// upsertHostSkillCredential encrypts (if a key is configured) and
// inserts-or-updates a credential keyed on (host_skill_id, key).
// Returns the masked-for-UI representation; raw plaintext is NOT
// surfaced back to the caller.
func (p *Plugin) upsertHostSkillCredential(hostSkillID int64, key, plaintext, createdBy string, now time.Time) (HostSkillCredential, error) {
	key = strings.TrimSpace(key)
	if hostSkillID <= 0 {
		return HostSkillCredential{}, errors.New("host_skill_id required")
	}
	if key == "" {
		return HostSkillCredential{}, errors.New("key required")
	}
	if strings.TrimSpace(createdBy) == "" {
		return HostSkillCredential{}, errors.New("created_by required")
	}

	cipherBlob, encrypted, err := p.encryptCoderCredential(plaintext)
	if err != nil {
		return HostSkillCredential{}, fmt.Errorf("encrypt: %w", err)
	}
	// When encryption is disabled, store the plaintext in the
	// fallback column and leave value_cipher empty. UI reads
	// `encrypted` to badge the row.
	plaintextCol := ""
	if !encrypted {
		plaintextCol = plaintext
	}

	var (
		id           int64
		insertedAt   time.Time
		updatedAt    time.Time
		storedCipher string
		storedPlain  string
		storedEnc    bool
		lastUsed     sql.NullTime
	)
	err = p.DB.QueryRow(`
		INSERT INTO host_skill_credentials
			(host_skill_id, key, value_cipher, value_plaintext, encrypted, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (host_skill_id, key) DO UPDATE
			SET value_cipher    = EXCLUDED.value_cipher,
			    value_plaintext = EXCLUDED.value_plaintext,
			    encrypted       = EXCLUDED.encrypted,
			    updated_at      = EXCLUDED.updated_at
		RETURNING id, value_cipher, value_plaintext, encrypted, created_at, updated_at, last_used_at`,
		hostSkillID, key, cipherBlob, plaintextCol, encrypted, createdBy, now,
	).Scan(&id, &storedCipher, &storedPlain, &storedEnc, &insertedAt, &updatedAt, &lastUsed)
	if err != nil {
		return HostSkillCredential{}, fmt.Errorf("upsert: %w", err)
	}

	out := HostSkillCredential{
		ID:          id,
		HostSkillID: hostSkillID,
		Key:         key,
		Encrypted:   storedEnc,
		MaskedValue: maskCredentialValue(plaintext),
		CreatedBy:   createdBy,
		CreatedAt:   insertedAt,
		UpdatedAt:   updatedAt,
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		out.LastUsedAt = &t
	}
	return out, nil
}

// listHostSkillCredentials returns every credential row for a skill
// with the value masked. Used by the UI to render a "configured
// keys" table without exposing secrets.
func (p *Plugin) listHostSkillCredentials(hostSkillID int64) ([]HostSkillCredential, error) {
	rows, err := p.DB.Query(`
		SELECT id, host_skill_id, key, value_cipher, value_plaintext, encrypted,
		       last_used_at, created_by, created_at, updated_at
		FROM host_skill_credentials
		WHERE host_skill_id = $1
		ORDER BY key`, hostSkillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostSkillCredential
	for rows.Next() {
		var (
			c          HostSkillCredential
			cipherBlob string
			plain      string
			lastUsed   sql.NullTime
		)
		if err := rows.Scan(&c.ID, &c.HostSkillID, &c.Key, &cipherBlob, &plain, &c.Encrypted,
			&lastUsed, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		// Compute the masked display value from whichever column
		// actually has data. We don't decrypt cipher just to mask —
		// the cipher is base64 so we'd see noise; mask the plaintext
		// column instead (only populated when encryption is disabled),
		// or use a fixed placeholder for encrypted rows.
		if c.Encrypted {
			c.MaskedValue = "********"
		} else {
			c.MaskedValue = maskCredentialValue(plain)
		}
		if lastUsed.Valid {
			t := lastUsed.Time
			c.LastUsedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fetchHostSkillCredentialPlaintext is the ONLY decrypt path —
// called by the dispatch layer (P1c.3) right before serializing the
// skill.start config. Touches last_used_at as a side effect so the
// UI can show "last injected N seconds ago" without exposing the
// value. Returns sql.ErrNoRows when the credential doesn't exist.
func (p *Plugin) fetchHostSkillCredentialPlaintext(hostSkillID int64, key string) (string, error) {
	var (
		cipherBlob string
		plain      string
		encrypted  bool
	)
	err := p.DB.QueryRow(`
		SELECT value_cipher, value_plaintext, encrypted
		FROM host_skill_credentials
		WHERE host_skill_id = $1 AND key = $2`,
		hostSkillID, key,
	).Scan(&cipherBlob, &plain, &encrypted)
	if err != nil {
		return "", err
	}
	value := plain
	if encrypted {
		value, err = p.decryptCoderCredential(cipherBlob)
		if err != nil {
			return "", fmt.Errorf("decrypt credential (skill=%d key=%s): %w", hostSkillID, key, err)
		}
	}
	// Best-effort touch — failure here doesn't fail the dispatch.
	_, _ = p.DB.Exec(`UPDATE host_skill_credentials SET last_used_at = $3 WHERE host_skill_id = $1 AND key = $2`,
		hostSkillID, key, time.Now().UTC())
	return value, nil
}

func (p *Plugin) deleteHostSkillCredential(hostSkillID int64, key string) error {
	_, err := p.DB.Exec(`DELETE FROM host_skill_credentials WHERE host_skill_id = $1 AND key = $2`,
		hostSkillID, strings.TrimSpace(key))
	return err
}
