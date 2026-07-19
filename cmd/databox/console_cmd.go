// console_cmd.go wires `databox console` (§5.1): connect,
// authenticate (with the §6.3 trust-on-first-use certificate workflow),
// then hand off to the REPL in pkg/console.
package main

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/hyperkubeorg/databox/pkg/console"
)

func consoleCommand() *cli.Command {
	return &cli.Command{
		Name:  "console",
		Usage: "interactive REPL for KV/blob operations and administration",
		Flags: append(connFlags(),
			&cli.StringFlag{Name: "execute", Aliases: []string{"e"}, Usage: "run these commands (';' or newline separated) and exit"},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			c, err := dial(ctx, cmd)
			if err != nil {
				return err
			}
			sess := &console.Session{Client: c, Output: cmd.String("output")}
			return sess.Run(ctx, cmd.String("execute"))
		},
	}
}
