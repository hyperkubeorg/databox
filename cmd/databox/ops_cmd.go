// ops_cmd.go implements the operator command families: cluster lifecycle,
// user/grant management, backup/restore, certificate and PSK generation,
// and node-local root recovery (§5).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/hyperkubeorg/databox/pkg/certs"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/server"
)

// emit renders a result in the requested output format (§5: -o json|yaml).
func emit(cmd *cli.Command, v any) error {
	switch cmd.String("output") {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		raw, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(raw)
		return err
	default:
		// Text mode: JSON is still the most faithful generic rendering
		// for structured values; simple strings print as-is.
		if s, ok := v.(string); ok {
			fmt.Println(s)
			return nil
		}
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
}

// statusView is the /cluster/status payload: the server's report plus the
// pending manual split hints the handler appends alongside it (§15).
type statusView struct {
	server.StatusReport
	SplitHints []cluster.SplitHint `json:"split_hints"`
}

// clusterStatusAction fetches and renders cluster status — shared by
// `cluster status`, its `info` alias, and the top-level `cluster-info`
// compatibility command the spec's §5 table names.
func clusterStatusAction(ctx context.Context, cmd *cli.Command) error {
	c, err := dial(ctx, cmd)
	if err != nil {
		return err
	}
	var report statusView
	if err := c.Raw(ctx, http.MethodGet, "/api/v1/cluster/status", nil, &report); err != nil {
		return err
	}
	if cmd.String("output") != "text" {
		return emit(cmd, report)
	}
	printStatus(report)
	return nil
}

// clusterInfoCommand is the kept top-level alias for `cluster status`.
func clusterInfoCommand() *cli.Command {
	return &cli.Command{
		Name:   "cluster-info",
		Usage:  "alias for `cluster status`",
		Flags:  connFlags(),
		Action: clusterStatusAction,
	}
}

// removeNodeAction posts a decommission (force per flag) — shared by
// `cluster decommission` and `cluster remove` (§16.3).
func removeNodeAction(ctx context.Context, cmd *cli.Command, usage string) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	var nodeID uint64
	if _, err := fmt.Sscanf(cmd.Args().First(), "%d", &nodeID); err != nil {
		return fmt.Errorf("node id must be a number: %w", err)
	}
	c, err := dial(ctx, cmd)
	if err != nil {
		return err
	}
	var out struct {
		Guidance string `json:"guidance"`
	}
	if err := c.Raw(ctx, http.MethodPost, "/api/v1/cluster/decommission",
		map[string]any{"node_id": nodeID, "force": cmd.Bool("force")}, &out); err != nil {
		return err
	}
	fmt.Println(out.Guidance)
	return nil
}

func clusterCommand() *cli.Command {
	return &cli.Command{
		Name:  "cluster",
		Usage: "cluster lifecycle: status, join tokens, decommission (§16)",
		Commands: []*cli.Command{
			{
				Name:    "status",
				Aliases: []string{"info"}, // `cluster info` compatibility
				Usage:   "topology, shard health, per-node safe-to-remove, active alerts",
				Flags:   connFlags(),
				Action:  clusterStatusAction,
			},
			{
				Name:  "join-token",
				Usage: "emit a one-line token for joining a new node (§16.2)",
				Flags: append(connFlags(), &cli.StringFlag{Name: "ttl", Value: "1h", Usage: "token validity window"}),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					var out struct {
						Token string `json:"token"`
					}
					if err := c.Raw(ctx, http.MethodPost, "/api/v1/cluster/join-token",
						map[string]string{"ttl": cmd.String("ttl")}, &out); err != nil {
						return err
					}
					fmt.Println(out.Token)
					return nil
				},
			},
			{
				Name:      "decommission",
				Usage:     "drain and remove a node with guided safety checks (§16.3)",
				ArgsUsage: "<node-id>",
				Flags:     append(connFlags(), &cli.BoolFlag{Name: "force", Usage: "remove a dead node without draining"}),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return removeNodeAction(ctx, cmd, "databox cluster decommission <node-id> [--force]")
				},
			},
			{
				// The §16.3 dead-hardware form: `cluster remove <node>
				// --force`. Same server operation as decommission; the
				// distinct name exists because the spec names it.
				Name:      "remove",
				Usage:     "remove a node; --force skips the drain for dead hardware (§16.3)",
				ArgsUsage: "<node-id>",
				Flags:     append(connFlags(), &cli.BoolFlag{Name: "force", Usage: "conf-change a dead node out of every group immediately"}),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return removeNodeAction(ctx, cmd, "databox cluster remove <node-id> --force")
				},
			},
		},
	}
}

// adminCommand is the §16.4 automation pause/resume family:
// `databox admin rebalance|split|repair pause|resume|status`.
func adminCommand() *cli.Command {
	sub := func(target, what string) *cli.Command {
		return &cli.Command{
			Name:  target,
			Usage: "pause/resume " + what,
			Commands: []*cli.Command{
				{
					Name: "pause", Usage: "suspend " + what, Flags: connFlags(),
					Action: func(ctx context.Context, cmd *cli.Command) error {
						return adminPauseAction(ctx, cmd, target, "pause")
					},
				},
				{
					Name: "resume", Usage: "resume " + what, Flags: connFlags(),
					Action: func(ctx context.Context, cmd *cli.Command) error {
						return adminPauseAction(ctx, cmd, target, "resume")
					},
				},
			},
		}
	}
	return &cli.Command{
		Name:  "admin",
		Usage: "cluster automation controls: pause/resume (§16.4), manual shard splits (§15)",
		Commands: []*cli.Command{
			sub("rebalance", "replica placement moves"),
			sub("split", "shard splitting"),
			sub("repair", "blob repair & re-replication"),
			adminShardCommand(),
		},
	}
}

// adminShardCommand is the §15 manual-split family:
// `databox admin shard split <gid> [--at <key>]`.
func adminShardCommand() *cli.Command {
	return &cli.Command{
		Name:  "shard",
		Usage: "manual shard operations (§15)",
		Commands: []*cli.Command{{
			Name:      "split",
			Usage:     "hint the splitter to divide the shard served by raft group <gid>",
			ArgsUsage: "<gid>",
			Flags: append(connFlags(),
				&cli.StringFlag{Name: "at", Usage: "explicit split key, strictly inside the shard's range (default: the range's median key)"}),
			Action: func(ctx context.Context, cmd *cli.Command) error {
				if cmd.Args().Len() != 1 {
					return fmt.Errorf("usage: databox admin shard split <gid> [--at <key>]")
				}
				var gid uint64
				if _, err := fmt.Sscanf(cmd.Args().First(), "%d", &gid); err != nil {
					return fmt.Errorf("gid must be a number: %w", err)
				}
				c, err := dial(ctx, cmd)
				if err != nil {
					return err
				}
				var out struct {
					Note string `json:"note"`
				}
				if err := c.Raw(ctx, http.MethodPost, fmt.Sprintf("/api/v1/admin/shards/%d/split", gid),
					map[string]string{"at": cmd.String("at")}, &out); err != nil {
					return err
				}
				fmt.Println(out.Note)
				return nil
			},
		}},
	}
}

// adminPauseAction posts one pause/resume flag change.
func adminPauseAction(ctx context.Context, cmd *cli.Command, target, action string) error {
	c, err := dial(ctx, cmd)
	if err != nil {
		return err
	}
	if err := c.Raw(ctx, http.MethodPost, "/api/v1/admin/"+target+"/"+action, nil, nil); err != nil {
		return err
	}
	fmt.Printf("%s %sd (flag visible in `databox cluster status`)\n", target, action)
	return nil
}

// printStatus renders the human-readable cluster status with the safety
// verdicts front and center (§16.3).
func printStatus(r statusView) {
	fmt.Printf("Cluster %s\n\nNODES\n", r.ClusterID)
	fmt.Printf("  %-4s %-16s %-24s %-9s %-8s %s\n", "ID", "NAME", "ADDRESS", "STATE", "HEALTHY", "SAFE-TO-REMOVE")
	for _, n := range r.Nodes {
		fmt.Printf("  %-4d %-16s %-24s %-9s %-8v %v\n", n.ID, n.Name, n.Addr, n.State, n.Healthy, n.SafeToRemove)
	}
	fmt.Printf("\nSHARDS\n  %-4s %-24s %-24s %-6s %s\n", "ID", "START", "END", "GROUP", "STATE")
	for _, s := range r.Shards {
		end := s.End
		if end == "" {
			end = "(end)"
		}
		fmt.Printf("  %-4d %-24s %-24s %-6d %s\n", s.ID, s.Start, end, s.GID, s.State)
	}
	if len(r.Alerts) > 0 {
		fmt.Println("\nALERTS")
		for _, a := range r.Alerts {
			fmt.Printf("  [%s] %s\n", strings.ToUpper(a.Severity), a.Message)
		}
	}
	// Queued manual splits (§15): visible until the reconciler consumes
	// them — which it will not do while splitting is paused.
	if len(r.SplitHints) > 0 {
		fmt.Println("\nPENDING SPLIT HINTS")
		for _, h := range r.SplitHints {
			at := h.At
			if at == "" {
				at = "(median)"
			}
			fmt.Printf("  group %-6d at %-24s requested by %s at %s\n", h.GID, at, h.Actor, h.Created.Format(time.RFC3339))
		}
	}
	// Suspended automation is operationally loud (§16.4).
	var paused []string
	for target, p := range r.Paused {
		if p {
			paused = append(paused, target)
		}
	}
	sort.Strings(paused)
	if len(paused) > 0 {
		fmt.Printf("\nPAUSED AUTOMATION: %s  (resume with `databox admin <name> resume`)\n", strings.Join(paused, ", "))
	}
	fmt.Printf("\nsafe_to_proceed: %v", r.SafeToProceed)
	if !r.SafeToProceed {
		fmt.Print("  ← do NOT take down another node until this is true")
	}
	fmt.Println()
}

func userCommand() *cli.Command {
	return &cli.Command{
		Name:  "user",
		Usage: "manage users and S3 access keys (§7.3)",
		Commands: []*cli.Command{
			{
				Name: "create", ArgsUsage: "<name>", Usage: "create a user (password prompted, empty allowed)",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.Args().First()
					if name == "" {
						return fmt.Errorf("usage: databox user create <name>")
					}
					fmt.Fprintf(os.Stderr, "Password for new user %s (empty for none): ", name)
					pw, _ := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Fprintln(os.Stderr)
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					return c.Raw(ctx, http.MethodPost, "/api/v1/users",
						map[string]string{"name": name, "password": string(pw)}, nil)
				},
			},
			{
				Name: "passwd", ArgsUsage: "<name>", Usage: "set a user's password",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.Args().First()
					if name == "" {
						return fmt.Errorf("usage: databox user passwd <name>")
					}
					fmt.Fprintf(os.Stderr, "New password for %s: ", name)
					pw, _ := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Fprintln(os.Stderr)
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					return c.Raw(ctx, http.MethodPost, "/api/v1/users/"+name+"/password",
						map[string]string{"password": string(pw)}, nil)
				},
			},
			{
				Name: "delete", ArgsUsage: "<name>", Usage: "delete a user",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.Args().First()
					if name == "" {
						return fmt.Errorf("usage: databox user delete <name>")
					}
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					return c.Raw(ctx, http.MethodDelete, "/api/v1/users/"+name, nil, nil)
				},
			},
			{
				Name: "list", Usage: "list users and their grants",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					var out any
					if err := c.Raw(ctx, http.MethodGet, "/api/v1/users", nil, &out); err != nil {
						return err
					}
					return emit(cmd, out)
				},
			},
			{
				Name: "access-keys", ArgsUsage: "<name>",
				Usage: "mint a Databox API key for a user (gateway credentials, optionally scoped)",
				Flags: append(connFlags(),
					&cli.StringSliceFlag{Name: "scope", Usage: "limit the key to a prefix (repeatable); empty = the user's full grant extent"}),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					name := cmd.Args().First()
					if name == "" {
						return fmt.Errorf("usage: databox user access-keys <name> [--scope /prefix ...]")
					}
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					var out any
					body := map[string]any{"scopes": cmd.StringSlice("scope")}
					if err := c.Raw(ctx, http.MethodPost, "/api/v1/users/"+name+"/access-keys", body, &out); err != nil {
						return err
					}
					fmt.Fprintln(os.Stderr, "Store the secret now — it is shown exactly once.")
					return emit(cmd, out)
				},
			},
		},
	}
}

func grantCommand() *cli.Command {
	return &cli.Command{
		Name:  "grant",
		Usage: "manage prefix grants (§7.2): allow/deny verbs on key prefixes",
		Commands: []*cli.Command{
			{
				Name: "add", ArgsUsage: "<user> <allow|deny> <prefix> <verb,verb,...>",
				Usage: "e.g.: databox grant add sam allow /home/sam list,read,write",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if cmd.Args().Len() != 4 {
						return fmt.Errorf("usage: databox grant add <user> <allow|deny> <prefix> <verbs>")
					}
					user, effect, prefix, verbs := cmd.Args().Get(0), cmd.Args().Get(1), cmd.Args().Get(2), cmd.Args().Get(3)
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					return c.Raw(ctx, http.MethodPost, "/api/v1/users/"+user+"/grants",
						map[string]any{"prefix": prefix, "effect": effect, "verbs": strings.Split(verbs, ",")}, nil)
				},
			},
			{
				Name: "remove", ArgsUsage: "<user> [prefix]",
				Usage: "remove a grant — with no prefix, pick from the user's grants interactively",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if cmd.Args().Len() < 1 {
						return fmt.Errorf("usage: databox grant remove <user> [prefix]")
					}
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					user := cmd.Args().Get(0)
					// Explicit prefix given: remove directly (old form).
					if cmd.Args().Len() == 2 {
						return c.Raw(ctx, http.MethodDelete, "/api/v1/users/"+user+"/grants",
							map[string]string{"prefix": cmd.Args().Get(1)}, nil)
					}
					// Interactive: the server already knows the user's
					// grants — list them numbered and let the operator
					// pick, instead of making them retype anything.
					var out struct {
						Users []struct {
							Name   string `json:"name"`
							Grants []struct {
								Prefix string   `json:"prefix"`
								Effect string   `json:"effect"`
								Verbs  []string `json:"verbs"`
							} `json:"grants"`
						} `json:"users"`
					}
					if err := c.Raw(ctx, http.MethodGet, "/api/v1/users", nil, &out); err != nil {
						return err
					}
					for _, u := range out.Users {
						if u.Name != user {
							continue
						}
						if len(u.Grants) == 0 {
							return fmt.Errorf("user %q has no grants", user)
						}
						for i, g := range u.Grants {
							fmt.Printf("  [%d] %-5s %-30s %s\n", i+1, g.Effect, g.Prefix, strings.Join(g.Verbs, ","))
						}
						fmt.Fprint(os.Stderr, "remove which grant? [number, empty to abort]: ")
						var line string
						fmt.Scanln(&line)
						if line == "" {
							return fmt.Errorf("aborted")
						}
						var n int
						if _, err := fmt.Sscanf(line, "%d", &n); err != nil || n < 1 || n > len(u.Grants) {
							return fmt.Errorf("pick a number between 1 and %d", len(u.Grants))
						}
						g := u.Grants[n-1]
						if err := c.Raw(ctx, http.MethodDelete, "/api/v1/users/"+user+"/grants",
							map[string]string{"prefix": g.Prefix, "effect": g.Effect}, nil); err != nil {
							return err
						}
						fmt.Printf("removed %s %s on %s\n", g.Effect, strings.Join(g.Verbs, ","), g.Prefix)
						return nil
					}
					return fmt.Errorf("user %q not found", user)
				},
			},
			{
				Name: "list", ArgsUsage: "[user]", Usage: "list grants (all users, or one)",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					var out any
					if err := c.Raw(ctx, http.MethodGet, "/api/v1/users", nil, &out); err != nil {
						return err
					}
					return emit(cmd, out)
				},
			},
		},
	}
}

func backupCommand() *cli.Command {
	return &cli.Command{
		Name:  "backup",
		Usage: "cluster backup jobs to S3-compatible storage or SFTP (§17)",
		Commands: []*cli.Command{
			{
				Name: "create", Usage: "start a backup job (or resume one with --id; stored credentials are reused)",
				Flags: append(connFlags(),
					&cli.StringFlag{Name: "to", Usage: "destination: s3://bucket/prefix or sftp://user@host/path (optional with --id)"},
					&cli.StringFlag{Name: "id", Usage: "resume an existing job after a coordinator crash"},
					&cli.StringFlag{Name: "access-key", Usage: "S3 access key (or AWS_ACCESS_KEY_ID)", Sources: cli.EnvVars("AWS_ACCESS_KEY_ID")},
					&cli.StringFlag{Name: "secret-key", Usage: "S3 secret key (or AWS_SECRET_ACCESS_KEY)", Sources: cli.EnvVars("AWS_SECRET_ACCESS_KEY")},
					&cli.StringFlag{Name: "s3-endpoint", Usage: "custom S3 endpoint (MinIO etc.)"},
					&cli.StringFlag{Name: "sftp-password", Usage: "SFTP password", Sources: cli.EnvVars("DATABOX_SFTP_PASSWORD")},
				),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if cmd.String("to") == "" && cmd.String("id") == "" {
						return fmt.Errorf("either --to (new job) or --id (resume) is required")
					}
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					var out any
					if err := c.Raw(ctx, http.MethodPost, "/api/v1/backups", map[string]string{
						"to":            cmd.String("to"),
						"id":            cmd.String("id"),
						"access_key":    cmd.String("access-key"),
						"secret_key":    cmd.String("secret-key"),
						"s3_endpoint":   cmd.String("s3-endpoint"),
						"sftp_password": cmd.String("sftp-password"),
					}, &out); err != nil {
						return err
					}
					return emit(cmd, out)
				},
			},
			{
				Name: "status", ArgsUsage: "[id]", Usage: "show backup job progress (state, bytes, ETA, per-shard capture)",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					// One job: render the record with progress/bytes/ETA in
					// text mode. No id: list all jobs (structured output).
					if id := cmd.Args().First(); id != "" {
						var job server.JobRecord
						if err := c.Raw(ctx, http.MethodGet, "/api/v1/backups/"+id, nil, &job); err != nil {
							return err
						}
						if cmd.String("output") != "text" {
							return emit(cmd, job)
						}
						printJobStatus(job)
						return nil
					}
					var out any
					if err := c.Raw(ctx, http.MethodGet, "/api/v1/backups", nil, &out); err != nil {
						return err
					}
					return emit(cmd, out)
				},
			},
			{
				Name: "cancel", ArgsUsage: "<id>", Usage: "cancel a running backup job",
				Flags: connFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					id := cmd.Args().First()
					if id == "" {
						return fmt.Errorf("usage: databox backup cancel <id>")
					}
					c, err := dial(ctx, cmd)
					if err != nil {
						return err
					}
					return c.Raw(ctx, http.MethodPost, "/api/v1/backups/"+id+"/cancel", nil, nil)
				},
			},
		},
	}
}

func restoreCommand() *cli.Command {
	return &cli.Command{
		Name:  "restore",
		Usage: "restore a backup into an empty cluster (§17); user writes stay refused until the restore verifies",
		Flags: append(connFlags(),
			&cli.StringFlag{Name: "from", Usage: "backup URL: s3://bucket/prefix or sftp://user@host/path (optional with --id)"},
			&cli.StringFlag{Name: "id", Usage: "resume an existing restore job (stored credentials are reused)"},
			&cli.StringFlag{Name: "access-key", Sources: cli.EnvVars("AWS_ACCESS_KEY_ID")},
			&cli.StringFlag{Name: "secret-key", Sources: cli.EnvVars("AWS_SECRET_ACCESS_KEY")},
			&cli.StringFlag{Name: "s3-endpoint"},
			&cli.StringFlag{Name: "sftp-password", Sources: cli.EnvVars("DATABOX_SFTP_PASSWORD")},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.String("from") == "" && cmd.String("id") == "" {
				return fmt.Errorf("either --from (new restore) or --id (resume) is required")
			}
			c, err := dial(ctx, cmd)
			if err != nil {
				return err
			}
			var out any
			if err := c.Raw(ctx, http.MethodPost, "/api/v1/restore", map[string]string{
				"from":          cmd.String("from"),
				"id":            cmd.String("id"),
				"access_key":    cmd.String("access-key"),
				"secret_key":    cmd.String("secret-key"),
				"s3_endpoint":   cmd.String("s3-endpoint"),
				"sftp_password": cmd.String("sftp-password"),
			}, &out); err != nil {
				return err
			}
			return emit(cmd, out)
		},
	}
}

// printJobStatus renders `databox backup status <id>` in text mode: state,
// progress, bytes moved, observed-rate ETA, and per-shard capture health
// (§17 observable: "progress, bytes, ETA").
func printJobStatus(j server.JobRecord) {
	fmt.Printf("%s %s: %s", j.Kind, j.ID, j.State)
	if j.Phase != "" {
		fmt.Printf(" (%s)", j.Phase)
	}
	if j.Error != "" {
		fmt.Printf(" — %s", j.Error)
	}
	fmt.Println()
	fmt.Printf("  dest:     %s\n", j.Dest)
	fmt.Printf("  node:     %d\n", j.Node)
	fmt.Printf("  started:  %s\n", j.Started.Format(time.RFC3339))
	fmt.Printf("  progress: %3.0f%%  (%d kv pairs, %d blobs", j.Progress*100, j.KVPairs, j.Blobs)
	if len(j.Units) > 0 {
		done := 0
		for _, u := range j.Units {
			if u.Done {
				done++
			}
		}
		fmt.Printf(", %d/%d shard units", done, len(j.Units))
	}
	fmt.Println(")")
	fmt.Printf("  bytes:    %s", humanBytes(j.BytesCopied))
	if j.BytesTotal > 0 {
		fmt.Printf(" of %s", humanBytes(j.BytesTotal))
	}
	fmt.Println()
	if j.ETASeconds > 0 {
		fmt.Printf("  eta:      ~%s\n", (time.Duration(j.ETASeconds) * time.Second).Round(time.Second))
	}
	if j.Repins > 0 {
		fmt.Printf("  repins:   %d — write load outran MVCC history; the affected shards are captured at a later revision than the others (see docs/consistency.md)\n", j.Repins)
	}
	if j.Kind == server.JobRestore && j.State == server.JobDone {
		fmt.Printf("  verified: %v\n", j.Verified)
	}
	if j.Finished != nil {
		fmt.Printf("  finished: %s\n", j.Finished.Format(time.RFC3339))
	}
}

// humanBytes renders a byte count in the largest sensible binary unit.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func certificatesCommand() *cli.Command {
	return &cli.Command{
		Name:  "certificates",
		Usage: "static certificate tooling for production PKI opt-out (§6.4)",
		Commands: []*cli.Command{{
			Name:  "generate",
			Usage: "generate a self-signed certificate + key pair",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "cn", Value: "databox", Usage: "certificate common name"},
				&cli.StringSliceFlag{Name: "san", Usage: "additional DNS names / IPs (repeatable)"},
				&cli.StringFlag{Name: "validity", Value: "8760h", Usage: "validity duration (default 1 year)"},
				&cli.StringFlag{Name: "out", Value: ".", Usage: "output directory for tls.crt / tls.key"},
			},
			Action: func(_ context.Context, cmd *cli.Command) error {
				validity, err := time.ParseDuration(cmd.String("validity"))
				if err != nil {
					return err
				}
				certPEM, keyPEM, err := certs.SelfSigned(cmd.String("cn"), cmd.StringSlice("san"), validity)
				if err != nil {
					return err
				}
				dir := cmd.String("out")
				if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o644); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
					return err
				}
				fmt.Printf("wrote %s/tls.crt and %s/tls.key (CN=%s, valid %s)\n", dir, dir, cmd.String("cn"), validity)
				return nil
			},
		}},
	}
}

func pskCommand() *cli.Command {
	return &cli.Command{
		Name:  "psk",
		Usage: "pre-shared key tooling for node authentication (§6.2)",
		Commands: []*cli.Command{{
			Name:  "generate",
			Usage: "print a fresh random PSK to stdout",
			Flags: []cli.Flag{
				&cli.IntFlag{Name: "bit", Value: 512, Usage: "key length in bits: 128, 256, or 512"},
			},
			Action: func(_ context.Context, cmd *cli.Command) error {
				psk, err := certs.GeneratePSK(int(cmd.Int("bit")))
				if err != nil {
					return err
				}
				fmt.Println(psk)
				return nil
			},
		}},
	}
}

func recoverCommand() *cli.Command {
	return &cli.Command{
		Name:  "recover",
		Usage: "node-local recovery operations (require host access, §7.3)",
		Commands: []*cli.Command{{
			Name:  "root-password",
			Usage: "reset the root password via a node's local admin socket",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "data-dir", Value: "/var/lib/databox", Usage: "the node's data directory (locates admin.sock)"},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				fmt.Fprint(os.Stderr, "New root password: ")
				pw, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
				if err != nil || len(pw) == 0 {
					return fmt.Errorf("a non-empty password is required")
				}
				sock := filepath.Join(cmd.String("data-dir"), "admin.sock")
				// HTTP over the unix socket: possession of the socket file
				// IS the authorization (§7.3).
				httpc := http.Client{Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, "unix", sock)
					},
				}}
				body := strings.NewReader(fmt.Sprintf(`{"password":%q}`, string(pw)))
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local/recover-root", body)
				if err != nil {
					return err
				}
				resp, err := httpc.Do(req)
				if err != nil {
					return fmt.Errorf("cannot reach admin socket at %s (is the server running?): %w", sock, err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("recovery failed: %s", resp.Status)
				}
				fmt.Println("root password updated (the reset is recorded in the audit trail)")
				return nil
			},
		}},
	}
}
