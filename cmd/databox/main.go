// Command databox is the single binary for everything
// (§5): storage node, processing layers, interactive
// console, and operator tooling. Command routing and flag parsing are
// handled exclusively by urfave/cli/v3.
//
// The command tree:
//
//	databox server                    run a storage node
//	databox service sql|s3            run a processing layer
//	databox console                   interactive REPL
//	databox cluster status|join-token|decommission|remove
//	databox cluster-info                alias for cluster status
//	databox admin rebalance|split|repair pause|resume (§16.4)
//	databox user …  / databox grant … identity management
//	databox backup … / databox restore
//	databox certificates generate     static certs for production
//	databox psk generate              node pre-shared keys
//	databox recover root-password     node-local root reset (§7.3)
//	databox utils sql|s3              layer client utilities: SQL REPL, S3 gateway client
//	databox config show               effective configuration + sources
//	databox version                   build information
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/hyperkubeorg/databox/pkg/config"
	"github.com/hyperkubeorg/databox/pkg/telemetry"
	"github.com/hyperkubeorg/databox/pkg/version"
)

// telemetryShutdown flushes buffered spans on exit; set by the root Before
// hook for the long-running commands (server, gateway).
var telemetryShutdown func(context.Context) error

func main() {
	app := &cli.Command{
		Name:  "databox",
		Usage: "distributed key-value & blob storage in a single binary",
		Commands: []*cli.Command{
			serverCommand(),
			serviceCommand(),
			consoleCommand(),
			clusterCommand(),
			clusterInfoCommand(),
			adminCommand(),
			userCommand(),
			grantCommand(),
			backupCommand(),
			restoreCommand(),
			certificatesCommand(),
			pskCommand(),
			recoverCommand(),
			utilsCommand(),
			configCommand(),
			versionCommand(),
		},
		// OpenTelemetry tracing (§19): enabled only for the long-running
		// commands and only when the standard OTel env vars ask for it
		// (see pkg/telemetry). One-shot CLI commands are not traced.
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			switch cmd.Args().First() {
			case "server":
				telemetryShutdown = telemetry.Init("databox", logger())
			case "gateway", "service":
				telemetryShutdown = telemetry.Init("databox-gateway", logger())
			}
			return ctx, nil
		},
		After: func(_ context.Context, _ *cli.Command) error {
			if telemetryShutdown == nil {
				return nil
			}
			// Fresh context: the run context is already canceled on
			// SIGINT/SIGTERM, and the final flush needs a live one.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryShutdown(ctx); err != nil {
				logger().Warn("telemetry shutdown", "error", err)
			}
			return nil
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// logger builds the process-wide structured JSON logger (§19).
func logger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("DATABOX_DEBUG") != "" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// buildConfig assembles configuration in precedence order (§16.1):
// defaults ← config file ← environment ← flags.
func buildConfig(cmd *cli.Command) (*config.Config, error) {
	cfg := config.Default()
	if path := cmd.String("config"); path != "" {
		if err := cfg.LoadFile(path); err != nil {
			return nil, err
		}
	}
	cfg.LoadEnv()
	// Flags win last. Only apply flags the user actually set.
	if cmd.IsSet("listen") {
		cfg.SetFlag("listen", func(c *config.Config) { c.Listen = cmd.String("listen") })
	}
	if cmd.IsSet("advertise") {
		cfg.SetFlag("advertise_addr", func(c *config.Config) { c.AdvertiseAddr = cmd.String("advertise") })
	}
	if cmd.IsSet("data-dir") {
		cfg.SetFlag("data_dir", func(c *config.Config) { c.DataDir = cmd.String("data-dir") })
	}
	if cmd.IsSet("node-name") {
		cfg.SetFlag("node_name", func(c *config.Config) { c.NodeName = cmd.String("node-name") })
	}
	if cmd.IsSet("join") {
		cfg.SetFlag("join", func(c *config.Config) { c.Join = cmd.String("join") })
	}
	if cmd.IsSet("psk") {
		cfg.SetFlag("psk", func(c *config.Config) { c.PSK = cmd.String("psk") })
	}
	if cmd.IsSet("auto-cert") {
		cfg.SetFlag("auto_cert", func(c *config.Config) { c.AutoCert = cmd.Bool("auto-cert") })
	}
	if cmd.IsSet("tls-cert") {
		cfg.SetFlag("tls_cert_file", func(c *config.Config) { c.TLSCertFile = cmd.String("tls-cert") })
	}
	if cmd.IsSet("tls-key") {
		cfg.SetFlag("tls_key_file", func(c *config.Config) { c.TLSKeyFile = cmd.String("tls-key") })
	}
	if cmd.IsSet("root-password-file") {
		cfg.SetFlag("root_password_file", func(c *config.Config) { c.RootPasswordFile = cmd.String("root-password-file") })
	}
	if cmd.IsSet("replicas") {
		cfg.SetFlag("replicas", func(c *config.Config) { c.Replicas = int(cmd.Int("replicas")) })
	}
	return cfg, cfg.Finish()
}

// serverFlags are shared by `server` and `config show` so both resolve
// configuration identically.
func serverFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "config", Usage: "path to YAML config file (§16.1)"},
		&cli.StringFlag{Name: "listen", Usage: "HTTPS listen address (default :8443)"},
		&cli.StringFlag{Name: "advertise", Usage: "address peers/clients use to reach this node"},
		&cli.StringFlag{Name: "data-dir", Usage: "storage directory (default /var/lib/databox)"},
		&cli.StringFlag{Name: "node-name", Usage: "stable unique node name (default hostname)"},
		&cli.StringFlag{Name: "join", Usage: "join token from `databox cluster join-token` (§16.2)"},
		&cli.StringFlag{Name: "psk", Usage: "node pre-shared key (§6.2)"},
		&cli.BoolFlag{Name: "auto-cert", Usage: "use a self-signed certificate instead of auto-PKI"},
		&cli.StringFlag{Name: "tls-cert", Usage: "static TLS certificate file (with --tls-key)"},
		&cli.StringFlag{Name: "tls-key", Usage: "static TLS key file (with --tls-cert)"},
		&cli.StringFlag{Name: "root-password-file", Usage: "file with the bootstrap root password"},
		&cli.IntFlag{Name: "replicas", Usage: "KV replication factor (default 3)"},
	}
}

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print binary version, Go runtime, and build metadata",
		Action: func(_ context.Context, _ *cli.Command) error {
			fmt.Println(version.String())
			return nil
		},
	}
}

func configCommand() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "configuration utilities",
		Commands: []*cli.Command{{
			Name:  "show",
			Usage: "print the effective configuration and where each value came from",
			Flags: serverFlags(),
			Action: func(_ context.Context, cmd *cli.Command) error {
				cfg, err := buildConfig(cmd)
				if err != nil {
					return err
				}
				fmt.Print(cfg.Show())
				return nil
			},
		}},
	}
}

// waitCtx is a tiny helper for commands that need a bounded context.
func waitCtx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
