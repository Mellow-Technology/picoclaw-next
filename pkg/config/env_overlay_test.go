package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTenantConfig writes a minimal current-version config.json (with one
// enabled telegram channel and one model) plus a .security.yml holding the
// telegram token and model key, and returns the config path.
func writeTenantConfig(t *testing.T, configJSON, securityYAML string) string {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(configJSON), 0o600))
	if securityYAML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, SecurityConfigFile), []byte(securityYAML), 0o600))
	}
	return configPath
}

const tenantConfigJSON = `{
  "version": 3,
  "gateway": {"host": "localhost", "port": 18790},
  "channel_list": {
    "telegram": {"enabled": true, "type": "telegram", "settings": {}}
  },
  "model_list": [
    {"model_name": "main", "provider": "openai", "model": "gpt-5.4", "api_base": "https://api.openai.com/v1"}
  ]
}`

const tenantSecurityYAML = `channel_list:
  telegram:
    settings:
      token: file-token
model_list:
  main:
    api_keys:
      - file-key
`

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// savedJSON returns config.json as a generic map.
func savedJSON(t *testing.T, configPath string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, configPath)), &m))
	return m
}

// jsonAt returns the value at a dotted path in m (no array support needed here).
func jsonAt(m map[string]any, path string) any {
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[seg]
	}
	return cur
}

// assertNotContains avoids dumping whole config files on failure.
func assertNotContains(t *testing.T, haystack, needle, msg string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: found %q", msg, needle)
	}
}

func assertContains(t *testing.T, haystack, needle, msg string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: missing %q", msg, needle)
	}
}

// Env overrides must never be written back by SaveConfig: CLI commands and
// the web backend load the effective config, edit it, and save it.
func TestSaveConfig_DoesNotPersistEnvOverrides(t *testing.T) {
	configPath := writeTenantConfig(t, tenantConfigJSON, tenantSecurityYAML)
	t.Setenv("PICOCLAW_CHANNELS_TELEGRAM_TOKEN", "env-token-secret")
	t.Setenv(EnvGatewayHost, "0.0.0.0")

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	tg, err := cfg.Channels.Get("telegram").GetDecoded()
	require.NoError(t, err)
	assert.Equal(t, "env-token-secret", tg.(*TelegramSettings).Token.String(), "env override applies at runtime")
	assert.Equal(t, "0.0.0.0", cfg.Gateway.Host)

	require.NoError(t, SaveConfig(configPath, cfg))

	sec := readFile(t, securityPath(configPath))
	assertNotContains(t, sec, "env-token-secret", ".security.yml must not contain env-provided secrets")
	assertContains(t, sec, "file-token", ".security.yml keeps the file value")

	assert.Equal(t, "localhost", jsonAt(savedJSON(t, configPath), "gateway.host"),
		"config.json must keep the file value, not the env override")
}

// A value the caller changes on purpose is saved, even if env overrode it.
func TestSaveConfig_KeepsCallerEditsOfOverriddenFields(t *testing.T) {
	configPath := writeTenantConfig(t, tenantConfigJSON, tenantSecurityYAML)
	t.Setenv(EnvGatewayHost, "0.0.0.0")
	t.Setenv("PICOCLAW_CHANNELS_TELEGRAM_TOKEN", "env-token-secret")

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = 19000 // not overridden: saved as usual
	tg, _ := cfg.Channels.Get("telegram").GetDecoded()
	tg.(*TelegramSettings).Token.Set("new-token-from-cli")
	require.NoError(t, SaveConfig(configPath, cfg))

	js := savedJSON(t, configPath)
	assert.Equal(t, "127.0.0.1", jsonAt(js, "gateway.host"))
	assert.Equal(t, float64(19000), jsonAt(js, "gateway.port"))
	sec := readFile(t, securityPath(configPath))
	assertContains(t, sec, "new-token-from-cli", "explicitly set secret is saved")
	assertNotContains(t, sec, "env-token-secret", "env secret is not saved")
}

// Old configs are migrated and saved inside LoadConfig; env values must not
// end up in the migrated files either.
func TestLoadConfig_MigrationSaveDoesNotPersistEnvOverrides(t *testing.T) {
	configPath := writeTenantConfig(t, `{"version": 2, "gateway": {"host": "localhost", "port": 18790}}`, "")
	t.Setenv(EnvGatewayHost, "0.0.0.0")
	t.Setenv("PICOCLAW_AGENTS_DEFAULTS_MAX_TOKENS", "1234")

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", cfg.Gateway.Host)
	assert.Equal(t, 1234, cfg.Agents.Defaults.MaxTokens)

	js := savedJSON(t, configPath)
	assert.Equal(t, float64(CurrentVersion), jsonAt(js, "version"), "config was migrated and saved")
	assert.Equal(t, "localhost", jsonAt(js, "gateway.host"))
	assert.NotEqual(t, float64(1234), jsonAt(js, "agents.defaults.max_tokens"))
}

func TestLoadConfig_SubstitutesEnvRefsAndSavesTemplates(t *testing.T) {
	configJSON := `{
  "version": 3,
  "gateway": {"host": "${PC_TEST_HOST:-localhost}", "port": 18790},
  "model_list": [
    {"model_name": "main", "provider": "openai", "model": "${PC_TEST_MODEL}",
     "api_base": "https://${PC_TEST_API_HOST}/v1", "api_keys": ["${PC_TEST_API_KEY}"]}
  ],
  "tools": {"mcp": {"enabled": true, "servers": {
    "fs": {"enabled": true, "command": "npx", "args": ["$${HOME}", "${PC_TEST_UNSET}"],
           "env": {"TOKEN": "${PC_TEST_MCP_TOKEN}"}}
  }}}
}`
	configPath := writeTenantConfig(t, configJSON, "")
	t.Setenv("PC_TEST_MODEL", "gpt-5.4")
	t.Setenv("PC_TEST_API_HOST", "llm.internal")
	t.Setenv("PC_TEST_API_KEY", "sk-from-env")
	t.Setenv("PC_TEST_MCP_TOKEN", "mcp-secret")

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	assert.Equal(t, "localhost", cfg.Gateway.Host, "default used when unset")
	m := cfg.ModelList[0]
	assert.Equal(t, "gpt-5.4", m.Model)
	assert.Equal(t, "https://llm.internal/v1", m.APIBase)
	assert.Equal(t, "sk-from-env", m.APIKey())
	fs := cfg.Tools.MCP.Servers["fs"]
	assert.Equal(t, []string{"${HOME}", "${PC_TEST_UNSET}"}, fs.Args, "escape gives a literal; unset is left as written")
	assert.Equal(t, "mcp-secret", fs.Env["TOKEN"])

	require.NoError(t, SaveConfig(configPath, cfg))

	js := readFile(t, configPath)
	for _, want := range []string{
		`"${PC_TEST_HOST:-localhost}"`, `"${PC_TEST_MODEL}"`, `"https://${PC_TEST_API_HOST}/v1"`,
		`"$${HOME}"`, `"${PC_TEST_UNSET}"`, `"${PC_TEST_MCP_TOKEN}"`,
	} {
		assertContains(t, js, want, "config.json keeps the reference")
	}
	for _, leaked := range []string{"llm.internal", "mcp-secret", "sk-from-env"} {
		assertNotContains(t, js, leaked, "config.json must not contain substituted values")
	}
	sec := readFile(t, securityPath(configPath))
	assertContains(t, sec, "${PC_TEST_API_KEY}", ".security.yml keeps the key reference")
	assertNotContains(t, sec, "sk-from-env", ".security.yml must not contain the resolved key")

	// The saved files load to the same effective values.
	again, err := LoadConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "sk-from-env", again.ModelList[0].APIKey())
	assert.Equal(t, "https://llm.internal/v1", again.ModelList[0].APIBase)
}

func TestLoadConfig_SecretRefsInSecurityYAML(t *testing.T) {
	configPath := writeTenantConfig(t, tenantConfigJSON, `channel_list:
  telegram:
    settings:
      token: ${PC_TEST_TG_TOKEN}
model_list:
  main:
    api_keys:
      - ${PC_TEST_API_KEY}
`)
	t.Setenv("PC_TEST_TG_TOKEN", "123:tg-from-env")
	t.Setenv("PC_TEST_API_KEY", "sk-from-env")

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)
	tg, _ := cfg.Channels.Get("telegram").GetDecoded()
	assert.Equal(t, "123:tg-from-env", tg.(*TelegramSettings).Token.String())
	assert.Equal(t, "sk-from-env", cfg.ModelList[0].APIKey())

	require.NoError(t, SaveConfig(configPath, cfg))
	sec := readFile(t, securityPath(configPath))
	assertContains(t, sec, "${PC_TEST_TG_TOKEN}", "token reference kept")
	assertContains(t, sec, "${PC_TEST_API_KEY}", "key reference kept")
	assertNotContains(t, sec, "tg-from-env", "resolved token not saved")
	assertNotContains(t, sec, "sk-from-env", "resolved key not saved")
}

func TestLoadConfig_ChannelEnvByTypeAndName(t *testing.T) {
	configJSON := `{
  "version": 3,
  "channel_list": {
    "telegram": {"enabled": false, "type": "telegram", "settings": {}},
    "support-bot": {"enabled": false, "type": "telegram", "settings": {}}
  }
}`
	configPath := writeTenantConfig(t, configJSON, "")
	t.Setenv("PICOCLAW_CHANNELS_TELEGRAM_ENABLED", "true")          // both telegram channels
	t.Setenv("PICOCLAW_CHANNELS_TELEGRAM_TOKEN", "shared-token")    // both telegram channels
	t.Setenv("PICOCLAW_CHANNEL_SUPPORT_BOT_TOKEN", "support-token") // only support-bot
	t.Setenv("PICOCLAW_CHANNEL_SUPPORT_BOT_PROXY", "http://proxy:3128")

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	main := cfg.Channels.Get("telegram")
	support := cfg.Channels.Get("support-bot")
	assert.True(t, main.Enabled)
	assert.True(t, support.Enabled)
	mainTG, _ := main.GetDecoded()
	supportTG, _ := support.GetDecoded()
	assert.Equal(t, "shared-token", mainTG.(*TelegramSettings).Token.String())
	assert.Equal(t, "", mainTG.(*TelegramSettings).Proxy)
	assert.Equal(t, "support-token", supportTG.(*TelegramSettings).Token.String(), "per-name wins over per-type")
	assert.Equal(t, "http://proxy:3128", supportTG.(*TelegramSettings).Proxy)

	t.Run("name-level enabled overrides type-level", func(t *testing.T) {
		t.Setenv("PICOCLAW_CHANNEL_SUPPORT_BOT_ENABLED", "false")
		cfg, err := LoadConfig(configPath)
		require.NoError(t, err)
		assert.True(t, cfg.Channels.Get("telegram").Enabled)
		assert.False(t, cfg.Channels.Get("support-bot").Enabled)
	})

	require.NoError(t, SaveConfig(configPath, cfg))
	js := savedJSON(t, configPath)
	assert.Equal(t, false, jsonAt(js, "channel_list.telegram.enabled"))
	assert.Equal(t, false, jsonAt(js, "channel_list.support-bot.enabled"))
	assert.Equal(t, "", jsonAt(js, "channel_list.support-bot.settings.proxy"))
	sec := readFile(t, securityPath(configPath))
	assertNotContains(t, sec, "shared-token", "env token not saved")
	assertNotContains(t, sec, "support-token", "env token not saved")
}

func TestLoadConfig_InvalidChannelEnabledEnv(t *testing.T) {
	configPath := writeTenantConfig(t, tenantConfigJSON, "")
	t.Setenv("PICOCLAW_CHANNEL_TELEGRAM_ENABLED", "yes please")
	_, err := LoadConfig(configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PICOCLAW_CHANNELS_TELEGRAM_ENABLED")
}

// With an encryption passphrase, secrets serialize to a fresh ciphertext each
// time; env-provided secrets must still be kept out of .security.yml.
func TestSaveConfig_EnvSecretWithPassphrase(t *testing.T) {
	mustSetupSSHKey(t)
	t.Setenv("PICOCLAW_KEY_PASSPHRASE", "test-passphrase")
	configPath := writeTenantConfig(t, tenantConfigJSON, tenantSecurityYAML)
	t.Setenv("PICOCLAW_CHANNELS_TELEGRAM_TOKEN", "env-token-secret")

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.NoError(t, SaveConfig(configPath, cfg))

	os.Unsetenv("PICOCLAW_CHANNELS_TELEGRAM_TOKEN") // t.Setenv above restores it after the test
	reloaded, err := LoadConfig(configPath)
	require.NoError(t, err)
	tg, _ := reloaded.Channels.Get("telegram").GetDecoded()
	assert.Equal(t, "file-token", tg.(*TelegramSettings).Token.String(), "saved file still holds the file token")
	assertNotContains(t, readFile(t, securityPath(configPath)), "env-token-secret", "no plaintext env secret")
}

func TestExpandEnvRefs(t *testing.T) {
	t.Setenv("PC_SET", "value")
	t.Setenv("PC_EMPTY", "")
	tests := []struct {
		in, want string
		missing  []string
	}{
		{"plain", "plain", nil},
		{"${PC_SET}", "value", nil},
		{"a-${PC_SET}-b", "a-value-b", nil},
		{"${PC_EMPTY}", "", nil},
		{"${PC_EMPTY:-dflt}", "dflt", nil},
		{"${PC_UNSET:-dflt}", "dflt", nil},
		{"${PC_UNSET:-}", "", nil},
		{"${PC_UNSET}", "${PC_UNSET}", []string{"PC_UNSET"}},
		{"$${PC_SET}", "${PC_SET}", nil},
		{"$HOME and ${ not-a-ref }", "$HOME and ${ not-a-ref }", nil},
	}
	for _, tt := range tests {
		got, missing := expandEnvRefs(tt.in)
		assert.Equal(t, tt.want, got, tt.in)
		assert.Equal(t, tt.missing, missing, tt.in)
	}
}

// The order-preserving JSON tree must reproduce MarshalIndent byte for byte,
// or every save with an override would reformat config.json.
func TestJSONTreeRoundTripMatchesMarshalIndent(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tools.Exec.CustomDenyPatterns = []string{`<script>&"quoted"\n`, "日本語 💭"}
	data, err := marshalConfigJSON(cfg)
	require.NoError(t, err)
	tree, err := parseJSONTree(data)
	require.NoError(t, err)
	var buf bytes.Buffer
	tree.encode(&buf, 0)
	if buf.String() != string(data) {
		t.Fatalf("round trip differs from MarshalIndent output")
	}
}
