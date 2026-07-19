// server_cmd.go wires `databox server` and `databox service sql|s3`.
//
// `server` starts a storage node: it mounts the API and GUI routers onto
// the node's HTTPS listener and runs until SIGINT/SIGTERM.
//
// `service sql` / `service s3` start stateless processing layers (§13,
// §14): they connect to a cluster as clients and expose their own
// protocol front-ends. They share this binary but hold no data.
package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/hyperkubeorg/databox/pkg/routes/frontend"
	"github.com/hyperkubeorg/databox/pkg/routes/v1api"
	"github.com/hyperkubeorg/databox/pkg/server"
	s3service "github.com/hyperkubeorg/databox/pkg/service/s3"
	sqlservice "github.com/hyperkubeorg/databox/pkg/service/sql"
)

func serverCommand() *cli.Command {
	return &cli.Command{
		Name:  "server",
		Usage: "start a storage node (zero config = single-node cluster)",
		Flags: serverFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := buildConfig(cmd)
			if err != nil {
				return err
			}
			log := logger()
			// Attach the public surfaces before the listener starts:
			// the JSON API under /api/v1 and the web GUI at /.
			// Order matters: the API routers claim /api/v1 first; the
			// GUI's catch-all root goes last.
			server.Mounters = append(server.Mounters, v1api.Mount, v1api.MountBackup, frontend.Mount)
			s, err := server.New(cfg, log)
			if err != nil {
				return err
			}
			return s.Run(ctx)
		},
	}
}

// layerFlags are shared by both processing layers.
func layerFlags(defaultListen string) []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "listen", Value: defaultListen, Usage: "listen address for this layer"},
		&cli.StringFlag{Name: "cluster", Value: "localhost:8443", Usage: "databox cluster endpoint (host:port)"},
		&cli.StringFlag{Name: "ca-fingerprint", Usage: "pin the cluster certificate by SHA-256 fingerprint"},
		&cli.StringFlag{Name: "tls-cert", Usage: "TLS certificate for this layer's listener"},
		&cli.StringFlag{Name: "tls-key", Usage: "TLS key for this layer's listener"},
	}
}

func serviceCommand() *cli.Command {
	return &cli.Command{
		Name:    "gateway",
		Aliases: []string{"service"}, // original name kept as an alias
		Usage:   "run a stateless gateway (protocol service) on top of the kv/blob system",
		Description: "Gateways turn the storage layer into protocol services: they hold no\n" +
			"data, authenticate to the cluster like any client, and scale by replica\n" +
			"count independently of the data nodes. The built-ins are `s3` and `sql`;\n" +
			"custom gateways for other interfaces build the same way on pkg/client.",
		Commands: []*cli.Command{
			{
				Name:  "sql",
				Usage: "PostgreSQL-wire SQL gateway speaking the chai dialect (§13)",
				Flags: append(layerFlags(":5432"),
					&cli.BoolFlag{Name: "allow-cleartext", Usage: "accept non-TLS pg connections (trusted networks only; passwords travel in the clear)"},
				),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return sqlservice.Run(ctx, sqlservice.Options{
						Listen:         cmd.String("listen"),
						Cluster:        cmd.String("cluster"),
						CAFingerprint:  cmd.String("ca-fingerprint"),
						TLSCertFile:    cmd.String("tls-cert"),
						TLSKeyFile:     cmd.String("tls-key"),
						AllowCleartext: cmd.Bool("allow-cleartext"),
						Logger:         logger(),
					})
				},
			},
			{
				Name:  "s3",
				Usage: "S3-compatible gateway over the blob store (§14)",
				Flags: append(layerFlags(":9000"),
					// The S3 gateway needs an operator identity to resolve
					// API keys and grants from the system view; SQL does
					// not (each pg connection logs in as its own user).
					&cli.StringFlag{Name: "operator-user", Value: "root", Usage: "databox user the gateway resolves keys/grants as", Sources: cli.EnvVars("DATABOX_OPERATOR_USER")},
					&cli.StringFlag{Name: "operator-password", Usage: "that user's password", Sources: cli.EnvVars("DATABOX_OPERATOR_PASSWORD")},
					&cli.BoolFlag{Name: "allow-cleartext", Usage: "serve plain HTTP when no TLS cert is configured (trusted networks only)"},
					&cli.StringFlag{Name: "root-prefix", Value: "/s3/", Usage: "KV prefix buckets map under (§14)"},
					&cli.DurationFlag{Name: "multipart-ttl", Usage: "expire unfinished multipart uploads after this long (default 168h)"},
					&cli.DurationFlag{Name: "clock-skew", Usage: "max tolerated x-amz-date drift for SigV4 (default 15m)"},
				),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return s3service.Run(ctx, s3service.Options{
						Listen:           cmd.String("listen"),
						Cluster:          cmd.String("cluster"),
						CAFingerprint:    cmd.String("ca-fingerprint"),
						TLSCertFile:      cmd.String("tls-cert"),
						TLSKeyFile:       cmd.String("tls-key"),
						OperatorUser:     cmd.String("operator-user"),
						OperatorPassword: cmd.String("operator-password"),
						AllowCleartext:   cmd.Bool("allow-cleartext"),
						RootPrefix:       cmd.String("root-prefix"),
						MultipartTTL:     cmd.Duration("multipart-ttl"),
						ClockSkew:        cmd.Duration("clock-skew"),
						Logger:           logger(),
					})
				},
			},
		},
	}
}

// ensure fmt stays imported even if handlers above change shape.
var _ = fmt.Sprintf
