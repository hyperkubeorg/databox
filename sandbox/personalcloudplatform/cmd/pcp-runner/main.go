// Command pcp-runner is a Personal Cloud Platform Build Runner (Draft
// 003): a separate static binary — never the pcp binary — that runs
// repository build pipelines in containers on behalf of a paired PCP.
// It pairs cryptographically (like cloudferry/postoffice), then DIALS PCP
// (§6.2: runners sit behind firewalls) to hold a persistent tunnel over
// which PCP pushes config and jobs. One binary, two executors:
//
//	pcp-runner setup --data-dir /var/lib/pcp-runner [--kind k8s|baremetal]
//	    pair with a PCP (paste the admin console's setup code, paste the
//	    printed completion code back). One runner, one PCP.
//
//	pcp-runner run --data-dir /var/lib/pcp-runner [--executor auto|k8s|baremetal]
//	    dial PCP and serve jobs. The executor auto-detects from the paired
//	    kind (in-cluster → k8s Pods, else podman/docker containers).
//
// There is deliberately almost nothing to configure: the data dir and, at
// most, the executor override. Concurrency cap and execution profile all
// arrive from PCP over the paired channel — authority flows PCP → runner.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/runner"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "setup":
		fs := flag.NewFlagSet("setup", flag.ExitOnError)
		dataDir := fs.String("data-dir", "/var/lib/pcp-runner", "state directory (identity, pairing, TLS)")
		kind := fs.String("kind", "", "executor kind to report (k8s|baremetal; default auto-detect)")
		_ = fs.Parse(args)
		if err := RunSetup(*dataDir, *kind, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", err)
			os.Exit(1)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dataDir := fs.String("data-dir", "/var/lib/pcp-runner", "state directory (identity, pairing, TLS)")
		executor := fs.String("executor", "auto", "executor: auto|k8s|baremetal (auto = the paired kind)")
		namespace := fs.String("namespace", os.Getenv("POD_NAMESPACE"), "k8s namespace for step Pods (k8s executor)")
		_ = fs.Parse(args)
		if err := run(*dataDir, *executor, *namespace); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "version":
		fmt.Println("pcp-runner", version)
	default:
		usage()
		os.Exit(2)
	}
}

const version = "0.1"

// run loads the paired identity, builds the executor, and serves jobs
// until a signal arrives.
func run(dataDir, executorKind, namespace string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	st, err := Load(dataDir)
	if err != nil {
		return err
	}

	kind := executorKind
	if kind == "" || kind == "auto" {
		kind = st.Identity.Kind
	}
	exec, err := buildExecutor(kind, namespace)
	if err != nil {
		return err
	}
	log.Info("pcp-runner starting", "executor", exec.Kind(), "endpoint", st.PCP.Endpoint, "runner", st.PCP.RunnerID)

	client, err := runner.New(runner.Config{
		Endpoint:      st.PCP.Endpoint,
		RunnerID:      st.PCP.RunnerID,
		ControlPriv:   st.Identity.ControlPriv,
		PCPControlPub: st.PCP.ControlPub,
		SealPriv:      st.Identity.SealPriv,
		TLSCert:       st.TLSCert,
		Executor:      exec,
		Log:           log,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		cancel()
	}()
	client.Run(ctx)
	return nil
}

// buildExecutor constructs the chosen executor.
func buildExecutor(kind, namespace string) (runner.Executor, error) {
	switch kind {
	case buildproto.KindK8s:
		return runner.NewK8s(namespace)
	case buildproto.KindBareMetal:
		return runner.NewBareMetal()
	default:
		return nil, fmt.Errorf("unknown executor %q (want k8s or baremetal)", kind)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  pcp-runner setup [--data-dir DIR] [--kind k8s|baremetal]
  pcp-runner run   [--data-dir DIR] [--executor auto|k8s|baremetal] [--namespace NS]
  pcp-runner version`)
}
