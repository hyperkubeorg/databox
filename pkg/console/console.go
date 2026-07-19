// Package console implements the interactive REPL behind `databox console`
// (§5.1): a line-oriented shell over the API client with
// stable session state (connection + auth persist across commands),
// non-interactive execution via -e, and json/yaml output modes.
//
// Command vocabulary (type `help` in the REPL for the same list):
//
//	get <key>                         linearizable read
//	set <key> <value…>                write (value is the rest of the line)
//	del <key>                         delete
//	list <prefix> [limit]             page keys under a prefix
//	watch <prefix>                    stream changes until Ctrl-C
//	putblob <key> <file>              upload a file as a blob
//	getblob <key> <file>              download a blob to a file
//	lock <resource> [ttl]             exclusive lock, prints fencing token
//	unlock <resource>                 release
//	locks <resource>                  inspect lock state
//	users / user-create <name> [pw]   identity management
//	grant <user> <allow|deny> <prefix> <verbs>
//	status                            cluster status
//	output <text|json|yaml>           switch output format
//	help / exit
package console

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/kv"
)

// Session is one console run: a connected client plus display settings.
type Session struct {
	Client *client.Client
	Output string // text | json | yaml
	Out    io.Writer
}

// Run executes the REPL. When script is non-empty (-e), its lines run
// sequentially and the console exits — same session semantics, no prompt.
func (s *Session) Run(ctx context.Context, script string) error {
	if s.Out == nil {
		s.Out = os.Stdout
	}
	if script != "" {
		for _, line := range strings.Split(script, "\n") {
			for _, cmd := range strings.Split(line, ";") {
				cmd = strings.TrimSpace(cmd)
				if cmd == "" {
					continue
				}
				if err := s.eval(ctx, cmd); err != nil {
					return fmt.Errorf("%q: %w", cmd, err)
				}
			}
		}
		return nil
	}
	fmt.Fprintln(s.Out, "databox console — type help for commands, exit to quit")
	rd := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprint(s.Out, "databox> ")
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil // EOF ends the session cleanly
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if err := s.eval(ctx, line); err != nil {
			fmt.Fprintln(s.Out, "error:", err)
		}
	}
}

// print renders a value according to the session's output mode.
func (s *Session) print(v any) {
	switch s.Output {
	case "json":
		enc := json.NewEncoder(s.Out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
	case "yaml":
		raw, _ := yaml.Marshal(v)
		s.Out.Write(raw)
	default:
		switch t := v.(type) {
		case string:
			fmt.Fprintln(s.Out, t)
		default:
			raw, _ := json.MarshalIndent(v, "", "  ")
			fmt.Fprintln(s.Out, string(raw))
		}
	}
}

// eval executes one console command line.
func (s *Session) eval(ctx context.Context, line string) error {
	fields := strings.Fields(line)
	cmd, args := fields[0], fields[1:]
	need := func(n int, usage string) error {
		if len(args) < n {
			return fmt.Errorf("usage: %s", usage)
		}
		return nil
	}
	switch cmd {
	case "help":
		s.print(strings.TrimSpace(helpText))
		return nil

	case "output":
		if err := need(1, "output <text|json|yaml>"); err != nil {
			return err
		}
		switch args[0] {
		case "text", "json", "yaml":
			s.Output = args[0]
			return nil
		}
		return fmt.Errorf("unknown format %q", args[0])

	case "get":
		if err := need(1, "get <key>"); err != nil {
			return err
		}
		e, found, err := s.Client.Get(ctx, args[0])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("not found")
		}
		if s.Output == "text" {
			s.print(string(e.Value))
			return nil
		}
		s.print(map[string]any{"key": e.Key, "value": string(e.Value), "rev": e.Rev, "blob": e.Blob})
		return nil

	case "set":
		if err := need(2, "set <key> <value…>"); err != nil {
			return err
		}
		// The value is everything after the key, whitespace preserved.
		value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, cmd)), args[0]))
		rev, err := s.Client.Set(ctx, args[0], []byte(value))
		if err != nil {
			return err
		}
		s.print(map[string]any{"rev": rev})
		return nil

	case "del", "delete":
		if err := need(1, "del <key>"); err != nil {
			return err
		}
		if err := s.Client.Delete(ctx, args[0]); err != nil {
			return err
		}
		s.print("deleted")
		return nil

	case "list", "ls":
		if err := need(1, "list <prefix> [limit]"); err != nil {
			return err
		}
		limit := 100
		if len(args) > 1 {
			limit, _ = strconv.Atoi(args[1])
		}
		entries, next, err := s.Client.List(ctx, args[0], "", limit)
		if err != nil {
			return err
		}
		if s.Output == "text" {
			for _, e := range entries {
				marker := ""
				if e.Blob {
					marker = " (blob)"
				}
				fmt.Fprintf(s.Out, "%-40s rev=%d%s\n", e.Key, e.Rev, marker)
			}
			if next != "" {
				fmt.Fprintf(s.Out, "… more (cursor %s)\n", next)
			}
			return nil
		}
		s.print(map[string]any{"entries": entries, "next_cursor": next})
		return nil

	case "watch":
		if err := need(1, "watch <prefix>"); err != nil {
			return err
		}
		fmt.Fprintln(s.Out, "watching", args[0], "(Ctrl-C to stop)")
		return s.Client.Watch(ctx, args[0], 0, func(ev kv.Event) error {
			s.print(map[string]any{"rev": ev.Rev, "type": ev.Type, "key": ev.Key, "value": string(ev.Value)})
			return nil
		})

	case "putblob":
		if err := need(2, "putblob <key> <file>"); err != nil {
			return err
		}
		f, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		if err := s.Client.PutBlob(ctx, args[0], f, ""); err != nil {
			return err
		}
		s.print("uploaded")
		return nil

	case "appendblob":
		if err := need(2, "appendblob <key> <file>"); err != nil {
			return err
		}
		f, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		if err := s.Client.AppendBlob(ctx, args[0], f); err != nil {
			return err
		}
		s.print("appended")
		return nil

	case "getblob":
		if err := need(2, "getblob <key> <file>"); err != nil {
			return err
		}
		f, err := os.Create(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		if err := s.Client.GetBlob(ctx, args[0], f); err != nil {
			return err
		}
		s.print("downloaded to " + args[1])
		return nil

	case "lock":
		if err := need(1, "lock <resource> [ttl e.g. 30s]"); err != nil {
			return err
		}
		ttl := 30 * time.Second
		if len(args) > 1 {
			d, err := time.ParseDuration(args[1])
			if err != nil {
				return err
			}
			ttl = d
		}
		fencing, err := s.Client.LockAcquire(ctx, args[0], "exclusive", ttl)
		if err != nil {
			return err
		}
		s.print(map[string]any{"fencing": fencing})
		return nil

	case "unlock":
		if err := need(1, "unlock <resource>"); err != nil {
			return err
		}
		if err := s.Client.LockRelease(ctx, args[0]); err != nil {
			return err
		}
		s.print("released")
		return nil

	case "locks":
		if err := need(1, "locks <resource>"); err != nil {
			return err
		}
		var out any
		if err := s.Client.Raw(ctx, http.MethodGet, "/api/v1/locks/"+args[0], nil, &out); err != nil {
			return err
		}
		s.print(out)
		return nil

	case "users":
		var out any
		if err := s.Client.Raw(ctx, http.MethodGet, "/api/v1/users", nil, &out); err != nil {
			return err
		}
		s.print(out)
		return nil

	case "user-create":
		if err := need(1, "user-create <name> [password]"); err != nil {
			return err
		}
		pw := ""
		if len(args) > 1 {
			pw = args[1]
		}
		if err := s.Client.Raw(ctx, http.MethodPost, "/api/v1/users",
			map[string]string{"name": args[0], "password": pw}, nil); err != nil {
			return err
		}
		s.print("created")
		return nil

	case "grant":
		if err := need(4, "grant <user> <allow|deny> <prefix> <verbs,comma,separated>"); err != nil {
			return err
		}
		if err := s.Client.Raw(ctx, http.MethodPost, "/api/v1/users/"+args[0]+"/grants",
			map[string]any{"prefix": args[2], "effect": args[1], "verbs": strings.Split(args[3], ",")}, nil); err != nil {
			return err
		}
		s.print("granted")
		return nil

	case "status":
		var out any
		if err := s.Client.Raw(ctx, http.MethodGet, "/api/v1/cluster/status", nil, &out); err != nil {
			return err
		}
		s.print(out)
		return nil

	default:
		return fmt.Errorf("unknown command %q (try help)", cmd)
	}
}

const helpText = `
get <key>                        set <key> <value…>
del <key>                        list <prefix> [limit]
watch <prefix>                   putblob <key> <file>
appendblob <key> <file>          getblob <key> <file>
lock <resource> [ttl]
unlock <resource>                locks <resource>
users                            user-create <name> [password]
grant <user> <allow|deny> <prefix> <verbs>
status                           output <text|json|yaml>
help                             exit
`
