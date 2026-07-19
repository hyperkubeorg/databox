// utils_cmd.go implements `databox utils`: client-side utilities for the
// processing layers. `utils sql` is an interactive SQL REPL that embeds
// the SQL layer engine (§13) in-process over an authenticated cluster
// client — no gateway required. `utils s3` is a minimal S3 client (SigV4)
// for exercising a running S3 gateway (§14). Endpoint defaults are the
// in-cluster service addresses a chart deployment creates, so both
// commands work flag-free from a pod in a cluster deployment.
package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/hyperkubeorg/databox/pkg/backup"
	sqlservice "github.com/hyperkubeorg/databox/pkg/service/sql"
)

func utilsCommand() *cli.Command {
	return &cli.Command{
		Name:  "utils",
		Usage: "client utilities for the processing layers: SQL REPL, S3 gateway client",
		Commands: []*cli.Command{
			utilsSQLCommand(),
			utilsS3Command(),
		},
	}
}

// --- utils sql ---------------------------------------------------------------

// sqlConnFlags are connFlags with the endpoint defaulting to the API
// service name a chart deployment creates (`databox:8443`); pass
// --endpoint for dev clusters.
func sqlConnFlags() []cli.Flag {
	flags := connFlags()
	for i, f := range flags {
		if sf, ok := f.(*cli.StringFlag); ok && sf.Name == "endpoint" {
			clone := *sf
			clone.Value = "databox:8443"
			clone.Usage = "cluster endpoint (host:port); default is the in-cluster service"
			flags[i] = &clone
		}
	}
	return flags
}

func utilsSQLCommand() *cli.Command {
	return &cli.Command{
		Name:  "sql",
		Usage: "SQL REPL with an embedded SQL layer (chai dialect, §13) — no gateway needed",
		Flags: append(sqlConnFlags(),
			&cli.StringFlag{Name: "database", Aliases: []string{"d"}, Usage: "database name (default: the authenticated username)"},
			&cli.StringFlag{Name: "execute", Aliases: []string{"e"}, Usage: "run these statements and exit"},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			c, err := dial(ctx, cmd)
			if err != nil {
				return err
			}
			db := cmd.String("database")
			if db == "" {
				// Same fallback the pg-wire gateway applies to a session
				// with no database parameter.
				db = cmd.String("user")
			}
			eng := sqlservice.NewEngine(c, db)
			if script := cmd.String("execute"); script != "" {
				results, err := eng.Exec(ctx, script)
				for _, res := range results {
					printSQLResult(cmd, res)
				}
				return err
			}
			return sqlREPL(ctx, cmd, eng, db)
		},
	}
}

// sqlREPL reads statements (terminated by ';') and executes them. Multi-
// line input accumulates until the terminator, psql-style. On a real
// terminal, lines come from x/term's line editor: arrow-key history (in
// memory only, last 100 lines — nothing touches disk), cursor movement,
// and the usual ^A/^E/^K/^U/^W editing keys.
func sqlREPL(ctx context.Context, cmd *cli.Command, eng *sqlservice.Engine, db string) error {
	fmt.Printf("databox sql — database %q on %s; statements end with ';', exit to quit\n", db, cmd.String("endpoint"))
	readLine := pipeLineReader()
	setPrompt := func(string) {}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		t := term.NewTerminal(struct {
			io.Reader
			io.Writer
		}{os.Stdin, os.Stdout}, "sql> ")
		setPrompt = t.SetPrompt
		// Raw mode only while editing a line: while a statement runs the
		// terminal is cooked, so ^C still interrupts the process (and a
		// long query) instead of being swallowed.
		readLine = func() (string, error) {
			old, err := term.MakeRaw(fd)
			if err != nil {
				return "", err
			}
			defer term.Restore(fd, old)
			return t.ReadLine()
		}
	}
	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			setPrompt("sql> ")
		} else {
			setPrompt("  -> ")
		}
		line, err := readLine()
		if err != nil {
			fmt.Println()
			return nil // EOF / ^D ends the session cleanly
		}
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 {
			if trimmed == "" {
				continue
			}
			if trimmed == "exit" || trimmed == "quit" {
				return nil
			}
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if !strings.HasSuffix(trimmed, ";") {
			continue
		}
		stmt := buf.String()
		buf.Reset()
		results, err := eng.Exec(ctx, stmt)
		for _, res := range results {
			printSQLResult(cmd, res)
		}
		if err != nil {
			fmt.Println("error:", err)
		}
	}
}

// pipeLineReader reads plain lines from stdin — the non-TTY path
// (scripts, pipes), where prompts and line editing have no meaning.
func pipeLineReader() func() (string, error) {
	rd := newStdinLines()
	return rd.next
}

func newStdinLines() *lineReader { return newLineReader(os.Stdin) }

type lineReader struct {
	src io.Reader
	buf []byte
}

func newLineReader(src io.Reader) *lineReader { return &lineReader{src: src} }

// next returns one line without the trailing newline, or an error at EOF.
func (l *lineReader) next() (string, error) {
	var line []byte
	one := make([]byte, 1)
	for {
		n, err := l.src.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return string(line), nil
			}
			line = append(line, one[0])
			continue
		}
		if err != nil {
			if len(line) > 0 {
				return string(line), nil
			}
			return "", err
		}
	}
}

// printSQLResult renders one statement result in the session's output
// format: text draws an aligned table, json/yaml emit the result struct.
func printSQLResult(cmd *cli.Command, res sqlservice.ExecResult) {
	if cmd.String("output") != "text" {
		_ = emit(cmd, struct {
			Tag     string      `json:"tag"`
			Columns []string    `json:"columns,omitempty"`
			Rows    [][]*string `json:"rows,omitempty"`
		}{res.Tag, res.Columns, res.Rows})
		return
	}
	if len(res.Columns) == 0 {
		fmt.Println(res.Tag)
		return
	}
	widths := make([]int, len(res.Columns))
	for i, c := range res.Columns {
		widths[i] = len(c)
	}
	cell := func(v *string) string {
		if v == nil {
			return "NULL"
		}
		return *v
	}
	for _, row := range res.Rows {
		for i, v := range row {
			if i < len(widths) && len(cell(v)) > widths[i] {
				widths[i] = len(cell(v))
			}
		}
	}
	var head, rule strings.Builder
	for i, c := range res.Columns {
		if i > 0 {
			head.WriteString(" | ")
			rule.WriteString("-+-")
		}
		head.WriteString(fmt.Sprintf("%-*s", widths[i], c))
		rule.WriteString(strings.Repeat("-", widths[i]))
	}
	fmt.Println(head.String())
	fmt.Println(rule.String())
	for _, row := range res.Rows {
		var b strings.Builder
		for i, v := range row {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(fmt.Sprintf("%-*s", widths[i], cell(v)))
		}
		fmt.Println(b.String())
	}
	fmt.Printf("(%d rows)\n", len(res.Rows))
}

// --- utils s3 ----------------------------------------------------------------

// s3utilFlags are shared by every `utils s3` subcommand. The endpoint
// default is the gateway service name a chart deployment creates.
func s3utilFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "endpoint", Value: "http://databox-gateway-s3:9000", Usage: "S3 gateway endpoint; default is the in-cluster service", Sources: cli.EnvVars("DATABOX_S3_ENDPOINT")},
		&cli.StringFlag{Name: "access-key", Usage: "access key ID (mint with `databox user access-keys <name>`)", Sources: cli.EnvVars("DATABOX_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID")},
		&cli.StringFlag{Name: "secret-key", Usage: "secret access key", Sources: cli.EnvVars("DATABOX_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY")},
		&cli.StringFlag{Name: "region", Value: "us-east-1", Usage: "SigV4 signing region", Sources: cli.EnvVars("AWS_REGION")},
		&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Value: "text", Usage: "output format: text|json|yaml"},
	}
}

func utilsS3Command() *cli.Command {
	return &cli.Command{
		Name:  "s3",
		Usage: "minimal S3 client for exercising the S3 gateway (§14)",
		Commands: []*cli.Command{
			{
				Name:      "ls",
				Usage:     "list buckets, or objects under s3://bucket[/prefix]",
				ArgsUsage: "[s3://bucket[/prefix]]",
				Flags: append(s3utilFlags(),
					&cli.BoolFlag{Name: "recursive", Usage: "list every key instead of grouping by '/'"},
				),
				Action: s3LsAction,
			},
			{
				Name:      "mb",
				Usage:     "create a bucket",
				ArgsUsage: "s3://bucket",
				Flags:     s3utilFlags(),
				Action:    s3BucketAction(http.MethodPut, "created"),
			},
			{
				Name:      "rb",
				Usage:     "delete a bucket",
				ArgsUsage: "s3://bucket",
				Flags:     s3utilFlags(),
				Action:    s3BucketAction(http.MethodDelete, "removed"),
			},
			{
				Name:      "cp",
				Usage:     "copy a file to/from the gateway ('-' is stdin/stdout; a leading '-' needs '--' first)",
				ArgsUsage: "<local|-|s3://bucket/key> <local|-|s3://bucket/key>",
				Flags:     s3utilFlags(),
				Action:    s3CpAction,
			},
			{
				Name:      "rm",
				Usage:     "delete an object",
				ArgsUsage: "s3://bucket/key",
				Flags:     s3utilFlags(),
				Action:    s3RmAction,
			},
			{
				Name:      "stat",
				Usage:     "object metadata without the body (HEAD)",
				ArgsUsage: "s3://bucket/key",
				Flags:     s3utilFlags(),
				Action:    s3StatAction,
			},
		},
	}
}

// s3util performs signed requests against one gateway endpoint.
type s3util struct {
	base   *url.URL
	keyID  string
	secret string
	region string
	hc     *http.Client
}

func newS3Util(cmd *cli.Command) (*s3util, error) {
	raw := cmd.String("endpoint")
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	key, secret := cmd.String("access-key"), cmd.String("secret-key")
	if key == "" || secret == "" {
		return nil, fmt.Errorf("credentials required: mint a key pair with `databox user access-keys <name>` and pass --access-key/--secret-key (or the env vars)")
	}
	return &s3util{
		base:   base,
		keyID:  key,
		secret: secret,
		region: cmd.String("region"),
		hc:     &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

// do signs and sends one request. body may be nil; size < 0 leaves
// Content-Length unset.
func (s *s3util) do(ctx context.Context, method, path string, query url.Values, body io.Reader, size int64, contentType string) (*http.Response, error) {
	u := *s.base
	u.Path = path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hash := backup.EmptyPayloadHash
	if body != nil {
		hash = backup.UnsignedPayload
	}
	backup.SignRequestV4(req, s.keyID, s.secret, s.region, "s3", hash, time.Now())
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		var se struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(raw, &se) == nil && se.Code != "" {
			return nil, fmt.Errorf("%s: %s (HTTP %d)", se.Code, se.Message, resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp, nil
}

// parseS3URI splits "s3://bucket/key…" into bucket and key.
func parseS3URI(s string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(s, "s3://")
	if !ok || rest == "" {
		return "", "", fmt.Errorf("%q is not an s3://bucket[/key] URI", s)
	}
	bucket, key, _ = strings.Cut(rest, "/")
	return bucket, key, nil
}

func s3LsAction(ctx context.Context, cmd *cli.Command) error {
	s, err := newS3Util(cmd)
	if err != nil {
		return err
	}
	if cmd.Args().Len() == 0 {
		return s.listBuckets(ctx, cmd)
	}
	bucket, prefix, err := parseS3URI(cmd.Args().First())
	if err != nil {
		return err
	}
	return s.listObjects(ctx, cmd, bucket, prefix)
}

func (s *s3util) listBuckets(ctx context.Context, cmd *cli.Command) error {
	resp, err := s.do(ctx, http.MethodGet, "/", nil, nil, -1, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Buckets []struct {
			Name         string `xml:"Name" json:"name"`
			CreationDate string `xml:"CreationDate" json:"creation_date"`
		} `xml:"Buckets>Bucket" json:"buckets"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if cmd.String("output") != "text" {
		return emit(cmd, out)
	}
	for _, b := range out.Buckets {
		fmt.Printf("%s  s3://%s\n", b.CreationDate, b.Name)
	}
	return nil
}

// s3Object is one listed key.
type s3Object struct {
	Key          string `xml:"Key" json:"key"`
	Size         int64  `xml:"Size" json:"size"`
	LastModified string `xml:"LastModified" json:"last_modified"`
	ETag         string `xml:"ETag" json:"etag"`
}

func (s *s3util) listObjects(ctx context.Context, cmd *cli.Command, bucket, prefix string) error {
	var objects []s3Object
	var prefixes []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if !cmd.Bool("recursive") {
			q.Set("delimiter", "/")
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "/"+bucket, q, nil, -1, "")
		if err != nil {
			return err
		}
		var page struct {
			Contents       []s3Object `xml:"Contents"`
			CommonPrefixes []struct {
				Prefix string `xml:"Prefix"`
			} `xml:"CommonPrefixes"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return err
		}
		objects = append(objects, page.Contents...)
		for _, p := range page.CommonPrefixes {
			prefixes = append(prefixes, p.Prefix)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	if cmd.String("output") != "text" {
		return emit(cmd, struct {
			Prefixes []string   `json:"prefixes,omitempty"`
			Objects  []s3Object `json:"objects"`
		}{prefixes, objects})
	}
	for _, p := range prefixes {
		fmt.Printf("%26s  %s\n", "PRE", p)
	}
	for _, o := range objects {
		fmt.Printf("%s %10d  %s\n", o.LastModified, o.Size, o.Key)
	}
	return nil
}

// s3BucketAction covers mb (PUT) and rb (DELETE): one bucket-level call.
func s3BucketAction(method, verb string) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if cmd.Args().Len() != 1 {
			return fmt.Errorf("exactly one s3://bucket argument required")
		}
		bucket, key, err := parseS3URI(cmd.Args().First())
		if err != nil {
			return err
		}
		if key != "" {
			return fmt.Errorf("bucket URI must not contain a key: s3://%s", bucket)
		}
		s, err := newS3Util(cmd)
		if err != nil {
			return err
		}
		resp, err := s.do(ctx, method, "/"+bucket, nil, nil, -1, "")
		if err != nil {
			return err
		}
		drain(resp)
		fmt.Printf("%s s3://%s\n", verb, bucket)
		return nil
	}
}

func s3CpAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 2 {
		// A leading bare "-" is eaten by flag parsing; "--" stops it.
		return fmt.Errorf("usage: cp <src> <dst> with exactly one s3:// side (stdin upload: cp -- - s3://bucket/key)")
	}
	src, dst := cmd.Args().Get(0), cmd.Args().Get(1)
	srcRemote := strings.HasPrefix(src, "s3://")
	dstRemote := strings.HasPrefix(dst, "s3://")
	if srcRemote == dstRemote {
		return fmt.Errorf("exactly one of src/dst must be an s3:// URI")
	}
	s, err := newS3Util(cmd)
	if err != nil {
		return err
	}
	if dstRemote {
		return s.upload(ctx, src, dst)
	}
	return s.download(ctx, src, dst)
}

func (s *s3util) upload(ctx context.Context, src, dst string) error {
	bucket, key, err := parseS3URI(dst)
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("destination needs a key: s3://%s/<key>", bucket)
	}
	var body *os.File
	if src == "-" {
		// Spool stdin so the request carries a Content-Length (the
		// gateway, like S3, needs the size up front).
		tmp, err := os.CreateTemp("", "databox-s3-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		if _, err := io.Copy(tmp, os.Stdin); err != nil {
			return err
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		body = tmp
	} else {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		body = f
	}
	info, err := body.Stat()
	if err != nil {
		return err
	}
	contentType := mime.TypeByExtension(filepath.Ext(key))
	resp, err := s.do(ctx, http.MethodPut, "/"+bucket+"/"+key, nil, body, info.Size(), contentType)
	if err != nil {
		return err
	}
	drain(resp)
	fmt.Printf("uploaded %d bytes to s3://%s/%s\n", info.Size(), bucket, key)
	return nil
}

func (s *s3util) download(ctx context.Context, src, dst string) error {
	bucket, key, err := parseS3URI(src)
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("source needs a key: s3://%s/<key>", bucket)
	}
	resp, err := s.do(ctx, http.MethodGet, "/"+bucket+"/"+key, nil, nil, -1, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out io.Writer = os.Stdout
	if dst != "-" {
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	n, err := io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	if dst != "-" {
		fmt.Printf("downloaded %d bytes to %s\n", n, dst)
	}
	return nil
}

func s3RmAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("exactly one s3://bucket/key argument required")
	}
	bucket, key, err := parseS3URI(cmd.Args().First())
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("rm needs a key (use rb for buckets)")
	}
	s, err := newS3Util(cmd)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, "/"+bucket+"/"+key, nil, nil, -1, "")
	if err != nil {
		return err
	}
	drain(resp)
	fmt.Printf("removed s3://%s/%s\n", bucket, key)
	return nil
}

func s3StatAction(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("exactly one s3://bucket/key argument required")
	}
	bucket, key, err := parseS3URI(cmd.Args().First())
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("stat needs a key")
	}
	s, err := newS3Util(cmd)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodHead, "/"+bucket+"/"+key, nil, nil, -1, "")
	if err != nil {
		return err
	}
	drain(resp)
	return emit(cmd, struct {
		Key          string `json:"key"`
		Size         int64  `json:"size"`
		ContentType  string `json:"content_type"`
		ETag         string `json:"etag,omitempty"`
		LastModified string `json:"last_modified,omitempty"`
	}{
		Key:          "s3://" + bucket + "/" + key,
		Size:         resp.ContentLength,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	})
}

// drain discards and closes a response body so connections are reused.
func drain(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
