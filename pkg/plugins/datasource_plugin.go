package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"time"

	"github.com/grafana/grafana/pkg/setting"

	datasourceV1 "github.com/grafana/grafana-plugin-model/go/datasource"
	sdk "github.com/grafana/grafana-plugin-sdk-go"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins/datasource/wrapper"
	"github.com/grafana/grafana/pkg/tsdb"
	plugin "github.com/hashicorp/go-plugin"
	"golang.org/x/xerrors"
)

// DataSourcePlugin contains all metadata about a datasource plugin
type DataSourcePlugin struct {
	FrontendPluginBase
	Annotations   bool              `json:"annotations"`
	Metrics       bool              `json:"metrics"`
	Alerting      bool              `json:"alerting"`
	Explore       bool              `json:"explore"`
	Table         bool              `json:"tables"`
	HiddenQueries bool              `json:"hiddenQueries"`
	Logs          bool              `json:"logs"`
	QueryOptions  map[string]bool   `json:"queryOptions,omitempty"`
	BuiltIn       bool              `json:"builtIn,omitempty"`
	Mixed         bool              `json:"mixed,omitempty"`
	Routes        []*AppPluginRoute `json:"routes"`
	Streaming     bool              `json:"streaming"`

	Backend    bool   `json:"backend,omitempty"`
	Executable string `json:"executable,omitempty"`
	SDK        bool   `json:"sdk,omitempty"`

	log    log.Logger
	client *plugin.Client
}

func (p *DataSourcePlugin) Load(decoder *json.Decoder, pluginDir string) error {
	if err := decoder.Decode(&p); err != nil {
		return err
	}

	if !p.isVersionOne() && !setting.IsExpressionsEnabled() {
		return errors.New("A plugin version 2 was found but expressions feature toggle is not enabled")
	}

	if err := p.registerPlugin(pluginDir); err != nil {
		return err
	}

	DataSources[p.Id] = p
	return nil
}

var handshakeConfig = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "grafana_plugin_type",
	MagicCookieValue: "datasource",
}

func (p *DataSourcePlugin) startBackendPlugin(ctx context.Context, log log.Logger) error {
	p.log = log.New("plugin-id", p.Id)

	if err := p.spawnSubProcess(); err != nil {
		return err
	}

	go func() {
		if err := p.restartKilledProcess(ctx); err != nil {
			p.log.Error("Attempting to restart killed process failed", "err", err)
		}
	}()

	return nil
}
func (p *DataSourcePlugin) isVersionOne() bool {
	return !p.SDK
}

func (p *DataSourcePlugin) spawnSubProcess() error {
	cmd := ComposePluginStartCommmand(p.Executable)
	fullpath := path.Join(p.PluginDir, cmd)

	var newClient *plugin.Client
	if p.isVersionOne() {
		newClient = plugin.NewClient(&plugin.ClientConfig{
			HandshakeConfig:  handshakeConfig,
			Plugins:          map[string]plugin.Plugin{p.Id: &datasourceV1.DatasourcePluginImpl{}},
			Cmd:              exec.Command(fullpath),
			AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
			Logger:           LogWrapper{Logger: p.log},
		})

	} else {
		newClient = plugin.NewClient(&plugin.ClientConfig{
			HandshakeConfig:  handshakeConfig,
			Plugins:          map[string]plugin.Plugin{p.Id: &sdk.DatasourcePluginImpl{}},
			Cmd:              exec.Command(fullpath),
			AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
			Logger:           LogWrapper{Logger: p.log},
		})
	}

	p.client = newClient

	rpcClient, err := p.client.Client()
	if err != nil {
		return err
	}

	raw, err := rpcClient.Dispense(p.Id)
	if err != nil {
		return err
	}

	if p.isVersionOne() {
		plugin := raw.(datasourceV1.DatasourcePlugin)

		tsdb.RegisterTsdbQueryEndpoint(p.Id, func(dsInfo *models.DataSource) (tsdb.TsdbQueryEndpoint, error) {
			return wrapper.NewDatasourcePluginWrapper(p.log, plugin), nil
		})
		return nil
	}

	plugin, ok := raw.(sdk.DatasourcePlugin)
	if !ok {
		return fmt.Errorf("unxpected type %T, expeced sdk.DatasourcePlugin", raw)
	}

	tsdb.RegisterTsdbQueryEndpoint(p.Id, func(dsInfo *models.DataSource) (tsdb.TsdbQueryEndpoint, error) {
		return wrapper.NewDatasourcePluginWrapperV2(p.log, plugin), nil
	})

	return nil
}

func (p *DataSourcePlugin) restartKilledProcess(ctx context.Context) error {
	ticker := time.NewTicker(time.Second * 1)

	for {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil && !xerrors.Is(err, context.Canceled) {
				return err
			}
			return nil
		case <-ticker.C:
			if !p.client.Exited() {
				continue
			}

			if err := p.spawnSubProcess(); err != nil {
				p.log.Error("Failed to restart plugin", "err", err)
				continue
			}

			p.log.Debug("Plugin process restarted")
		}
	}
}

func (p *DataSourcePlugin) Kill() {
	if p.client != nil {
		p.log.Debug("Killing subprocess ", "name", p.Name)
		p.client.Kill()
	}
}
