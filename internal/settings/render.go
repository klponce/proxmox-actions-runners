package settings

import (
	"fmt"
	"net/netip"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
)

// Env is what the controller's config needs from the node itself. It is read fresh each time the config is rendered,
// never stored in the settings.
type Env struct {
	Node string
	// PVEAddress is the address the controller reaches the API on.
	PVEAddress netip.Addr
	TLS        hostsys.TLSInfo
	// LinkedClone reports whether the storage can make linked clones.
	LinkedClone bool
}

const configHeader = "# Written by parcon on the Proxmox host from its settings, and rewritten whenever they change.\n" +
	"# Change them there with `parcon config set KEY VALUE`, not here.\n"

// ControllerConfig renders the controller VM's config.yaml from the settings and the node, and checks it the way
// the controller will: with config.Parse. It returns the parsed config and the file's bytes.
func ControllerConfig(s *Settings, env Env) (*config.Config, []byte, error) {
	linked := env.LinkedClone
	c := &config.Config{
		Proxmox: config.Proxmox{
			URL:             fmt.Sprintf("https://%s/api2/json", netip.AddrPortFrom(env.PVEAddress, 8006)),
			TokenID:         config.DefaultTokenID,
			TokenSecretFile: config.TokenSecretFile,
			Node:            env.Node,
			Pool:            config.DefaultPool,
			Storage:         s.Proxmox.Storage,
			Zone:            config.DefaultZone,
			VNet:            config.DefaultVNet,
			VMIDRange:       s.Proxmox.VMIDRange,
			LinkedClone:     &linked,
		},
		GitHub: config.GitHub{ConfigURL: s.GitHub.ConfigURL},
		Worker: s.Worker,
		ScaleSets: []config.ScaleSet{{
			Name:        s.ScaleSet.Name,
			Labels:      s.ScaleSet.Labels,
			RunnerGroup: s.ScaleSet.RunnerGroup,
			MinRunners:  s.ScaleSet.MinRunners,
			MaxRunners:  s.ScaleSet.MaxRunners,
		}},
	}
	switch env.TLS.Mode {
	case hostsys.TLSNodeCA:
		c.Proxmox.CACertFile = config.CACertFile
		c.Proxmox.TLSServerName = env.TLS.ServerName
	case hostsys.TLSSystem:
		c.Proxmox.TLSServerName = env.TLS.ServerName
	case hostsys.TLSPin:
		c.Proxmox.TLSFingerprint = env.TLS.Fingerprint
	}
	// The App comes once its key is in the controller, which is when the installer learns its Client ID.
	if s.GitHub.App.ClientID != "" {
		c.GitHub.App = config.GitHubApp{
			ClientID:       s.GitHub.App.ClientID,
			InstallationID: s.GitHub.App.InstallationID,
			PrivateKeyFile: config.AppKeyFile,
		}
	}

	body, err := config.Marshal(c)
	if err != nil {
		return nil, nil, err
	}
	data := append([]byte(configHeader), body...)
	parsed, err := config.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("render the controller's config: %w", err)
	}
	return parsed, data, nil
}
