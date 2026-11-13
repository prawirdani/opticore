// Command opticore is the Opticore daemon and its operator CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/api"
	"github.com/prawirdani/opticore/internal/config"
	"github.com/prawirdani/opticore/internal/cwmp"
	"github.com/prawirdani/opticore/internal/hook"
	"github.com/prawirdani/opticore/internal/store"
	"github.com/prawirdani/opticore/internal/tui"
	"github.com/prawirdani/opticore/internal/wake"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `usage: opticore [-env-file path] <command> [args]

  The daemon reads ./.env, or .env beside the binary, unless -env-file (or
  $OPTICORE_ENV_FILE) names another file. Real environment variables win.

  serve                                 run the daemon in the foreground
  migrate                               apply database migrations
  tui                                   operator console (also the no-arg default)
  devices [--json] [--serial P] [--class C]
  device <serial|id> [--json] [--prefix P]
  tasks <serial|id> [--json]
  task <serial|id> get [path]...        no path: every parameter
  task <serial|id> set <path> <value> [type]
  task <serial|id> reboot
  task <serial|id> ping <host>          IPPingDiagnostics; result after 8 DIAGNOSTICS COMPLETE
  task <serial|id> revert <taskId>       re-set the values the task overwrote
  jobs [--json]
  job <id> [--json]
  job cancel <id>
  job revert <id>                       revert every done set task of the job
  wake <serial|id>                      connection request only
  summon <serial|id>                    wake plus a whole-tree get (GenieACS Summon)
  version                               print the build version
`

func main() {
	args, path, err := splitEnvFile(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "opticore:", err)
		os.Exit(2)
	}
	if _, err := config.LoadEnvFile(path); err != nil {
		fmt.Fprintln(os.Stderr, "opticore:", err)
		os.Exit(1)
	}
	if len(args) < 1 {
		if !isTerminal() {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		args = []string{"tui"}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd, args := args[0], args[1:]; cmd {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = cmdMigrate(ctx)
	case "tui":
		err = cmdTUI(ctx)
	case "devices":
		err = cmdDevices(ctx, args)
	case "device":
		err = cmdDevice(ctx, args)
	case "tasks":
		err = cmdTasks(ctx, args)
	case "task":
		err = cmdTask(ctx, args)
	case "jobs":
		err = cmdJobs(ctx, args)
	case "job":
		err = cmdJob(ctx, args)
	case "wake":
		err = cmdWake(ctx, args)
	case "summon":
		err = cmdSummon(ctx, args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		if store.IsMissingTable(err) {
			err = fmt.Errorf("%w (database schema missing: run `opticore migrate`)", err)
		}
		fmt.Fprintln(os.Stderr, "opticore:", err)
		os.Exit(1)
	}
}

// splitEnvFile removes -env-file/--env-file from the arguments, so it can sit
// before or after the subcommand. The last occurrence wins.
func splitEnvFile(args []string) (rest []string, path string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-env-file" || a == "--env-file":
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s needs a path", a)
			}
			i++
			path = args[i]
		case strings.HasPrefix(a, "-env-file=") || strings.HasPrefix(a, "--env-file="):
			path = a[strings.Index(a, "=")+1:]
			if path == "" {
				return nil, "", fmt.Errorf("%s needs a path", a)
			}
		default:
			rest = append(rest, a)
		}
	}
	return rest, path, nil
}

func serve(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	logger := log.Default()

	if err := ensureDatabase(ctx, cfg.DBDSN, logger); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer st.Close()
	st.TaskExpiry = cfg.TaskExpiry
	if _, err := st.AcquireLeader(ctx); err != nil {
		return err
	}
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if v, err := st.SchemaVersion(ctx); err == nil {
		logger.Printf("database schema version=%d", v)
	}
	logger.Printf("cwmp device security allow_cidrs=%d cr_url_match_source=%v device_auth=%v",
		len(cfg.CWMPAllowCIDRs), cfg.CWMPCRMatchSource, cfg.CWMPAuthUsername != "")

	svc := &acs.Service{Store: st, Waker: wake.New(cfg.CWMPCRTimeout, logger), Logger: logger,
		PumpWindow: cfg.CWMPSessionTimeout}
	sessions := &acs.Sessions{
		Store: st, Svc: svc, Logger: logger,
		GPVBatch: cfg.CWMPGPVBatch, MaxAttempts: cfg.TaskMaxAttempts,
		CRMatchSource: cfg.CWMPCRMatchSource, AutoTreeRead: cfg.AutoTreeRead,
		HookEvents: cfg.HookEvents, HookPrefetch: cfg.HookPrefetch, HookTimeout: cfg.HookTimeout,
	}
	if cfg.HookURL != "" {
		hc, err := hook.New(cfg.HookURL, cfg.HookStepTimeout)
		if err != nil {
			return err
		}
		sessions.Hook = hc
		logger.Printf("inform hook configured url=%q events=%v step_timeout=%v timeout=%v",
			cfg.HookURL, cfg.HookEvents, cfg.HookStepTimeout, cfg.HookTimeout)
	}
	cs := &cwmp.Server{
		Handler:        sessions,
		Logger:         logger,
		MaxBody:        cfg.CWMPMaxBody,
		SessionTimeout: cfg.CWMPSessionTimeout,
		MaxSessions:    cfg.CWMPMaxSessions,
		Allow:          cfg.CWMPAllowCIDRs,
	}
	if cfg.CWMPAuthUsername != "" {
		cs.Auth = cwmp.NewAuth(cfg.CWMPAuthUsername, cfg.CWMPAuthPassword)
	}
	cs.Start()
	defer cs.Stop()

	cwmpMux := http.NewServeMux()
	cwmpMux.Handle("/", cs)
	cwmpMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		health(w, r, st)
	})
	srv := &http.Server{
		Addr:              cfg.CWMPAddr,
		Handler:           cwmpMux,
		ConnContext:       cs.ConnContext,
		ReadHeaderTimeout: cfg.CWMPRequestTimeout,
		ReadTimeout:       cfg.CWMPRequestTimeout,
		WriteTimeout:      cfg.CWMPRequestTimeout,
		IdleTimeout:       cfg.CWMPSessionTimeout,
	}

	apiSrv := api.New(st, svc, logger)
	apiMux := http.NewServeMux()
	apiMux.Handle("/", apiSrv.Handler())
	apiMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		health(w, r, st)
	})
	apiHTTP := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           apiMux,
		ReadHeaderTimeout: cfg.CWMPRequestTimeout,
		IdleTimeout:       cfg.CWMPSessionTimeout,
	}

	janitorCtx, cancelJanitor := context.WithCancel(ctx)
	defer cancelJanitor()
	go janitor(janitorCtx, st, svc, logger, cfg.TaskRetention, cfg.EventRetention)

	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- apiHTTP.ListenAndServe() }()
	logger.Printf("opticore listening cwmp=%s api=%s", cfg.CWMPAddr, cfg.APIAddr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.CWMPSessionTimeout)
	defer cancel()
	logger.Printf("shutting down")
	apiSrv.Close()
	apiErr := apiHTTP.Shutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return apiErr
}

func health(w http.ResponseWriter, r *http.Request, st *store.Store) {
	out := map[string]any{"db": "ok"}
	code := http.StatusOK
	if err := st.Ping(r.Context()); err != nil {
		out["db"], code = err.Error(), http.StatusServiceUnavailable
	} else if t, err := st.NewestInform(r.Context()); err == nil && !t.IsZero() {
		out["newest_inform_age_s"] = int(time.Since(t).Seconds())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(out)
}

func janitor(ctx context.Context, st *store.Store, svc *acs.Service, logger *log.Logger, taskRetention, eventRetention time.Duration) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n, err := st.ExpireTasks(ctx); err != nil {
				logger.Printf("ERROR janitor: expire tasks err=%v", err)
			} else if n > 0 {
				logger.Printf("janitor: expired tasks count=%d", n)
			}
			if err := svc.PumpJobs(ctx); err != nil {
				logger.Printf("ERROR janitor: pump jobs err=%v", err)
			}
			if now.Sub(lastPurge) < time.Hour {
				continue
			}
			lastPurge = now
			if nt, ne, err := st.PurgeOld(ctx, taskRetention, eventRetention); err != nil {
				logger.Printf("ERROR janitor: purge err=%v", err)
			} else if nt+ne > 0 {
				logger.Printf("janitor: purged old rows tasks=%d events=%d", nt, ne)
			}
		}
	}
}

// cmdTUI runs the operator console. Its one write, a wake, goes through
// acs.Service so credential self-heal and validation stay in one place.
func cmdTUI(ctx context.Context) error {
	return withStore(ctx, func(st *store.Store, cfg config.Config) error {
		svc := service(st, cfg)
		return tui.Run(ctx, tui.Deps{
			Devices: st, Tasks: st, Jobs: st, Events: st,
			Params:  tuiParams{st: st},
			Wake:    svc.Wake,
			Enqueue: svc.Enqueue,
			Serving: st.DaemonRunning,
		})
	})
}

// tuiParams adapts the cached parameter map: the store has no prefix query.
type tuiParams struct{ st *store.Store }

func (p tuiParams) Params(ctx context.Context, idOrSerial, prefix string) (map[string]store.Param, error) {
	d, err := p.st.GetDevice(ctx, idOrSerial)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.Param, len(d.Params))
	for path, v := range d.Params {
		if strings.HasPrefix(path, prefix) {
			out[path] = v
		}
	}
	return out, nil
}

// isTerminal reports whether both stdin and stdout are a terminal, which the
// console needs; a pipe gets the usage text instead.
func isTerminal() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}

// cmdMigrate creates the database if it is missing, then applies migrations.
func cmdMigrate(ctx context.Context) error {
	cfg, err := config.LoadClient(os.Getenv)
	if err != nil {
		return err
	}
	if err := ensureDatabase(ctx, cfg.DBDSN, log.Default()); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Migrate(ctx)
}

func ensureDatabase(ctx context.Context, dsn string, logger *log.Logger) error {
	created, err := store.EnsureDatabase(ctx, dsn)
	if err != nil {
		return err
	}
	if created {
		logger.Printf("database created")
	}
	return nil
}

func withStore(ctx context.Context, fn func(*store.Store, config.Config) error) error {
	cfg, err := config.LoadClient(os.Getenv)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(st, cfg)
}

func service(st *store.Store, cfg config.Config) *acs.Service {
	logger := log.Default()
	return &acs.Service{Store: st,
		Waker: wake.New(cfg.CWMPCRTimeout, logger), Logger: logger}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// split separates flags from positional args, so flags may follow them.
func split(args []string) (flags, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && a != "--json" && a != "-json" && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

func cmdDevices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	serial := fs.String("serial", "", "serial prefix")
	class := fs.String("class", "", "product class")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		ds, err := st.ListDevices(ctx, store.ListFilter{SerialPrefix: *serial, ProductClass: *class, Limit: 1000})
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(ds)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSERIAL\tCLASS\tVERSION\tLAST INFORM\tADDR\tCR")
		for _, d := range ds {
			cr := "no"
			if d.CRSetAt != nil {
				cr = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s ago\t%s\t%s\n", d.ID, d.Serial, d.ProductClass,
				d.SoftwareVersion, time.Since(d.LastInform).Round(time.Second), d.LastAddr, cr)
		}
		return w.Flush()
	})
}

func cmdDevice(ctx context.Context, args []string) error {
	fl, pos := split(args)
	fs := flag.NewFlagSet("device", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	prefix := fs.String("prefix", "", "only parameters under this path")
	if err := fs.Parse(fl); err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: device <serial|id> [--json] [--prefix P]")
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		d, err := st.GetDevice(ctx, pos[0])
		if err != nil {
			return err
		}
		for k := range d.Params {
			if !strings.HasPrefix(k, *prefix) {
				delete(d.Params, k)
			}
		}
		if *asJSON {
			return printJSON(d)
		}
		fmt.Printf("id: %s\nserial: %s\noui: %s\nclass: %s\nversion: %s\nlast inform: %s (%s ago)\naddr: %s\ncr url: %s\ncr set: %v\npending reboot: %v\n",
			d.ID, d.Serial, d.OUI, d.ProductClass, d.SoftwareVersion, d.LastInform.Format(time.RFC3339),
			time.Since(d.LastInform).Round(time.Second), d.LastAddr, d.CRURL, d.CRSetAt != nil, d.PendingReboot)
		if len(d.LastFault) > 0 {
			fmt.Printf("last fault: %s\n", d.LastFault)
		}
		keys := make([]string, 0, len(d.Params))
		for k := range d.Params {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fmt.Println("params:")
		for _, k := range keys {
			p := d.Params[k]
			v := p.Value
			fmt.Printf("  %s = %s (%s)\n", k, v, p.Type)
		}
		return nil
	})
}

func cmdTasks(ctx context.Context, args []string) error {
	fl, pos := split(args)
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(fl); err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: tasks <serial|id> [--json]")
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		ts, err := st.ListTasks(ctx, pos[0], 100)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(ts)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTATUS\tATTEMPTS\tCREATED\tFAULT\tRESULT")
		for _, t := range ts {
			fault := t.Fault
			if t.FaultCode != nil {
				fault = fmt.Sprintf("%d %s", *t.FaultCode, t.Fault)
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\t%s\n", t.ID, t.Name, t.Status, t.Attempts,
				t.CreatedAt.Local().Format("01-02 15:04:05"), fault, t.Result)
		}
		return w.Flush()
	})
}

func cmdTask(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: task <serial|id> get [path]... | set <path> <value> [type] | reboot | ping <host> | revert <taskId>")
	}
	dev, name, rest := args[0], args[1], args[2:]
	if name == "revert" {
		if len(rest) != 1 {
			return errors.New("usage: task <serial|id> revert <taskId>")
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			return errors.New("task id must be an integer")
		}
		return withStore(ctx, func(st *store.Store, cfg config.Config) error {
			svc := service(st, cfg)
			out, err := svc.RevertTask(ctx, dev, id, nil)
			if err != nil {
				return err
			}
			return queued(ctx, svc, out)
		})
	}
	var t store.NewTask
	switch name {
	case "get":
		// No paths reads every parameter under the device's root.
		t = store.NewTask{Name: "get", Args: mustJSON(acs.GetArgs{Paths: rest})}
	case "set":
		if len(rest) < 2 || len(rest) > 3 {
			return errors.New("usage: task <serial|id> set <path> <value> [type]")
		}
		t = store.NewTask{Name: "set", Args: mustJSON(acs.SetArgs{Values: [][]string{rest}})}
	case "reboot":
		t = store.NewTask{Name: "reboot"}
	case "ping":
		if len(rest) != 1 {
			return errors.New("usage: task <serial|id> ping <host>")
		}
		t = store.NewTask{Name: "ping", Args: mustJSON(acs.PingArgs{Host: rest[0]})}
	default:
		return fmt.Errorf("unknown task %q", name)
	}
	return withStore(ctx, func(st *store.Store, cfg config.Config) error {
		svc := service(st, cfg)
		out, err := svc.Enqueue(ctx, dev, []store.NewTask{t})
		if err != nil {
			return err
		}
		return queued(ctx, svc, out)
	})
}

// queued prints the queued tasks and wakes the device synchronously:
// Enqueue's coalesced wake fires after the CLI process has already exited.
func queued(ctx context.Context, svc *acs.Service, out []store.Task) error {
	for _, t := range out {
		fmt.Printf("queued task %d (%s) for %s\n", t.ID, t.Name, t.DeviceID)
	}
	if len(out) == 0 {
		return nil
	}
	if err := svc.Wake(ctx, out[0].DeviceID); err != nil {
		fmt.Fprintf(os.Stderr, "wake failed, the task runs at the next inform: %v\n", err)
	}
	return nil
}

func cmdJobs(ctx context.Context, args []string) error {
	fl, _ := split(args)
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(fl); err != nil {
		return err
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		jobs, err := st.ListJobs(ctx, 100)
		if err != nil {
			return err
		}
		rows := make([]jobRow, 0, len(jobs))
		for _, j := range jobs {
			counts, err := st.JobCounts(ctx, j.ID)
			if err != nil {
				return err
			}
			rows = append(rows, newJobRow(j, counts, nil))
		}
		if *asJSON {
			return printJSON(rows)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTATUS\tDONE\tFAILED\tRUNNING\tPENDING\tTOTAL\tCREATED")
		for _, r := range rows {
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n", r.ID, r.Name, r.Status,
				r.Counts["done"], r.Counts["failed"], r.Counts["running"], r.Counts["pending"],
				r.Counts["total"], r.CreatedAt.Local().Format("01-02 15:04:05"))
		}
		return w.Flush()
	})
}

type jobRow struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	MaxParallel int                `json:"max_parallel"`
	CreatedAt   time.Time          `json:"created_at"`
	Counts      map[string]int     `json:"counts"`
	Failures    []store.JobFailure `json:"failures,omitempty"`
}

func newJobRow(j store.Job, counts map[string]int, failures []store.JobFailure) jobRow {
	total := 0
	for _, n := range counts {
		total += n
	}
	counts["total"] = total
	return jobRow{
		ID: j.ID, Name: j.Name, Status: j.Status, MaxParallel: j.MaxParallel,
		CreatedAt: j.CreatedAt, Counts: counts, Failures: failures,
	}
}

func cmdJob(ctx context.Context, args []string) error {
	fl, pos := split(args)
	fs := flag.NewFlagSet("job", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(fl); err != nil {
		return err
	}
	if len(pos) == 2 && pos[0] == "cancel" {
		id, err := strconv.ParseInt(pos[1], 10, 64)
		if err != nil {
			return errors.New("job id must be an integer")
		}
		return withStore(ctx, func(st *store.Store, _ config.Config) error {
			if err := st.CancelJob(ctx, id); err != nil {
				return err
			}
			fmt.Printf("job %d cancelled\n", id)
			return nil
		})
	}
	if len(pos) == 2 && pos[0] == "revert" {
		id, err := strconv.ParseInt(pos[1], 10, 64)
		if err != nil {
			return errors.New("job id must be an integer")
		}
		return withStore(ctx, func(st *store.Store, cfg config.Config) error {
			svc := service(st, cfg)
			job, tasks, err := svc.RevertJob(ctx, id)
			if err != nil {
				return err
			}
			if err := svc.PumpJobs(ctx); err != nil {
				return err
			}
			fmt.Printf("job %d reverts job %d with %d task(s)\n", job.ID, id, len(tasks))
			return nil
		})
	}
	if len(pos) != 1 {
		return errors.New("usage: job <id> [--json] | job cancel <id> | job revert <id>")
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return errors.New("job id must be an integer")
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		job, err := st.GetJob(ctx, id)
		if err != nil {
			return err
		}
		counts, err := st.JobCounts(ctx, id)
		if err != nil {
			return err
		}
		failures, err := st.JobFailures(ctx, id)
		if err != nil {
			return err
		}
		row := newJobRow(job, counts, failures)
		if *asJSON {
			return printJSON(row)
		}
		fmt.Printf("id: %d\nname: %s\nstatus: %s\nmax parallel: %d\ncreated: %s\n",
			row.ID, row.Name, row.Status, row.MaxParallel, row.CreatedAt.Format(time.RFC3339))
		statuses := make([]string, 0, len(row.Counts))
		for s := range row.Counts {
			if s != "total" {
				statuses = append(statuses, s)
			}
		}
		slices.Sort(statuses)
		fmt.Println("progress:")
		for _, s := range statuses {
			fmt.Printf("  %s: %d\n", s, row.Counts[s])
		}
		if len(row.Failures) > 0 {
			fmt.Println("failures:")
			for _, f := range row.Failures {
				code := "-"
				if f.FaultCode != nil {
					code = strconv.Itoa(*f.FaultCode)
				}
				fmt.Printf("  %s task %d: %s %s\n", f.DeviceID, f.TaskID, code, f.Fault)
			}
		}
		return nil
	})
}

// cmdSummon queues a root get and wakes the device: the GenieACS Summon, which
// is a connection request followed by a full-tree refresh. Queueing the task
// already sends the request, so there is no separate wake.
func cmdSummon(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: summon <serial|id>")
	}
	return withStore(ctx, func(st *store.Store, cfg config.Config) error {
		svc := service(st, cfg)
		out, err := svc.Enqueue(ctx, args[0], []store.NewTask{{Name: "get"}})
		if err != nil {
			return err
		}
		return queued(ctx, svc, out)
	})
}

func cmdWake(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wake <serial|id>")
	}
	return withStore(ctx, func(st *store.Store, cfg config.Config) error {
		if err := service(st, cfg).Wake(ctx, args[0]); err != nil {
			return err
		}
		fmt.Println("connection request accepted")
		return nil
	})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
