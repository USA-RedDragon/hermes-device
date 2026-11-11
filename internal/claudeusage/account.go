package claudeusage

import (
	"encoding/json"
	"os"
)

// maxConfig bounds the .claude.json read for the signed-in account.
const maxConfig = 64 << 20

// readOAuthAccount returns the account in a .claude.json's oauthAccount:
// its uuid, organization uuid and email as the label. Every other field of
// the file is skipped.
func readOAuthAccount(path string) Account {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > maxConfig {
		return Account{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Account{}
	}
	var cfg struct {
		OAuthAccount *struct {
			AccountUUID      string `json:"accountUuid"`
			OrganizationUUID string `json:"organizationUuid"`
			EmailAddress     string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.OAuthAccount == nil {
		return Account{}
	}
	o := cfg.OAuthAccount
	return Account{UUID: o.AccountUUID, Org: o.OrganizationUUID, Label: o.EmailAddress}
}
